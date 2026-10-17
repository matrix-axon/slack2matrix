// Package transform decides which Matrix events each Slack message becomes.
// It is pure: media is referenced, not uploaded, and relations to other
// events are expressed in Slack terms for the importer to resolve.
package transform

import (
	"encoding/json"
	"fmt"
	"html"
	"mime"
	"path/filepath"
	"strings"

	"maunium.net/go/mautrix/event"

	"slack2matrix/internal/convert"
	"slack2matrix/internal/directory"
	"slack2matrix/internal/emoji"
	"slack2matrix/internal/slackexport"
)

type Kind string

const (
	KindMessage    Kind = "message"    // text, notice or emote
	KindMedia      Kind = "media"      // file, image, video or audio needing upload
	KindReaction   Kind = "reaction"   // annotation on the message's anchor event
	KindMembership Kind = "membership" // join or leave state change
)

// Draft is one Matrix event to be sent for a Slack message.
type Draft struct {
	// Part is the event's stable index within its Slack message.
	Part   int
	Kind   Kind
	Sender *directory.Identity
	// TS is origin_server_ts in milliseconds.
	TS int64
	// Content is the message content; for media the URL/file fields are
	// filled in after upload.
	Content *event.MessageEventContent
	Media   *Media
	// ReactionKey is the annotation key for KindReaction.
	ReactionKey string
	// Membership is the new membership for KindMembership.
	Membership event.Membership
	// ReplyToFile makes the event a reply to the event that shared this Slack
	// file (legacy file comments).
	ReplyToFile string
	// FileID is set on media drafts for Slack-hosted files so later file
	// comments can find the event.
	FileID string
}

// Media references content to upload.
type Media struct {
	// LocalPath is set for files from the export's media mirror.
	LocalPath string
	// RemoteURL is set for images referenced by cards, fetched at import time.
	RemoteURL string
	Name      string
	Mimetype  string
	Size      int64
	Width     int
	Height    int
	// Rotation is Slack's EXIF rotation hint for images.
	Rotation   int
	DurationMS int64
	// SlackFileID identifies hosted files for upload caching.
	SlackFileID string
}

// Options control optional behaviour.
type Options struct {
	// UnfurlCards renders link previews as quoted cards instead of skipping them.
	UnfurlCards bool
	// LinkPreviews embeds Slack's link previews in the message as MSC4095
	// bundled URL previews (com.beeper.linkpreviews), without images.
	LinkPreviews bool
	// FetchCardImages re-hosts images referenced by bot/app cards.
	FetchCardImages bool
	// MarkEdited appends "(edited)" to edited messages.
	MarkEdited bool
	// SnippetInlineLimit is the largest snippet inlined as a code block.
	SnippetInlineLimit int64
}

// Transformer converts Slack messages into drafts.
type Transformer struct {
	Conv *convert.Converter
	Dir  *directory.Directory
	Exp  *slackexport.Export
	Opts Options
}

