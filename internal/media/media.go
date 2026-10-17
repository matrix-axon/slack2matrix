// Package media inspects files and makes the derived files Matrix clients
// need: web-displayable images and thumbnails. Because imported rooms are
// encrypted, the homeserver can't generate thumbnails, so the importer must.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"

	_ "golang.org/x/image/webp"
)

// Thumbnail limits, matching what Element generates for its own uploads.
const (
	thumbMaxW = 800
	thumbMaxH = 600
	// Images smaller than this (in bytes and pixels) are shown directly.
	thumbMinBytes = 512 << 10
)

// File is a file ready for upload.
type File struct {
	Path       string
	Mimetype   string
	Size       int64
	Width      int
	Height     int
	DurationMS int64
}

// Prepared is an upload plus its optional thumbnail.
type Prepared struct {
	File
	Thumbnail *File
	tmpFiles  []string
}

// Cleanup removes derived temporary files.
func (p *Prepared) Cleanup() {
	for _, f := range p.tmpFiles {
		os.Remove(f)
	}
}

// webImage lists image types browsers can display.
var webImage = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true,
}

// Prepare inspects a file and creates any conversions and thumbnails.
// Images browsers can't show (HEIC, TIFF, ...) are converted to JPEG.
func Prepare(ctx context.Context, path, name, mimetype, tmpDir string) (*Prepared, error) {
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	p := &Prepared{File: File{Path: path, Mimetype: mimetype, Size: st.Size()}}
	if p.Mimetype == "" || p.Mimetype == "application/octet-stream" {
		p.Mimetype = sniff(path)
	}
	switch {
	case strings.HasPrefix(p.Mimetype, "image/"):
		err = p.prepareImage(ctx, tmpDir)
	case strings.HasPrefix(p.Mimetype, "video/"):
		err = p.prepareVideo(ctx, tmpDir)
	case strings.HasPrefix(p.Mimetype, "audio/"):
		if d, _, _, perr := probe(ctx, path); perr == nil {
			p.DurationMS = d
		}
	}
	if err != nil {
		p.Cleanup()
		return nil, err
	}
	return p, nil
}

func sniff(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := io.ReadFull(f, buf)
	ct := http.DetectContentType(buf[:n])
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return ct
}

func (p *Prepared) tmp(tmpDir, pattern string) (string, error) {
	f, err := os.CreateTemp(tmpDir, pattern)
	if err != nil {
		return "", err
	}
	f.Close()
	p.tmpFiles = append(p.tmpFiles, f.Name())
	return f.Name(), nil
}

func (p *Prepared) prepareImage(ctx context.Context, tmpDir string) error {
	if !webImage[p.Mimetype] {
		// Convert to JPEG so every client can display it.
		out, err := p.tmp(tmpDir, "convert-*.jpg")
		if err != nil {
			return err
		}
		if err := magick(ctx, p.Path+"[0]", "-auto-orient", "-quality", "90", "jpeg:"+out); err != nil {
			// Not convertible: leave it as a plain file upload.
			p.Mimetype = "application/octet-stream"
			return nil
		}
		st, err := os.Stat(out)
		if err != nil {
			return err
		}
		p.Path, p.Mimetype, p.Size = out, "image/jpeg", st.Size()
	}
	w, h, rotated, err := imageSize(ctx, p.Path)
	if err != nil {
		// Unreadable image: send it, just without dimensions.
		return nil
	}
	if rotated {
		w, h = h, w
	}
	p.Width, p.Height = w, h
	if w <= thumbMaxW && h <= thumbMaxH && p.Size < thumbMinBytes {
		return nil
	}
	thumb, err := p.tmp(tmpDir, "thumb-*")
	if err != nil {
		return err
	}
	format, mime := "jpeg", "image/jpeg"
	if p.Mimetype == "image/png" || p.Mimetype == "image/gif" {
		// Keep transparency.
		format, mime = "png", "image/png"
	}
	if err := magick(ctx, p.Path+"[0]", "-auto-orient", "-thumbnail", fmt.Sprintf("%dx%d>", thumbMaxW, thumbMaxH),
		"-strip", "-quality", "85", format+":"+thumb); err != nil {
		return nil
	}
	return p.setThumb(ctx, thumb, mime)
}

