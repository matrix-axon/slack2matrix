package slackexport

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTS(t *testing.T) {
	cases := map[TS]int64{"1492622693.330270": 1492622693330, "1392383645.000002": 1392383645000, "1400000000": 1400000000000}
	for ts, want := range cases {
		if got := ts.Millis(); got != want {
			t.Errorf("%s.Millis() = %d, want %d", ts, got, want)
		}
	}
	if !TS("9.5").Less("10.1") {
		t.Error("timestamps must compare numerically, not as strings")
	}
}

func TestReaderLatin1DirAndAppleDouble(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "users.json"), `[]`)
	write(filepath.Join(dir, "channels.json"), `[{"id":"C1","name":"trophés"}]`)
	latin1 := filepath.Join(dir, "troph\xe9s")
	write(filepath.Join(latin1, "2020-01-02.json"), `[{"type":"message","ts":"1577923200.000200","text":"b"},{"type":"message","ts":"1577923200.000100","text":"a"}]`)
	write(filepath.Join(latin1, "2020-01-01.json"), `[{"type":"message","ts":"1577836800.000100","text":"first","attachments":[{"ts":1577836800}]}]`)
	write(filepath.Join(latin1, "._2020-01-01.json"), "\x00\x05\x16\x07")
	exp, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := exp.Messages(exp.Channels[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || msgs[0].Text != "first" || msgs[1].Text != "a" || msgs[2].Text != "b" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
}

func TestLocalPath(t *testing.T) {
	e := &Export{MediaDir: "/m"}
	got := e.LocalPath("https://files.slack.com/files-pri/T-F/download/a%20b.png?t=xoxe-1")
	if want := "/m/files.slack.com/files-pri/T-F/download/a%20b.png?t=xoxe-1"; got != want {
		t.Errorf("LocalPath = %q, want %q", got, want)
	}
}
