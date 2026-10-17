package convert

import (
	"encoding/json"
	"testing"

	"maunium.net/go/mautrix/id"

	"slack2matrix/internal/directory"
	"slack2matrix/internal/slackexport"
)

type fakeResolver struct{}

func (fakeResolver) User(slackID string) *directory.Identity {
	switch slackID {
	case "U1":
		return &directory.Identity{MXID: "@adam:example.com", DisplayName: "Adam"}
	case "U2":
		return &directory.Identity{MXID: "@slack_steve:example.com", DisplayName: "Steve"}
	}
	return nil
}

func (fakeResolver) Channel(slackID string) (string, id.RoomID) {
	if slackID == "C1" {
		return "meta", "!meta:example.com"
	}
	return "", ""
}

func (fakeResolver) Message(channelID, ts string) id.EventID {
	if channelID == "C1" && ts == "1689090713.181639" {
		return "$evt1"
	}
	return ""
}

func newConv() *Converter {
	return &Converter{Resolver: fakeResolver{}, ServerName: "example.com"}
}

func TestMrkdwn(t *testing.T) {
	tests := []struct {
		name, in, plain, html string
	}{
		{"plain", "hello world", "hello world", "hello world"},
		{"entities", "Bread &amp; Puppet &lt;3", "Bread & Puppet <3", "Bread &amp; Puppet &lt;3"},
		{"bold italic strike", "*bold* _it_ ~gone~", "*bold* _it_ ~gone~", "<strong>bold</strong> <em>it</em> <del>gone</del>"},
		{"snake case untouched", "foo_bar_baz and 2*3*4", "foo_bar_baz and 2*3*4", "foo_bar_baz and 2*3*4"},
		{"bare link", "see <https://example.com/a_b_c>", "see https://example.com/a_b_c", `see <a href="https://example.com/a_b_c">https://example.com/a_b_c</a>`},
		{"labelled link", "<https://x.org/?a=1&amp;b=2|the site>", "the site (https://x.org/?a=1&b=2)", `<a href="https://x.org/?a=1&amp;b=2">the site</a>`},
		{"user mention", "hi <@U1>!", "hi Adam!", `hi <a href="https://matrix.to/#/@adam:example.com">Adam</a>!`},
		{"unknown user", "hi <@U9|bob>", "hi @bob", "hi @bob"},
		{"channel", "see <#C1|meta> and <#C9|other>", "see #meta and #other", `see <a href="https://matrix.to/#/%21meta:example.com?via=example.com">#meta</a> and #other`},
		{"here", "<!here> lunch", "@here lunch", "@here lunch"},
		{"subteam", "<!subteam^S1|@devs> hi", "@devs hi", "@devs hi"},
		{"emoji", "nice :+1::skin-tone-3: :allthethings:", "nice 👍🏼 :allthethings:", "nice 👍🏼 :allthethings:"},
		{"code span", "run `rm -rf *` now", "run `rm -rf *` now", "run <code>rm -rf *</code> now"},
		{"code block", "look:\n```x = 1 &lt; 2\n*y*```\nok", "look:\n```\nx = 1 < 2\n*y*\n```\nok", "look:<br><pre><code>x = 1 &lt; 2\n*y*</code></pre>ok"},
		{"quote", "&gt; quoted *text*\nreply", "> quoted *text*\nreply", "<blockquote>quoted <strong>text</strong></blockquote>reply"},
		{"triple quote", "intro\n&gt;&gt;&gt; a\nb", "intro\n> a\n> b", "intro<br><blockquote>a<br>b</blockquote>"},
		{"bold in link label not broken", "<https://a.com|*x*>", "*x* (https://a.com)", `<a href="https://a.com">*x*</a>`},
		{"newlines", "a\nb", "a\nb", "a<br>b"},
		{"javascript link", "<javascript:alert(1)|click>", "click (javascript:alert(1))", "click (javascript:alert(1))"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newConv().Mrkdwn(tt.in)
			if got.Plain != tt.plain {
				t.Errorf("plain:\n got %q\nwant %q", got.Plain, tt.plain)
			}
			if got.HTML != tt.html {
				t.Errorf("html:\n got %q\nwant %q", got.HTML, tt.html)
			}
		})
	}
}

func TestMapURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://theoldgang.slack.com/archives/C1/p1689090713181639", "https://matrix.to/#/%21meta:example.com/$evt1?via=example.com"},
		{"https://theoldgang.slack.com/archives/C1/p1689090713181639?thread_ts=1689090000.000100&cid=C1", "https://matrix.to/#/%21meta:example.com/$evt1?via=example.com"},
		// Not imported (yet): the room is the best available target.
		{"https://theoldgang.slack.com/archives/C1/p1500000000000001", "https://matrix.to/#/%21meta:example.com?via=example.com"},
		{"https://theoldgang.slack.com/archives/C1", "https://matrix.to/#/%21meta:example.com?via=example.com"},
		{"https://theoldgang.slack.com/team/U1", "https://matrix.to/#/@adam:example.com"},
		// Channels that weren't imported and other sites are left alone.
		{"https://theoldgang.slack.com/archives/C9/p1689090713181639", "https://theoldgang.slack.com/archives/C9/p1689090713181639"},
		{"https://example.com/archives/C1/p1689090713181639", "https://example.com/archives/C1/p1689090713181639"},
	}
	for _, tt := range tests {
		if got := newConv().MapURL(tt.in); got != tt.want {
			t.Errorf("MapURL(%q)\n got %q\nwant %q", tt.in, got, tt.want)
		}
	}
	got := newConv().Mrkdwn("see <https://theoldgang.slack.com/archives/C1/p1689090713181639|this> and <https://theoldgang.slack.com/archives/C1/p1689090713181639>")
	wantHTML := `see <a href="https://matrix.to/#/%21meta:example.com/$evt1?via=example.com">this</a> and <a href="https://matrix.to/#/%21meta:example.com/$evt1?via=example.com">https://matrix.to/#/%21meta:example.com/$evt1?via=example.com</a>`
	if got.HTML != wantHTML {
		t.Errorf("html:\n got %q\nwant %q", got.HTML, wantHTML)
	}
}

