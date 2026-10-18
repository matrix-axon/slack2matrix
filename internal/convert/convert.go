// Package convert turns Slack message text (rich_text blocks or mrkdwn) into
// Matrix plain-text bodies and HTML formatted bodies.
package convert

import (
	"html"
	"net/url"
	"strings"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"slack2matrix/internal/directory"
)

// Resolver supplies the Matrix side of mentions and channel links.
type Resolver interface {
	// User returns the identity for a Slack user ID, or nil if unknown.
	User(slackID string) *directory.Identity
	// FindUser returns the identity of a known Slack user by ID or username,
	// or nil; unlike User it never invents a ghost.
	FindUser(ref string) *directory.Identity
	// Channel returns a channel's name and its Matrix room (empty if not imported).
	Channel(slackID string) (name string, room id.RoomID)
	// Message returns the Matrix event a Slack message was imported as, or
	// "" if it hasn't been (yet).
	Message(channelID, ts string) id.EventID
}

// Converter renders Slack text using a Resolver for mentions.
type Converter struct {
	Resolver   Resolver
	ServerName string
}

// Text is converted message text.
type Text struct {
	Plain    string
	HTML     string
	Mentions []id.UserID
}

// IsEmpty reports whether there is no visible text.
func (t *Text) IsEmpty() bool {
	return strings.TrimSpace(t.Plain) == ""
}

// HasFormatting reports whether the HTML carries anything beyond the plain
// text, i.e. whether formatted_body is worth sending.
func (t *Text) HasFormatting() bool {
	return t.HTML != plainToHTML(t.Plain)
}

// Append adds another chunk of text on a new line.
func (t *Text) Append(o Text) {
	if o.IsEmpty() {
		return
	}
	if t.IsEmpty() {
		*t = o
		return
	}
	t.Plain += "\n" + o.Plain
	t.HTML += "<br>" + o.HTML
	t.Mentions = appendUnique(t.Mentions, o.Mentions...)
}

// Content builds a message event content of the given msgtype.
func (t *Text) Content(msgType event.MessageType) *event.MessageEventContent {
	c := &event.MessageEventContent{
		MsgType:  msgType,
		Body:     t.Plain,
		Mentions: &event.Mentions{UserIDs: t.Mentions},
	}
	if t.HasFormatting() {
		c.Format = event.FormatHTML
		c.FormattedBody = t.HTML
	}
	return c
}

func plainToHTML(s string) string {
	return strings.ReplaceAll(html.EscapeString(s), "\n", "<br>")
}

func appendUnique(list []id.UserID, items ...id.UserID) []id.UserID {
outer:
	for _, it := range items {
		for _, existing := range list {
			if existing == it {
				continue outer
			}
		}
		list = append(list, it)
	}
	return list
}

// writer accumulates plain and HTML output in parallel.
type writer struct {
	plain    strings.Builder
	html     strings.Builder
	mentions []id.UserID
}

// text writes literal text; in HTML it is escaped and newlines become <br>
// unless inside preformatted content.
func (w *writer) text(s string, pre bool) {
	w.plain.WriteString(s)
	esc := html.EscapeString(s)
	if !pre {
		esc = strings.ReplaceAll(esc, "\n", "<br>")
	}
	w.html.WriteString(esc)
}

// raw writes different plain and HTML representations.
func (w *writer) raw(plain, htm string) {
	w.plain.WriteString(plain)
	w.html.WriteString(htm)
}

func (w *writer) result() Text {
	return Text{Plain: w.plain.String(), HTML: w.html.String(), Mentions: w.mentions}
}

