package transform

import (
	"html"
	"mime"
	"net/url"
	"os"
	"path"
	"strings"

	"maunium.net/go/mautrix/event"

	"slack2matrix/internal/convert"
	"slack2matrix/internal/slackexport"
)

// attachments renders Slack attachments as quoted cards, plus media drafts
// for card images that should be re-hosted.
func (t *Transformer) attachments(m *slackexport.Message) ([]convert.Text, []*Draft) {
	var cards []convert.Text
	var media []*Draft
	for i := range m.Attachments {
		a := &m.Attachments[i]
		unfurl := a.IsUnfurl() && !a.QuotesMessage()
		if unfurl && !t.Opts.UnfurlCards {
			continue
		}
		card := t.card(a)
		if !card.IsEmpty() {
			cards = append(cards, card)
		}
		// Images from unfurls are left to the link; images that are the
		// point of a bot card (giphy, xkcd, image posts) are re-hosted.
		if !unfurl && t.Opts.FetchCardImages && a.ImageURL != "" {
			media = append(media, remoteImageDraft(a.ImageURL, a.Title, a.ImageWidth, a.ImageHeight, a.ImageBytes))
		}
		for j := range a.Files {
			if d := t.fileDraft(&a.Files[j]); d != nil {
				media = append(media, d)
			}
		}
	}
	return cards, media
}

// linkPreviews converts Slack's link unfurls into MSC4095 bundled previews.
// Images are left out: Slack only recorded the original image URLs, and
// fetching them is out of scope.
func (t *Transformer) linkPreviews(m *slackexport.Message) []*event.BeeperLinkPreview {
	var out []*event.BeeperLinkPreview
	for i := range m.Attachments {
		a := &m.Attachments[i]
		if !a.IsUnfurl() || a.QuotesMessage() {
			continue
		}
		matched := firstNonEmpty(a.FromURL, a.OriginalURL)
		p := &event.BeeperLinkPreview{
			MatchedURL: matched,
			LinkPreview: event.LinkPreview{
				CanonicalURL: firstNonEmpty(a.TitleLink, matched),
				Title:        a.Title,
				SiteName:     a.ServiceName,
			},
		}
		if a.Text != "" {
			p.Description = t.Conv.Mrkdwn(a.Text).Plain
		}
		if p.Title == "" && p.Description == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// card renders one attachment as a blockquote.
func (t *Transformer) card(a *slackexport.Attachment) convert.Text {
	var plain []string
	var htm []string
	add := func(tx convert.Text) {
		if tx.IsEmpty() {
			return
		}
		plain = append(plain, tx.Plain)
		htm = append(htm, tx.HTML)
	}
	if a.Pretext != "" {
		add(t.Conv.Mrkdwn(a.Pretext))
	}
	author := firstNonEmpty(a.AuthorName, a.AuthorSub)
	if a.QuotesMessage() && a.AuthorID != "" {
		if ident := t.Dir.User(a.AuthorID); ident != nil && ident.DisplayName != "" {
			author = ident.DisplayName
		}
	}
	if author != "" {
		add(linked(author, t.Conv.MapURL(a.AuthorLink), true))
	}
	if a.Title != "" {
		add(linked(a.Title, t.Conv.MapURL(a.TitleLink), true))
	}
	if a.Text != "" {
		add(t.Conv.Mrkdwn(a.Text))
	} else if len(a.Blocks) == 0 && a.Title == "" && author == "" && a.Fallback != "" && a.ImageURL == "" {
		add(t.Conv.Mrkdwn(a.Fallback))
	}
	for _, f := range a.Fields {
		if f.Title == "" && f.Value == "" {
			continue
		}
		v := t.Conv.Mrkdwn(f.Value)
		add(convert.Text{
			Plain: f.Title + ": " + v.Plain,
			HTML:  "<strong>" + html.EscapeString(f.Title) + "</strong>: " + v.HTML,
		})
	}
	footer := firstNonEmpty(a.Footer, a.ServiceName)
	// A quoted Slack message links to the imported original. Links that
	// can't be mapped would only lead back to Slack, so they're dropped.
	var permalink string
	if from := html.UnescapeString(a.FromURL); from != "" {
		if mapped := t.Conv.MapURL(from); mapped != from {
			permalink = mapped
			if footer == "" {
				footer = "View message"
			}
		}
	}
	if footer != "" {
		f := t.Conv.Mrkdwn(footer)
		if permalink != "" {
			f = linked(f.Plain, permalink, false)
		}
		add(convert.Text{Plain: f.Plain, HTML: "<sub>" + f.HTML + "</sub>"})
	}
	if len(plain) == 0 {
		return convert.Text{}
	}
	p := strings.Join(plain, "\n")
	lines := strings.Split(p, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return convert.Text{
		Plain: strings.Join(lines, "\n"),
		HTML:  "<blockquote>" + strings.Join(htm, "<br>") + "</blockquote>",
	}
}

func linked(label, href string, bold bool) convert.Text {
	inner := html.EscapeString(label)
	if bold {
		inner = "<strong>" + inner + "</strong>"
	}
	if href == "" {
		return convert.Text{Plain: label, HTML: inner}
	}
	return convert.Text{
		Plain: label + " (" + href + ")",
		HTML:  `<a href="` + html.EscapeString(href) + `">` + inner + `</a>`,
	}
}

// imageBlocks re-hosts images posted as Block Kit image blocks (e.g. /giphy).
func (t *Transformer) imageBlocks(blocks []slackexport.Block) []*Draft {
	if !t.Opts.FetchCardImages {
		return nil
	}
	var out []*Draft
	for _, b := range blocks {
		if b.Type == "image" && b.ImageURL != "" {
			title := b.AltText
			if b.Title != nil && b.Title.Text != "" {
				title = b.Title.Text
			}
			out = append(out, remoteImageDraft(b.ImageURL, title, b.ImageW, b.ImageH, b.ImageSize))
		}
	}
	return out
}

func remoteImageDraft(rawURL, title string, w, h int, size int64) *Draft {
	name := "image"
	mimetype := ""
	if u, err := url.Parse(rawURL); err == nil {
		if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
			name = base
		}
		mimetype = mime.TypeByExtension(strings.ToLower(path.Ext(u.Path)))
	}
	if mimetype == "" {
		mimetype = "image/jpeg"
	}
	body := name
	if title != "" {
		body = title
	}
	return &Draft{
		Kind: KindMedia,
		Media: &Media{
			RemoteURL: rawURL,
			Name:      name,
			Mimetype:  mimetype,
			Size:      size,
			Width:     w,
			Height:    h,
		},
		Content: &event.MessageEventContent{
			MsgType:  event.MsgImage,
			Body:     body,
			FileName: name,
		},
	}
}

// readSmallFile reads a file of at most limit bytes as text.
func readSmallFile(p string, limit int64) (string, error) {
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if st.Size() > limit {
		return "", os.ErrInvalid
	}
	data, err := os.ReadFile(p)
	return string(data), err
}