func TestMrkdwnMentions(t *testing.T) {
	got := newConv().Mrkdwn("<@U1> and <@U2> and <@U1>")
	if len(got.Mentions) != 2 || got.Mentions[0] != "@adam:example.com" || got.Mentions[1] != "@slack_steve:example.com" {
		t.Fatalf("mentions = %v", got.Mentions)
	}
}

func blocks(t *testing.T, js string) []slackexport.Block {
	t.Helper()
	var b []slackexport.Block
	if err := json.Unmarshal([]byte(js), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRichText(t *testing.T) {
	tests := []struct {
		name, blocks, plain, html string
	}{
		{
			"styles and entities",
			`[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[
				{"type":"text","text":"Bread & "},{"type":"text","text":"Puppet","style":{"bold":true,"italic":true}},
				{"type":"text","text":" "},{"type":"text","text":"x<y","style":{"code":true}}]}]}]`,
			"Bread & Puppet `x<y`",
			"Bread &amp; <strong><em>Puppet</em></strong> <code>x&lt;y</code>",
		},
		{
			"link mention emoji",
			`[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[
				{"type":"user","user_id":"U1"},{"type":"text","text":" look "},
				{"type":"link","url":"https://a.com/x"},{"type":"text","text":" "},
				{"type":"link","url":"https://b.com","text":"B"},{"type":"text","text":" "},
				{"type":"emoji","name":"thumbsup","unicode":"1f44d","skin_tone":2},
				{"type":"emoji","name":"yqy"},{"type":"emoji","name":"smile"},
				{"type":"channel","channel_id":"C1"},{"type":"broadcast","range":"here"}]}]}]`,
			"Adam look https://a.com/x B (https://b.com) 👍🏻:yqy:😄#meta@here",
			`<a href="https://matrix.to/#/@adam:example.com">Adam</a> look <a href="https://a.com/x">https://a.com/x</a> <a href="https://b.com">B</a> 👍🏻:yqy:😄<a href="https://matrix.to/#/%21meta:example.com?via=example.com">#meta</a>@here`,
		},
		{
			"section then list",
			`[{"type":"rich_text","elements":[
				{"type":"rich_text_section","elements":[{"type":"text","text":"open to\n"}]},
				{"type":"rich_text_list","style":"ordered","indent":0,"elements":[
					{"type":"rich_text_section","elements":[{"type":"text","text":"old people"}]},
					{"type":"rich_text_section","elements":[{"type":"text","text":"ESRD"}]}]}]}]`,
			"open to\n1. old people\n2. ESRD",
			"open to<ol><li>old people</li><li>ESRD</li></ol>",
		},
		{
			"nested lists",
			`[{"type":"rich_text","elements":[
				{"type":"rich_text_list","style":"bullet","indent":0,"elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"a"}]}]},
				{"type":"rich_text_list","style":"bullet","indent":1,"elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"a1"}]},{"type":"rich_text_section","elements":[{"type":"text","text":"a2"}]}]},
				{"type":"rich_text_list","style":"bullet","indent":0,"elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"b"}]}]}]}]`,
			"• a\n    • a1\n    • a2\n• b",
			"<ul><li>a<ul><li>a1</li><li>a2</li></ul></li><li>b</li></ul>",
		},
		{
			"quote and preformatted",
			`[{"type":"rich_text","elements":[
				{"type":"rich_text_quote","elements":[{"type":"text","text":"line1\nline2","style":{"bold":true}}]},
				{"type":"rich_text_preformatted","elements":[{"type":"text","text":"if a < b {\n}"}]},
				{"type":"rich_text_section","elements":[{"type":"text","text":"after"}]}]}]`,
			"> line1\n> line2\n```\nif a < b {\n}\n```\nafter",
			"<blockquote><strong>line1<br>line2</strong></blockquote><pre><code>if a &lt; b {\n}</code></pre>after",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newConv().Message("", blocks(t, tt.blocks))
			if got.Plain != tt.plain {
				t.Errorf("plain:\n got %q\nwant %q", got.Plain, tt.plain)
			}
			if got.HTML != tt.html {
				t.Errorf("html:\n got %q\nwant %q", got.HTML, tt.html)
			}
		})
	}
}

func TestHasFormatting(t *testing.T) {
	plain := newConv().Mrkdwn("just a & b\nline")
	if plain.HasFormatting() {
		t.Errorf("plain text reported as formatted: %q", plain.HTML)
	}
	bold := newConv().Mrkdwn("*b*")
	if !bold.HasFormatting() {
		t.Errorf("bold text not reported as formatted")
	}
}
