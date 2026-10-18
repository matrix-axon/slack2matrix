package transform

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"slack2matrix/internal/config"
	"slack2matrix/internal/convert"
	"slack2matrix/internal/directory"
	"slack2matrix/internal/slackexport"
)

// newTestTransformer builds a transformer over a tiny export in a temp dir.
func newTestTransformer(t *testing.T) (*Transformer, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, v any) {
		data, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("users.json", []map[string]any{
		{"id": "U1", "name": "adam", "profile": map[string]any{"display_name": "Adam"}},
		{"id": "U2", "name": "steve", "profile": map[string]any{"real_name": "Steve S"}},
	})
	write("channels.json", []map[string]any{{"id": "C1", "name": "general", "created": 1400000000}})
	media := filepath.Join(dir, "media", "files.slack.com", "files-pri", "T-F1", "download")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(media, "cat.jpg?t=tok"), []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(media, "snip.py?t=tok"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exp, err := slackexport.Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Homeserver: config.Homeserver{ServerName: "example.com"},
		AppService: config.AppService{GhostPrefix: "slack_"},
		Users:      map[string]id.UserID{"U1": "@adam:example.com"},
	}
	dir2, err := directory.Build(cfg, exp, nil)
	if err != nil {
		t.Fatal(err)
	}
	res := resolver{dir2}
	return &Transformer{
		Conv: &convert.Converter{Resolver: res, ServerName: "example.com"},
		Dir:  dir2,
		Exp:  exp,
		Opts: Options{FetchCardImages: true, SnippetInlineLimit: 16 * 1024},
	}, dir
}

type resolver struct{ d *directory.Directory }

func (r resolver) User(s string) *directory.Identity   { return r.d.User(s) }
func (r resolver) Channel(string) (string, id.RoomID)  { return "", "" }
func (r resolver) Message(string, string) id.EventID   { return "" }
func (r resolver) FindUser(string) *directory.Identity { return nil }

func msg(t *testing.T, js string) *slackexport.Message {
	t.Helper()
	var m slackexport.Message
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func TestPlainMessageWithReactions(t *testing.T) {
	tr, _ := newTestTransformer(t)
	drafts := tr.Message(msg(t, `{"type":"message","user":"U2","ts":"1500000000.000100","text":"hi <@U1>",
		"reactions":[{"name":"+1::skin-tone-2","users":["U1","U2"],"count":2},{"name":"partyparrot","users":["U1"],"count":1}]}`))
	if len(drafts) != 4 {
		t.Fatalf("got %d drafts, want 4", len(drafts))
	}
	d := drafts[0]
	if d.Kind != KindMessage || d.Sender.MXID != "@slack_steve:example.com" || d.TS != 1500000000000 {
		t.Errorf("unexpected message draft: %+v", d)
	}
	if d.Content.Body != "hi Adam" || d.Content.Mentions == nil || len(d.Content.Mentions.UserIDs) != 1 {
		t.Errorf("unexpected content: %+v", d.Content)
	}
	if drafts[1].ReactionKey != "👍🏻" || drafts[1].Sender.MXID != "@adam:example.com" || !drafts[1].Sender.Real {
		t.Errorf("unexpected reaction: %+v", drafts[1])
	}
	if drafts[3].ReactionKey != ":partyparrot:" {
		t.Errorf("custom emoji reaction key = %q", drafts[3].ReactionKey)
	}
	for i, d := range drafts {
		if d.Part != i {
			t.Errorf("draft %d has part %d", i, d.Part)
		}
	}
}

func TestSingleFileBecomesCaptionedMedia(t *testing.T) {
	tr, _ := newTestTransformer(t)
	drafts := tr.Message(msg(t, `{"type":"message","user":"U1","ts":"1500000000.000200","text":"look at this",
		"files":[{"id":"F1","name":"cat.jpg","title":"cat.jpg","mimetype":"image/jpeg","mode":"hosted","size":4,
		"original_w":10,"original_h":20,
		"url_private_download":"https://files.slack.com/files-pri/T-F1/download/cat.jpg?t=tok"}]}`))
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want 1", len(drafts))
	}
	d := drafts[0]
	if d.Kind != KindMedia || d.Content.MsgType != event.MsgImage || d.Content.Body != "look at this" || d.Content.FileName != "cat.jpg" {
		t.Errorf("unexpected media draft: %+v / %+v", d, d.Content)
	}
	if d.Media.LocalPath == "" || d.FileID != "F1" || d.Media.Width != 10 {
		t.Errorf("unexpected media ref: %+v", d.Media)
	}
}

func TestMissingAndDeletedFiles(t *testing.T) {
	tr, _ := newTestTransformer(t)
	drafts := tr.Message(msg(t, `{"type":"message","user":"U1","ts":"1500000000.000300","text":"",
		"files":[{"id":"F2","name":"gone.png","mode":"hosted","url_private_download":"https://files.slack.com/files-pri/T-F2/download/gone.png?t=tok"},
		{"id":"F3","mode":"tombstone"}]}`))
	if len(drafts) != 1 || drafts[0].Kind != KindMessage {
		t.Fatalf("want one text draft, got %+v", drafts)
	}
	want := "[file unavailable: gone.png]\n[file deleted]"
	if drafts[0].Content.Body != want {
		t.Errorf("body = %q, want %q", drafts[0].Content.Body, want)
	}
}

