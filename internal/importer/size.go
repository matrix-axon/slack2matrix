package importer

import (
	"context"
	"encoding/json"
	"os"
	"unicode/utf8"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// maxPlaintextContent keeps events under Synapse's 64 KiB limit once
// encrypted: Megolm ciphertext is base64 (4/3 larger) inside an envelope.
const maxPlaintextContent = 44_000

// previewLength is how much text stays visible when a message is too long
// and its full text is attached as a file.
const previewLength = 4000

// fitSize shrinks an oversized text message: first by dropping its HTML,
// then by attaching the full text as an encrypted file with a preview.
func (imp *Importer) fitSize(ctx context.Context, sender id.UserID, c *event.MessageEventContent) error {
	if contentSize(c) <= maxPlaintextContent {
		return nil
	}
	c.Format, c.FormattedBody = "", ""
	if contentSize(c) <= maxPlaintextContent {
		return nil
	}
	if err := os.MkdirAll(imp.Cfg.TmpDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(imp.Cfg.TmpDir, "long-message-*.txt")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(c.Body)
	tmp.Close()
	if err != nil {
		return err
	}
	file, err := imp.encryptUpload(ctx, sender, tmp.Name(), int64(len(c.Body)))
	if err != nil {
		return err
	}
	preview := truncateUTF8(c.Body, previewLength)
	imp.Log.Info().Int("bytes", len(c.Body)).Msg("Message too long for one event; attaching full text")
	*c = event.MessageEventContent{
		MsgType:  event.MsgFile,
		Body:     preview + "\n\n[Message truncated; the full text is attached.]",
		FileName: "message.txt",
		File:     file,
		Info:     &event.FileInfo{MimeType: "text/plain", Size: len(c.Body)},
		Mentions: c.Mentions,
	}
	return nil
}

func contentSize(c *event.MessageEventContent) int {
	data, err := json.Marshal(c)
	if err != nil {
		return 0
	}
	return len(data)
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
