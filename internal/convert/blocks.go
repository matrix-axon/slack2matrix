package convert

import (
	"html"
	"strconv"
	"strings"

	"slack2matrix/internal/emoji"
	"slack2matrix/internal/slackexport"
)

// HasRichText reports whether the blocks include a rich_text block.
func HasRichText(blocks []slackexport.Block) bool {
	for _, b := range blocks {
		if b.Type == "rich_text" {
			return true
		}
	}
	return false
}

// Message converts a message's text, preferring rich_text blocks (which carry
// exact structure) and falling back to the mrkdwn text field.
func (c *Converter) Message(text string, blocks []slackexport.Block) Text {
	if !HasRichText(blocks) {
		out := c.Mrkdwn(text)
		// Non-rich blocks (sections/context from apps) add content the text
		// field may lack.
		for _, b := range blocks {
			out.Append(c.layoutBlock(b))
		}
		return out
	}
	var out Text
	for _, b := range blocks {
		if b.Type == "rich_text" {
			out.Append(c.RichText(b))
		} else {
			out.Append(c.layoutBlock(b))
		}
	}
	return out
}

// layoutBlock renders a non-rich_text Block Kit block.
func (c *Converter) layoutBlock(b slackexport.Block) Text {
	switch b.Type {
	case "section":
		var out Text
		if b.Text != nil {
			out.Append(c.textObject(*b.Text))
		}
		for _, f := range b.Fields {
			out.Append(c.textObject(f))
		}
		return out
	case "context":
		var parts []Text
		for _, el := range b.Elements {
			switch el.Type {
			case "mrkdwn":
				parts = append(parts, c.Mrkdwn(el.Text))
			case "plain_text":
				parts = append(parts, Text{Plain: el.Text, HTML: plainToHTML(el.Text)})
			}
		}
		var out Text
		for i, p := range parts {
			if i > 0 {
				out.Plain += " | "
				out.HTML += " | "
			}
			out.Plain += p.Plain
			out.HTML += p.HTML
			out.Mentions = appendUnique(out.Mentions, p.Mentions...)
		}
		if out.HTML != "" {
			out.HTML = "<sub>" + out.HTML + "</sub>"
		}
		return out
	case "header":
		if b.Text != nil {
			return Text{Plain: b.Text.Text, HTML: "<strong>" + html.EscapeString(b.Text.Text) + "</strong>"}
		}
	}
	// Image blocks are handled as media by the importer; dividers, actions
	// and inputs have no meaningful text.
	return Text{}
}

func (c *Converter) textObject(t slackexport.TextObj) Text {
	if t.Type == "mrkdwn" {
		return c.Mrkdwn(t.Text)
	}
	return Text{Plain: t.Text, HTML: plainToHTML(t.Text)}
}

// RichText renders one rich_text block.
func (c *Converter) RichText(b slackexport.Block) Text {
	w := &writer{}
	els := b.Elements
	for i := 0; i < len(els); i++ {
		el := els[i]
		nextIsBlock := i+1 < len(els) && els[i+1].Type != "rich_text_section"
		switch el.Type {
		case "rich_text_section":
			c.section(w, el.Elements, nextIsBlock)
		case "rich_text_preformatted":
			w.raw("```\n", "<pre><code>")
			for _, inner := range el.Elements {
				c.inline(w, inner, true)
			}
			w.raw("\n```\n", "</code></pre>")
		case "rich_text_quote":
			sub := &writer{}
			for _, inner := range el.Elements {
				c.inline(sub, inner, false)
			}
			res := sub.result()
			w.mentions = appendUnique(w.mentions, res.Mentions...)
			w.raw(quotePlain(strings.TrimSuffix(res.Plain, "\n"))+"\n", "<blockquote>"+strings.TrimSuffix(res.HTML, "<br>")+"</blockquote>")
		case "rich_text_list":
			// Consecutive lists form one (possibly nested) list structure.
			j := i
			for j+1 < len(els) && els[j+1].Type == "rich_text_list" {
				j++
			}
			c.lists(w, els[i:j+1])
			i = j
		}
	}
	res := w.result()
	res.Plain = strings.TrimRight(res.Plain, "\n")
	for strings.HasSuffix(res.HTML, "<br>") {
		res.HTML = strings.TrimSuffix(res.HTML, "<br>")
	}
	return res
}

// section renders inline elements. A trailing newline before a block-level
// element is dropped from the HTML, since the block already breaks the line.
func (c *Converter) section(w *writer, els []slackexport.Element, nextIsBlock bool) {
	if nextIsBlock && len(els) > 0 {
		if last := els[len(els)-1]; last.Type == "text" && strings.HasSuffix(last.Text, "\n") {
			trimmed := make([]slackexport.Element, len(els))
			copy(trimmed, els)
			trimmed[len(trimmed)-1].Text = strings.TrimSuffix(last.Text, "\n")
			els = trimmed
		}
		defer w.raw("\n", "")
	}
	for _, el := range els {
		c.inline(w, el, false)
	}
}