// Message returns the drafts for one Slack message, in send order. A nil
// result means the message produces no events.
func (t *Transformer) Message(m *slackexport.Message) []*Draft {
	if m.Hidden {
		return nil
	}
	ts := m.TS.Millis()
	sender := t.Dir.Sender(m)
	b := &builder{ts: ts, sender: sender}

	switch m.Subtype {
	case "channel_join":
		b.add(&Draft{Kind: KindMembership, Membership: event.MembershipJoin})
		return b.drafts
	case "channel_leave":
		b.add(&Draft{Kind: KindMembership, Membership: event.MembershipLeave})
		return b.drafts
	case "file_comment":
		return t.fileComment(m, b)
	}

	msgType := event.MsgText
	switch {
	case m.Subtype == "me_message":
		msgType = event.MsgEmote
	case sender.Bot, isNoticeSubtype(m.Subtype):
		msgType = event.MsgNotice
	}

	text := t.Conv.Message(m.Text, m.Blocks)
	cards, cardMedia := t.attachments(m)
	var previews []*event.BeeperLinkPreview
	if t.Opts.LinkPreviews {
		previews = t.linkPreviews(m)
	}
	for _, c := range cards {
		text.Append(c)
	}
	for _, f := range m.Files {
		if extra, ok := t.inlineFile(&f); ok {
			text.Append(extra)
		}
	}
	if t.Opts.MarkEdited && m.Edited != nil && !text.IsEmpty() {
		text.Plain += " (edited)"
		text.HTML += " <sub>(edited)</sub>"
	}

	var media []*Draft
	for i := range m.Files {
		if d := t.fileDraft(&m.Files[i]); d != nil {
			media = append(media, d)
		}
	}
	media = append(media, cardMedia...)
	media = append(media, t.imageBlocks(m.Blocks)...)

	// A single file with text is sent as one captioned media event.
	if len(media) == 1 && !text.IsEmpty() && media[0].Media.LocalPath != "" {
		caption := text.Content(msgType)
		media[0].Content.Body = caption.Body
		media[0].Content.Format = caption.Format
		media[0].Content.FormattedBody = caption.FormattedBody
		media[0].Content.Mentions = caption.Mentions
		media[0].Content.BeeperLinkPreviews = previews
		b.add(media[0])
	} else {
		if !text.IsEmpty() {
			c := text.Content(msgType)
			c.BeeperLinkPreviews = previews
			b.add(&Draft{Kind: KindMessage, Content: c})
		}
		for _, d := range media {
			b.add(d)
		}
	}
	if len(b.drafts) == 0 {
		return nil
	}
	// Different Slack names can map to the same emoji (":+1:" and
	// ":thumbsup:"), and Matrix allows one identical reaction per user.
	seen := map[string]bool{}
	for _, r := range m.Reactions {
		key := emoji.Render(r.Name, 0)
		for _, u := range r.Users {
			sender := t.Dir.User(u)
			if seen[string(sender.MXID)+"\x00"+key] {
				continue
			}
			seen[string(sender.MXID)+"\x00"+key] = true
			b.add(&Draft{Kind: KindReaction, ReactionKey: key, Sender: sender})
		}
	}
	return b.drafts
}

// isNoticeSubtype lists system-generated messages sent as m.notice.
func isNoticeSubtype(st string) bool {
	switch st {
	case "channel_archive", "channel_unarchive", "channel_topic", "channel_purpose", "channel_name",
		"bot_add", "bot_remove", "reminder_add", "pinned_item", "unpinned_item", "sh_room_created",
		"tombstone", "slackbot_response":
		return true
	}
	return false
}

type builder struct {
	ts     int64
	sender *directory.Identity
	drafts []*Draft
}

func (b *builder) add(d *Draft) {
	d.Part = len(b.drafts)
	if d.TS == 0 {
		d.TS = b.ts
	}
	if d.Sender == nil {
		d.Sender = b.sender
	}
	b.drafts = append(b.drafts, d)
}

// fileComment converts a legacy file comment into a reply to the file's event.
func (t *Transformer) fileComment(m *slackexport.Message, b *builder) []*Draft {
	if m.Comment == nil {
		return nil
	}
	if m.Comment.User != "" {
		b.sender = t.Dir.User(m.Comment.User)
	}
	text := t.Conv.Mrkdwn(m.Comment.Comment)
	if text.IsEmpty() {
		return nil
	}
	d := &Draft{Kind: KindMessage, Content: text.Content(event.MsgText)}
	if m.File != nil {
		d.ReplyToFile = m.File.ID
	}
	b.add(d)
	return b.drafts
}

// inlineFile renders files that become text rather than uploads: external
// links, deleted files, and small text snippets.
func (t *Transformer) inlineFile(f *slackexport.File) (convert.Text, bool) {
	name := firstNonEmpty(f.Title, f.Name, f.ID)
	switch {
	case f.Mode == "tombstone":
		return plainText("[file deleted]"), true
	case f.Mode == "external" || f.IsExternal:
		link := firstNonEmpty(f.URLPrivate, f.Permalink)
		return convert.Text{
			Plain: "📎 " + name + " (" + link + ")",
			HTML:  `📎 <a href="` + html.EscapeString(link) + `">` + html.EscapeString(name) + `</a>`,
		}, true
	case f.Mode == "snippet" && f.Size <= t.Opts.SnippetInlineLimit:
		p := t.Exp.FilePath(f)
		if p == "" {
			return convert.Text{}, false
		}
		code, err := readSmallFile(p, t.Opts.SnippetInlineLimit)
		if err != nil {
			return convert.Text{}, false
		}
		code = strings.TrimRight(code, "\n")
		lang := ""
		if f.Filetype != "" && f.Filetype != "text" {
			lang = ` class="language-` + html.EscapeString(f.Filetype) + `"`
		}
		return convert.Text{
			Plain: name + ":\n```\n" + code + "\n```",
			HTML:  "<strong>" + html.EscapeString(name) + "</strong><pre><code" + lang + ">" + html.EscapeString(code) + "</code></pre>",
		}, true
	case f.Mode == "hosted" || f.Mode == "snippet" || f.Mode == "docs" || f.Mode == "":
		if f.DownloadURL() != "" && t.Exp.FilePath(f) == "" {
			return plainText("[file unavailable: " + name + "]"), true
		}
	}
	return convert.Text{}, false
}

