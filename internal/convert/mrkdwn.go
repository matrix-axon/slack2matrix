package convert

import (
	"html"
	"regexp"
	"strings"

	"slack2matrix/internal/emoji"
)

// Slack's mrkdwn escapes &, < and > as entities and wraps links and mentions
// in <...>. Inline styles use *bold*, _italic_, ~strike~ and `code`.

var (
	entityRe    = regexp.MustCompile(`<([^<>\n]+)>`)
	codeSpanRe  = regexp.MustCompile("`([^`\n]+)`")
	shortcodeRe = regexp.MustCompile(`:([a-z0-9_+'-]+)(::skin-tone-[2-6])?:`)
	boldRe      = styleRe(`\*`)
	italicRe    = styleRe(`_`)
	strikeRe    = styleRe(`~`)
)

// styleRe matches a marker pair that opens after start/space/punctuation and
// closes before end/space/punctuation, mirroring Slack's word-boundary rule.
func styleRe(m string) *regexp.Regexp {
	const pua = `\x{F0000}-\x{FFFFD}`
	return regexp.MustCompile(`(^|[\s(\[{"';>` + pua + `])` + m + `([^\s` + m + `](?:[^` + m + `\n]*[^\s` + m + `])?)` + m + `($|[\s.,!?;:)\]}"'<&` + pua + `])`)
}

// placeholders stand in for already-rendered fragments while styles are
// applied; they use Supplementary Private Use Area-A, which real text never contains.
const puaBase = 0xF0000

type placeholders struct {
	plain []string
	html  []string
}

func (p *placeholders) add(plain, htm string) string {
	p.plain = append(p.plain, plain)
	p.html = append(p.html, htm)
	return string(rune(puaBase + len(p.plain) - 1))
}