func quotePlain(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// inline renders a leaf element of a section, quote or preformatted block.
func (c *Converter) inline(w *writer, el slackexport.Element, pre bool) {
	switch el.Type {
	case "text":
		if pre {
			w.text(el.Text, true)
			return
		}
		c.styled(w, el.Style, func() { w.text(el.Text, false) }, el.Text)
	case "link":
		if pre {
			c.link(w, el.URL, el.Text, true)
			return
		}
		c.styled(w, el.Style, func() { c.link(w, el.URL, el.Text, false) }, el.Text+el.URL)
	case "emoji":
		w.text(emojiElement(el), pre)
	case "user":
		c.userMention(w, el.UserID, "", pre)
	case "channel":
		c.channelMention(w, el.ChannelID, "", pre)
	case "usergroup":
		w.text("@"+el.UsergroupID, pre)
	case "broadcast":
		w.text(broadcastLabel(el.Range), pre)
	case "color":
		w.text(el.Value, pre)
	case "date":
		w.text(el.Text, pre)
	default:
		if el.Text != "" {
			w.text(el.Text, pre)
		}
	}
}

// styled wraps content in HTML style tags. Plain text keeps no markers except
// backticks for code, which carry meaning for readers.
func (c *Converter) styled(w *writer, st slackexport.Style, content func(), text string) {
	if strings.TrimSpace(text) == "" && !st.Code {
		content()
		return
	}
	var open, close string
	if st.Bold {
		open, close = open+"<strong>", "</strong>"+close
	}
	if st.Italic {
		open, close = open+"<em>", "</em>"+close
	}
	if st.Strike {
		open, close = open+"<del>", "</del>"+close
	}
	if st.Code {
		open, close = open+"<code>", "</code>"+close
	}
	if st.Code {
		w.plain.WriteString("`")
	}
	w.html.WriteString(open)
	content()
	w.html.WriteString(close)
	if st.Code {
		w.plain.WriteString("`")
	}
}

// emojiElement resolves a rich_text emoji, preferring the code points Slack
// recorded and falling back to the shortcode table.
func emojiElement(el slackexport.Element) string {
	if el.Unicode != "" {
		if s := emoji.FromHex(el.Unicode); s != "" {
			if el.SkinTone >= 2 {
				if toned, ok := emoji.Lookup(el.Name, el.SkinTone); ok {
					return toned
				}
			}
			return s
		}
	}
	return emoji.Render(el.Name, el.SkinTone)
}

// lists renders consecutive rich_text_list elements, nesting by indent.
func (c *Converter) lists(w *writer, lists []slackexport.Element) {
	type level struct {
		tag    string
		indent int
	}
	var stack []level
	// counters track ordered numbering per indent for the plain-text output.
	counters := map[int]int{}
	closeTo := func(indent int) {
		for len(stack) > 0 && stack[len(stack)-1].indent > indent {
			top := stack[len(stack)-1]
			w.html.WriteString("</li></" + top.tag + ">")
			stack = stack[:len(stack)-1]
			delete(counters, top.indent)
		}
	}
	for _, l := range lists {
		tag := "ul"
		if l.Style.ListStyle == "ordered" {
			tag = "ol"
		}
		closeTo(l.Indent)
		if len(stack) > 0 && stack[len(stack)-1].indent == l.Indent && stack[len(stack)-1].tag != tag {
			top := stack[len(stack)-1]
			w.html.WriteString("</li></" + top.tag + ">")
			stack = stack[:len(stack)-1]
		}
		opened := false
		if len(stack) == 0 || stack[len(stack)-1].indent < l.Indent {
			attr := ""
			if tag == "ol" && l.Offset > 0 {
				attr = ` start="` + strconv.Itoa(l.Offset+1) + `"`
				counters[l.Indent] = l.Offset
			}
			w.html.WriteString("<" + tag + attr + ">")
			stack = append(stack, level{tag, l.Indent})
			opened = true
		}
		for k, item := range l.Elements {
			if !(opened && k == 0) {
				w.html.WriteString("</li>")
			}
			w.html.WriteString("<li>")
			prefix := strings.Repeat("    ", l.Indent)
			if tag == "ol" {
				counters[l.Indent]++
				prefix += strconv.Itoa(counters[l.Indent]) + ". "
			} else {
				prefix += "• "
			}
			w.plain.WriteString(prefix)
			for _, inner := range item.Elements {
				c.inline(w, inner, false)
			}
			w.plain.WriteString("\n")
		}
	}
	closeTo(-1)
}