func (p *Prepared) setThumb(ctx context.Context, path, mime string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	tw, th, _, err := imageSize(ctx, path)
	if err != nil {
		return nil
	}
	p.Thumbnail = &File{Path: path, Mimetype: mime, Size: st.Size(), Width: tw, Height: th}
	return nil
}

// imageSize returns stored dimensions and whether EXIF orientation rotates
// the image by 90° (so displayed width and height are swapped).
func imageSize(ctx context.Context, path string) (w, h int, rotated bool, err error) {
	if f, ferr := os.Open(path); ferr == nil {
		cfg, _, derr := image.DecodeConfig(f)
		f.Close()
		if derr == nil {
			w, h = cfg.Width, cfg.Height
		}
	}
	out, err := exec.CommandContext(ctx, "magick", "identify", "-format", "%w %h %[orientation]\n", path+"[0]").Output()
	if err != nil {
		if w > 0 {
			return w, h, false, nil
		}
		return 0, 0, false, err
	}
	fields := strings.Fields(strings.SplitN(string(out), "\n", 2)[0])
	if len(fields) >= 2 && w == 0 {
		w, _ = strconv.Atoi(fields[0])
		h, _ = strconv.Atoi(fields[1])
	}
	if len(fields) >= 3 {
		switch fields[2] {
		case "LeftTop", "RightTop", "RightBottom", "LeftBottom":
			rotated = true
		}
	}
	return w, h, rotated, nil
}

func magick(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "magick", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("magick: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (p *Prepared) prepareVideo(ctx context.Context, tmpDir string) error {
	dur, w, h, err := probe(ctx, p.Path)
	if err == nil {
		p.DurationMS, p.Width, p.Height = dur, w, h
	}
	thumb, err := p.tmp(tmpDir, "poster-*.jpg")
	if err != nil {
		return err
	}
	scale := fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease", thumbMaxW, thumbMaxH)
	for _, seek := range []string{"1", "0"} {
		cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-y", "-ss", seek, "-i", p.Path,
			"-frames:v", "1", "-vf", scale, "-f", "image2", "-c:v", "mjpeg", thumb)
		if cmd.Run() == nil {
			if st, err := os.Stat(thumb); err == nil && st.Size() > 0 {
				return p.setThumb(ctx, thumb, "image/jpeg")
			}
		}
	}
	return nil
}

// probe returns duration (ms) and displayed video dimensions via ffprobe.
func probe(ctx context.Context, path string) (durationMS int64, w, h int, err error) {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-print_format", "json",
		"-show_format", "-show_streams", path).Output()
	if err != nil {
		return 0, 0, 0, err
	}
	var res struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType string            `json:"codec_type"`
			Width     int               `json:"width"`
			Height    int               `json:"height"`
			Tags      map[string]string `json:"tags"`
			SideData  []struct {
				Rotation float64 `json:"rotation"`
			} `json:"side_data_list"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return 0, 0, 0, err
	}
	if d, err := strconv.ParseFloat(res.Format.Duration, 64); err == nil {
		durationMS = int64(d * 1000)
	}
	for _, s := range res.Streams {
		if s.CodecType != "video" {
			continue
		}
		w, h = s.Width, s.Height
		rot := 0.0
		if r, err := strconv.ParseFloat(s.Tags["rotate"], 64); err == nil {
			rot = r
		}
		for _, sd := range s.SideData {
			if sd.Rotation != 0 {
				rot = sd.Rotation
			}
		}
		if int(rot)%180 != 0 {
			w, h = h, w
		}
		break
	}
	return durationMS, w, h, nil
}