func (p *placeholders) expand(s string, useHTML bool) string {
	if len(p.plain) == 0 {
		return s
	}
	var sb strings.Builder
	for _, r := range s {
		if r >= puaBase && int(r-puaBase) < len(p.plain) {
			if useHTML {
				sb.WriteString(p.html[r-puaBase])
			} else {
				sb.WriteString(p.plain[r-puaBase])
			}
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// Mrkdwn converts Slack mrkdwn text.
func (c *Converter) Mrkdwn(s string) Text {
	var out writer
	// Fenced code blocks first: their content is not formatted.
	parts := strings.Split(s, "```")
	for i, part := range parts {
		if i%2 == 1 && i < len(parts)-1 {
			code := strings.Trim(part, "\n")
			pw := &writer{}
			c.entities(pw, code, true)
			res := pw.result()
			out.raw("```\n"+res.Plain+"\n```\n", "<pre><code>"+res.HTML+"</code></pre>")
			out.mentions = appendUnique(out.mentions, res.Mentions...)
			continue
		}
		if i%2 == 1 {
			// Unbalanced fence: treat the marker literally.
			part = "```" + part
		}
		c.mrkdwnBlocks(&out, strings.Trim(part, "\n"), i > 0)
	}
	res := out.result()
	res.Plain = strings.TrimRight(res.Plain, "\n")
	for strings.HasSuffix(res.HTML, "<br>") {
		res.HTML = strings.TrimSuffix(res.HTML, "<br>")
	}
	return res
}

// mrkdwnBlocks splits text into quoted and unquoted runs of lines.
func (c *Converter) mrkdwnBlocks(w *writer, s string, afterCode bool) {
	if s == "" {
		return
	}
	lines := strings.Split(s, "\n")
	var quote []string
	flushQuote := func() {
		if len(quote) == 0 {
			return
		}
		res := c.inlineMrkdwn(strings.Join(quote, "\n"))
		w.raw(quotePlain(res.Plain)+"\n", "<blockquote>"+res.HTML+"</blockquote>")
		w.mentions = appendUnique(w.mentions, res.Mentions...)
		quote = nil
	}
	var normal []string
	flushNormal := func() {
		if len(normal) == 0 {
			return
		}
		res := c.inlineMrkdwn(strings.Join(normal, "\n"))
		w.raw(res.Plain+"\n", res.HTML+"<br>")
		w.mentions = appendUnique(w.mentions, res.Mentions...)
		normal = nil
	}
	for i, line := range lines {
		if rest, ok := strings.CutPrefix(line, "&gt;&gt;&gt;"); ok {
			// ">>>" quotes everything that follows.
			flushNormal()
			quote = append(quote, strings.TrimPrefix(rest, " "))
			quote = append(quote, lines[i+1:]...)
			break
		}
		if rest, ok := strings.CutPrefix(line, "&gt;"); ok {
			flushNormal()
			quote = append(quote, strings.TrimPrefix(rest, " "))
			continue
		}
		flushQuote()
		normal = append(normal, line)
	}
	flushNormal()
	flushQuote()
}

// inlineMrkdwn renders one run of lines with inline styles.
func (c *Converter) inlineMrkdwn(s string) Text {
	ph := &placeholders{}
	w := &writer{}

	// Code spans: content is literal (but entities are still decoded).
	s = codeSpanRe.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[1 : len(m)-1]
		cw := &writer{}
		c.entities(cw, inner, true)
		r := cw.result()
		w.mentions = appendUnique(w.mentions, r.Mentions...)
		return ph.add("`"+r.Plain+"`", "<code>"+r.HTML+"</code>")
	})
	// Links, mentions and special commands.
	s = entityRe.ReplaceAllStringFunc(s, func(m string) string {
		ew := &writer{}
		c.entity(ew, m[1:len(m)-1], false)
		r := ew.result()
		w.mentions = appendUnique(w.mentions, r.Mentions...)
		return ph.add(r.Plain, r.HTML)
	})
	// Emoji shortcodes.
	s = shortcodeRe.ReplaceAllStringFunc(s, func(m string) string {
		if u, ok := emoji.Lookup(strings.Trim(m, ":"), 0); ok {
			return ph.add(u, u)
		}
		return m
	})
	plain := html.UnescapeString(ph.expand(s, false))

	// HTML: escape the remaining literal text (decoding Slack's entities
	// first), then apply styles around placeholders.
	h := html.EscapeString(html.UnescapeString(s))
	for _, st := range []struct {
		re  *regexp.Regexp
		tag string
	}{{boldRe, "strong"}, {italicRe, "em"}, {strikeRe, "del"}} {
		// Run twice so adjacent matches sharing a boundary character are caught.
		for range 2 {
			h = st.re.ReplaceAllString(h, "${1}<"+st.tag+">${2}</"+st.tag+">${3}")
		}
	}
	h = strings.ReplaceAll(h, "\n", "<br>")
	h = ph.expand(h, true)
	return Text{Plain: plain, HTML: h, Mentions: w.mentions}
}

// entities renders text that may contain <...> entities but no styles.
func (c *Converter) entities(w *writer, s string, pre bool) {
	last := 0
	for _, loc := range entityRe.FindAllStringIndex(s, -1) {
		w.text(html.UnescapeString(s[last:loc[0]]), pre)
		c.entity(w, s[loc[0]+1:loc[1]-1], pre)
		last = loc[1]
	}
	w.text(html.UnescapeString(s[last:]), pre)
}

// entity renders the inside of a <...> mrkdwn entity.
func (c *Converter) entity(w *writer, inner string, pre bool) {
	target, label, _ := strings.Cut(inner, "|")
	label = html.UnescapeString(label)
	switch {
	case strings.HasPrefix(target, "@"):
		c.userMention(w, target[1:], strings.TrimPrefix(label, "@"), pre)
	case strings.HasPrefix(target, "#"):
		c.channelMention(w, target[1:], label, pre)
	case strings.HasPrefix(target, "!subteam^"):
		if label == "" {
			label = "@" + strings.TrimPrefix(target, "!subteam^")
		}
		w.text(label, pre)
	case strings.HasPrefix(target, "!date^"):
		w.text(label, pre)
	case strings.HasPrefix(target, "!"):
		cmd, _, _ := strings.Cut(target[1:], "^")
		if label != "" {
			w.text(label, pre)
		} else {
			w.text(broadcastLabel(cmd), pre)
		}
	default:
		c.link(w, html.UnescapeString(target), label, pre)
	}
}
