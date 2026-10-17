package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"slack2matrix/internal/media"
	"slack2matrix/internal/transform"
)

// maxRemoteImage bounds downloads of card images.
const maxRemoteImage = 100 << 20

// storedMedia is what's cached per uploaded file: the fields merged into
// every message that shares it.
type storedMedia struct {
	File *event.EncryptedFileInfo `json:"file"`
	Info *event.FileInfo          `json:"info"`
}

func mediaKey(m *transform.Media) string {
	if m.SlackFileID != "" {
		return "slack:" + m.SlackFileID
	}
	return "url:" + m.RemoteURL
}

// attachMedia uploads (or reuses) the draft's media, encrypted, and fills in
// the content's file and info. ok is false if the media is unavailable.
func (imp *Importer) attachMedia(ctx context.Context, sender id.UserID, m *transform.Media, c *event.MessageEventContent) (ok bool, err error) {
	key := mediaKey(m)
	if failure, err := imp.Store.MediaFailure(ctx, key); err != nil {
		return false, err
	} else if failure != "" {
		return false, nil
	}
	stored, err := imp.Store.Media(ctx, key)
	if err != nil {
		return false, err
	}
	var sm storedMedia
	if stored == "" {
		sm, err = imp.uploadMedia(ctx, sender, m)
		if errors.Is(err, errUnavailable) {
			imp.Log.Warn().Err(err).Str("media", key).Msg("Media unavailable")
			return false, imp.Store.SetMediaFailure(ctx, key, err.Error())
		} else if err != nil {
			return false, err
		}
		data, err := json.Marshal(sm)
		if err != nil {
			return false, err
		}
		if err := imp.Store.SetMedia(ctx, key, string(data)); err != nil {
			return false, err
		}
	} else if err := json.Unmarshal([]byte(stored), &sm); err != nil {
		return false, fmt.Errorf("corrupt media record %s: %w", key, err)
	}
	c.File = sm.File
	c.Info = sm.Info
	c.MsgType = transform.MsgTypeFor(sm.Info.MimeType)
	if sm.Info.MimeType == "image/jpeg" && m.Mimetype != "image/jpeg" {
		// Converted (e.g. from HEIC): make the file name match the content.
		c.FileName = strings.TrimSuffix(c.FileName, filepath.Ext(c.FileName)) + ".jpg"
		if c.Body == m.Name {
			c.Body = c.FileName
		}
	}
	return true, nil
}

var errUnavailable = errors.New("media unavailable")

func (imp *Importer) uploadMedia(ctx context.Context, sender id.UserID, m *transform.Media) (storedMedia, error) {
	path := m.LocalPath
	if path == "" {
		tmp, err := imp.download(ctx, m.RemoteURL)
		if err != nil {
			return storedMedia{}, err
		}
		defer os.Remove(tmp)
		path = tmp
	}
	prepared, err := media.Prepare(ctx, path, m.Name, m.Mimetype, imp.Cfg.TmpDir)
	if err != nil {
		return storedMedia{}, fmt.Errorf("preparing %s: %w", m.Name, err)
	}
	defer prepared.Cleanup()

	info := &event.FileInfo{
		MimeType: prepared.Mimetype,
		Size:     int(prepared.Size),
		Width:    prepared.Width,
		Height:   prepared.Height,
		Duration: int(prepared.DurationMS),
	}
	if info.Width == 0 && m.Width > 0 && prepared.Mimetype == m.Mimetype {
		info.Width, info.Height = m.Width, m.Height
	}
	file, err := imp.encryptUpload(ctx, sender, prepared.Path, prepared.Size)
	if err != nil {
		return storedMedia{}, err
	}
	if prepared.Thumbnail != nil {
		thumb, err := imp.encryptUpload(ctx, sender, prepared.Thumbnail.Path, prepared.Thumbnail.Size)
		if err != nil {
			return storedMedia{}, fmt.Errorf("uploading thumbnail: %w", err)
		}
		info.ThumbnailFile = thumb
		info.ThumbnailInfo = &event.FileInfo{
			MimeType: prepared.Thumbnail.Mimetype,
			Size:     int(prepared.Thumbnail.Size),
			Width:    prepared.Thumbnail.Width,
			Height:   prepared.Thumbnail.Height,
		}
	}
	return storedMedia{File: file, Info: info}, nil
}

// encryptUpload encrypts a file with a fresh key and uploads the ciphertext.
// The file is read twice (once to hash the ciphertext, once to upload) rather
// than writing an encrypted copy to disk.
func (imp *Importer) encryptUpload(ctx context.Context, sender id.UserID, path string, size int64) (*event.EncryptedFileInfo, error) {
	ef := attachment.NewEncryptedFile()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	hashing := ef.EncryptStream(f)
	if _, err := io.Copy(io.Discard, hashing); err != nil {
		f.Close()
		return nil, err
	}
	// Close computes the SHA-256 of the ciphertext into ef.
	if err := hashing.Close(); err != nil {
		return nil, err
	}

	var resp *mautrix.RespMediaUpload
	for attempt := 1; ; attempt++ {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		// Don't Close this stream: that would recompute the hash.
		upload := ef.EncryptStream(f)
		resp, err = imp.MX.Client(sender).UploadMedia(ctx, mautrix.ReqUploadMedia{
			Content:       upload,
			ContentLength: size,
			ContentType:   "application/octet-stream",
		})
		f.Close()
		if err == nil {
			break
		}
		var httpErr mautrix.HTTPError
		if errors.As(err, &httpErr) && httpErr.Response != nil && httpErr.Response.StatusCode == http.StatusRequestEntityTooLarge {
			return nil, fmt.Errorf("%w: %s (%d bytes) is larger than the server's upload limit", errUnavailable, filepath.Base(path), size)
		}
		if attempt >= 3 || ctx.Err() != nil {
			return nil, fmt.Errorf("uploading %s: %w", filepath.Base(path), err)
		}
		imp.Log.Warn().Err(err).Int("attempt", attempt).Str("file", path).Msg("Upload failed, retrying")
	}
	return &event.EncryptedFileInfo{EncryptedFile: *ef, URL: resp.ContentURI.CUString()}, nil
}

// download fetches a remote image into the temp directory.
func (imp *Importer) download(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errUnavailable, err)
	}
	resp, err := imp.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%w: %v", errUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: HTTP %d from %s", errUnavailable, resp.StatusCode, rawURL)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "video/") {
		return "", fmt.Errorf("%w: %s returned %q, not an image", errUnavailable, rawURL, ct)
	}
	if err := os.MkdirAll(imp.Cfg.TmpDir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(imp.Cfg.TmpDir, "remote-*")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxRemoteImage+1))
	tmp.Close()
	if err == nil && n > maxRemoteImage {
		err = fmt.Errorf("%w: %s is larger than %d bytes", errUnavailable, rawURL, maxRemoteImage)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

func escapeAttr(s string) string { return html.EscapeString(s) }