// userMention writes a pill for a Slack user. In preformatted text only the
// name is written.
func (c *Converter) userMention(w *writer, slackID, label string, pre bool) {
	ident := c.Resolver.User(slackID)
	name := label
	if ident != nil && ident.DisplayName != "" {
		name = ident.DisplayName
	}
	if name == "" {
		name = slackID
	}
	if ident == nil || pre {
		w.text("@"+name, pre)
		return
	}
	w.raw(name, `<a href="https://matrix.to/#/`+url.PathEscape(string(ident.MXID))+`">`+html.EscapeString(name)+`</a>`)
	w.mentions = appendUnique(w.mentions, ident.MXID)
}

func (c *Converter) channelMention(w *writer, slackID, label string, pre bool) {
	name, room := c.Resolver.Channel(slackID)
	if name == "" {
		name = label
	}
	if name == "" {
		name = slackID
	}
	if room == "" || pre {
		w.text("#"+name, pre)
		return
	}
	href := "https://matrix.to/#/" + url.PathEscape(string(room)) + "?via=" + url.QueryEscape(c.ServerName)
	w.raw("#"+name, `<a href="`+html.EscapeString(href)+`">#`+html.EscapeString(name)+`</a>`)
}

func (c *Converter) link(w *writer, href, label string, pre bool) {
	if mapped := c.MapURL(href); mapped != href {
		if label == href {
			label = mapped
		}
		href = mapped
	}
	if label == "" || label == href || strings.TrimPrefix(href, "mailto:") == label {
		if label == "" {
			label = strings.TrimPrefix(href, "mailto:")
		}
		if pre || !safeScheme(href) {
			w.text(label, pre)
			return
		}
		w.raw(label, `<a href="`+html.EscapeString(href)+`">`+html.EscapeString(label)+`</a>`)
		return
	}
	if pre || !safeScheme(href) {
		w.text(label+" ("+href+")", pre)
		return
	}
	w.raw(label+" ("+href+")", `<a href="`+html.EscapeString(href)+`">`+html.EscapeString(label)+`</a>`)
}

// MapURL rewrites links into the Slack workspace so they keep working after
// the migration: message and channel links become matrix.to links to the
// imported event or room, and profile links point to the Matrix user. Other
// URLs, and Slack links whose target wasn't imported, are returned unchanged.
func (c *Converter) MapURL(href string) string {
	u, err := url.Parse(href)
	if err != nil || !strings.HasSuffix(strings.ToLower(u.Host), ".slack.com") {
		return href
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case len(parts) >= 2 && parts[0] == "archives":
		_, room := c.Resolver.Channel(parts[1])
		if room == "" {
			return href
		}
		target := url.PathEscape(string(room))
		if len(parts) >= 3 {
			if ts, ok := PermalinkTS(parts[2]); ok {
				if evt := c.Resolver.Message(parts[1], ts); evt != "" {
					target += "/" + url.PathEscape(string(evt))
				}
			}
		}
		return "https://matrix.to/#/" + target + "?via=" + url.QueryEscape(c.ServerName)
	case len(parts) == 2 && parts[0] == "team":
		if ident := c.Resolver.FindUser(parts[1]); ident != nil {
			return "https://matrix.to/#/" + url.PathEscape(string(ident.MXID))
		}
	}
	return href
}

// PermalinkTS converts the "p1689090713181639" segment of a Slack message
// link to the message's ts, "1689090713.181639". Characters after the 16
// digits are ignored.
func PermalinkTS(seg string) (string, bool) {
	digits, ok := strings.CutPrefix(seg, "p")
	if !ok || len(digits) < 16 {
		return "", false
	}
	// Anything after the 16 digits was typed after the link (e.g. "+").
	digits = digits[:16]
	for _, r := range digits {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return digits[:10] + "." + digits[10:], true
}

// safeScheme limits links to schemes Matrix clients allow in formatted bodies.
func safeScheme(href string) bool {
	u, err := url.Parse(href)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ftp", "mailto", "magnet":
		return true
	}
	return false
}

func broadcastLabel(rng string) string {
	switch rng {
	case "here", "channel", "everyone":
		return "@" + rng
	}
	return "@" + rng
}