// fileDraft creates a media draft for an uploadable file, or nil.
func (t *Transformer) fileDraft(f *slackexport.File) *Draft {
	if f.Mode == "tombstone" || f.Mode == "external" || f.IsExternal {
		return nil
	}
	if f.Mode == "snippet" && f.Size <= t.Opts.SnippetInlineLimit {
		return nil
	}
	p := t.Exp.FilePath(f)
	if p == "" {
		return nil
	}
	name := firstNonEmpty(f.Name, f.Title, f.ID)
	mimetype := f.Mimetype
	if mimetype == "" || mimetype == "application/octet-stream" {
		if guessed := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); guessed != "" {
			mimetype = guessed
		}
	}
	m := &Media{
		LocalPath:   p,
		Name:        name,
		Mimetype:    mimetype,
		Size:        f.Size,
		Width:       f.OriginalW,
		Height:      f.OriginalH,
		Rotation:    f.ImageExifRotation,
		DurationMS:  f.DurationMS,
		SlackFileID: f.ID,
	}
	body := name
	if f.Title != "" && f.Title != f.Name && !looksLikeFilename(f.Title) {
		body = f.Title
	}
	return &Draft{
		Kind:   KindMedia,
		Media:  m,
		FileID: f.ID,
		Content: &event.MessageEventContent{
			MsgType:  MsgTypeFor(mimetype),
			Body:     body,
			FileName: name,
		},
	}
}

// MsgTypeFor picks the Matrix msgtype for a MIME type.
func MsgTypeFor(mimetype string) event.MessageType {
	switch {
	case strings.HasPrefix(mimetype, "image/"):
		return event.MsgImage
	case strings.HasPrefix(mimetype, "video/"):
		return event.MsgVideo
	case strings.HasPrefix(mimetype, "audio/"):
		return event.MsgAudio
	}
	return event.MsgFile
}

func looksLikeFilename(s string) bool {
	ext := filepath.Ext(s)
	return ext != "" && len(ext) <= 5 && !strings.Contains(s, " ")
}

func plainText(s string) convert.Text {
	return convert.Text{Plain: s, HTML: html.EscapeString(s)}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// String summarizes a draft for the render command.
func (d *Draft) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "part %d %s as %s ts=%d", d.Part, d.Kind, d.Sender.MXID, d.TS)
	switch d.Kind {
	case KindReaction:
		fmt.Fprintf(&sb, " key=%q", d.ReactionKey)
	case KindMembership:
		fmt.Fprintf(&sb, " membership=%s", d.Membership)
	}
	if d.ReplyToFile != "" {
		fmt.Fprintf(&sb, " reply-to-file=%s", d.ReplyToFile)
	}
	if d.Media != nil {
		src := d.Media.LocalPath
		if src == "" {
			src = d.Media.RemoteURL
		}
		fmt.Fprintf(&sb, "\n    media: %s (%s, %d bytes, %dx%d) from %s", d.Media.Name, d.Media.Mimetype, d.Media.Size, d.Media.Width, d.Media.Height, src)
	}
	if d.Content != nil {
		fmt.Fprintf(&sb, "\n    msgtype: %s\n    body: %q", d.Content.MsgType, d.Content.Body)
		if d.Content.FormattedBody != "" {
			fmt.Fprintf(&sb, "\n    html: %q", d.Content.FormattedBody)
		}
		if len(d.Content.BeeperLinkPreviews) > 0 {
			previews, _ := json.Marshal(d.Content.BeeperLinkPreviews)
			fmt.Fprintf(&sb, "\n    com.beeper.linkpreviews: %s", previews)
		}
		if d.Content.Mentions != nil && len(d.Content.Mentions.UserIDs) > 0 {
			fmt.Fprintf(&sb, "\n    mentions: %v", d.Content.Mentions.UserIDs)
		}
	}
	return sb.String()
}