func TestSnippetInlined(t *testing.T) {
	tr, _ := newTestTransformer(t)
	drafts := tr.Message(msg(t, `{"type":"message","user":"U1","ts":"1500000000.000400","text":"",
		"files":[{"id":"F4","name":"snip.py","title":"My script","filetype":"python","mode":"snippet","size":12,
		"url_private_download":"https://files.slack.com/files-pri/T-F1/download/snip.py?t=tok"}]}`))
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts", len(drafts))
	}
	c := drafts[0].Content
	if c.Body != "My script:\n```\nprint('hi')\n```" {
		t.Errorf("body = %q", c.Body)
	}
	if c.FormattedBody != `<strong>My script</strong><pre><code class="language-python">print(&#39;hi&#39;)</code></pre>` {
		t.Errorf("html = %q", c.FormattedBody)
	}
}

func TestSubtypes(t *testing.T) {
	tr, _ := newTestTransformer(t)
	join := tr.Message(msg(t, `{"type":"message","subtype":"channel_join","user":"U2","ts":"1500000000.000500","text":"<@U2> has joined"}`))
	if len(join) != 1 || join[0].Kind != KindMembership || join[0].Membership != event.MembershipJoin {
		t.Errorf("channel_join: %+v", join)
	}
	me := tr.Message(msg(t, `{"type":"message","subtype":"me_message","user":"U2","ts":"1500000000.000600","text":"waves"}`))
	if len(me) != 1 || me[0].Content.MsgType != event.MsgEmote {
		t.Errorf("me_message: %+v", me)
	}
	hidden := tr.Message(msg(t, `{"type":"message","user":"U2","ts":"1500000000.000700","text":"x","hidden":true}`))
	if hidden != nil {
		t.Errorf("hidden message produced drafts")
	}
	fc := tr.Message(msg(t, `{"type":"message","subtype":"file_comment","ts":"1500000000.000800","text":"...",
		"comment":{"user":"U1","comment":"nice *pic*"},"file":{"id":"F1"}}`))
	if len(fc) != 1 || fc[0].ReplyToFile != "F1" || fc[0].Sender.MXID != "@adam:example.com" || fc[0].Content.FormattedBody != "nice <strong>pic</strong>" {
		t.Errorf("file_comment: %+v", fc[0])
	}
}

func TestBotMessagesAreNotices(t *testing.T) {
	tr, _ := newTestTransformer(t)
	drafts := tr.Message(msg(t, `{"type":"message","subtype":"bot_message","bot_id":"B1","username":"Reacji Channeler","ts":"1500000000.000900",
		"text":"","attachments":[{"author_name":"Steve","text":"quoted *msg*","footer":"Twitter"},{"from_url":"https://x.com","title":"unfurl"}]}`))
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts", len(drafts))
	}
	d := drafts[0]
	if d.Content.MsgType != event.MsgNotice || d.Sender.MXID != "@slack_reacji_channeler:example.com" {
		t.Errorf("unexpected: %+v %+v", d, d.Content)
	}
	if d.Content.Body != "> Steve\n> quoted *msg*\n> Twitter" {
		t.Errorf("body = %q (unfurl should be skipped)", d.Content.Body)
	}
}

func TestCardImageFetched(t *testing.T) {
	tr, _ := newTestTransformer(t)
	drafts := tr.Message(msg(t, `{"type":"message","user":"U1","ts":"1500000001.000100","text":"/giphy hi",
		"attachments":[{"title":"hi","title_link":"https://giphy.com/x","image_url":"https://media.giphy.com/media/x/giphy.gif","image_width":200,"image_height":100}]}`))
	if len(drafts) != 2 || drafts[1].Kind != KindMedia || drafts[1].Media.RemoteURL == "" || drafts[1].Media.Mimetype != "image/gif" {
		t.Fatalf("unexpected drafts: %+v", drafts)
	}
}

func TestDuplicateReactionsCollapsed(t *testing.T) {
	tr, _ := newTestTransformer(t)
	drafts := tr.Message(msg(t, `{"type":"message","user":"U2","ts":"1500000002.000100","text":"x",
		"reactions":[{"name":"+1","users":["U1"],"count":1},{"name":"thumbsup","users":["U1","U2"],"count":2}]}`))
	if len(drafts) != 3 {
		t.Fatalf("got %d drafts, want message + 2 reactions", len(drafts))
	}
}

// linkResolver knows one imported room and message, for link mapping.
type linkResolver struct{ resolver }

func (linkResolver) Channel(string) (string, id.RoomID) { return "general", "!general:example.com" }
func (linkResolver) Message(_, ts string) id.EventID {
	if ts == "1500000000.000100" {
		return "$reply"
	}
	return ""
}
func (r linkResolver) FindUser(ref string) *directory.Identity { return r.d.Find(ref) }

func TestReplyBroadcastCardsQuoteMessages(t *testing.T) {
	tr, _ := newTestTransformer(t)
	tr.Conv = &convert.Converter{Resolver: linkResolver{resolver{tr.Dir}}, ServerName: "example.com"}
	// Older exports attach the reply as a card with only from_url to mark
	// it as a quoted message; it must not be dropped as a link preview.
	drafts := tr.Message(msg(t, `{"type":"message","subtype":"reply_broadcast","user":"U2","ts":"1500000000.000200","text":"",
		"attachments":[{"from_url":"https://x.slack.com/archives/C1/p1500000000000100?thread_ts=1400000000000100&cid=C1",
			"author_name":"Steve S","author_link":"https://x.slack.com/team/adam","text":"the reply","ts":"1500000000.000100"}]}`))
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want 1", len(drafts))
	}
	html := drafts[0].Content.FormattedBody
	for _, want := range []string{
		"the reply",
		`<a href="https://matrix.to/#/@adam:example.com"><strong>Steve S</strong></a>`,
		`<a href="https://matrix.to/#/%21general:example.com/$reply?via=example.com">View message</a>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("formatted body lacks %q:\n%s", want, html)
		}
	}
}
