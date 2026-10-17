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
	// Channel returns a channel's name and its Matrix room (empty if not imported).
	Channel(slackID string) (name string, room id.RoomID)
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
