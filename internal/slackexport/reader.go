package slackexport

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/text/encoding/charmap"
)

// Export is an opened Slack export directory.
type Export struct {
	Dir      string
	MediaDir string
	Users    []*User
	Channels []*Channel
	Canvases []*Canvas

	usersByID map[string]*User
	chanByID  map[string]*Channel
}

// Open loads the workspace metadata from an export directory. If mediaDir is
// empty it defaults to <dir>/media.
func Open(dir, mediaDir string) (*Export, error) {
	if mediaDir == "" {
		mediaDir = filepath.Join(dir, "media")
	}
	e := &Export{Dir: dir, MediaDir: mediaDir}
	if err := readJSON(filepath.Join(dir, "users.json"), &e.Users); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(dir, "channels.json"), &e.Channels); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(dir, "canvases.json"), &e.Canvases); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	e.usersByID = make(map[string]*User, len(e.Users))
	for _, u := range e.Users {
		e.usersByID[u.ID] = u
	}
	e.chanByID = make(map[string]*Channel, len(e.Channels))
	for _, c := range e.Channels {
		e.chanByID[c.ID] = c
	}
	return e, nil
}

func readJSON(path string, into any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(into); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	return nil
}

func (e *Export) User(id string) *User       { return e.usersByID[id] }
func (e *Export) Channel(id string) *Channel { return e.chanByID[id] }

// ChannelByName finds a channel by its Slack name.
func (e *Export) ChannelByName(name string) *Channel {
	for _, c := range e.Channels {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// ChannelDir locates the directory holding a channel's day files. Some export
// tools write non-ASCII names in Latin-1 rather than UTF-8, so if the UTF-8
// name doesn't exist the Latin-1 encoding is tried too.
func (e *Export) ChannelDir(c *Channel) (string, error) {
	candidates := []string{c.Name}
	if latin1, err := charmap.ISO8859_1.NewEncoder().String(c.Name); err == nil && latin1 != c.Name {
		candidates = append(candidates, latin1)
	}
	for _, name := range candidates {
		p := filepath.Join(e.Dir, name)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no directory found for channel #%s (%s)", c.Name, c.ID)
}

// Messages reads every day file of a channel and returns its messages sorted
// chronologically. Duplicate timestamps (which can occur when a message is
// repeated across day files) are collapsed, keeping the first copy.
func (e *Export) Messages(c *Channel) ([]*Message, error) {
	dir, err := e.ChannelDir(c)
	if err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	seen := make(map[TS]bool)
	var msgs []*Message
	for _, f := range files {
		// Skip dotfiles such as macOS "._2014-02-14.json" AppleDouble files.
		if strings.HasPrefix(filepath.Base(f), ".") {
			continue
		}
		var day []*Message
		if err := readJSON(f, &day); err != nil {
			return nil, err
		}
		for _, m := range day {
			if m.TS == "" || seen[m.TS] {
				continue
			}
			seen[m.TS] = true
			msgs = append(msgs, m)
		}
	}
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].TS.Less(msgs[j].TS) })
	return msgs, nil
}

// ChannelSize returns the total size of a channel's day files, a cheap proxy
// for how long it takes to import.
func (e *Export) ChannelSize(c *Channel) int64 {
	dir, err := e.ChannelDir(c)
	if err != nil {
		return 0
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var total int64
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			total += st.Size()
		}
	}
	return total
}

// LocalPath maps a Slack file URL onto the mirrored copy under MediaDir. The
// mirror keeps the host and path and appends the query string to the file
// name, e.g. media/files.slack.com/files-pri/T-F/download/a.png?t=xoxe-...
func (e *Export) LocalPath(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	// The mirror stores the URL verbatim (still percent-escaped), so the path
	// is built from the raw string rather than a parsed and unescaped URL.
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	_, rest, _ := strings.Cut(rawURL, "://")
	return filepath.Join(e.MediaDir, filepath.FromSlash(rest))
}

// FilePath returns the local path of a file's content, or "" if the file has no
// downloadable content or is missing on disk.
func (e *Export) FilePath(f *File) string {
	for _, u := range []string{f.URLPrivateDownload, f.URLPrivate} {
		p := e.LocalPath(u)
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// IsHostedSlackFile reports whether a URL points at Slack's own file storage.
func IsHostedSlackFile(rawURL string) bool {
	return strings.HasPrefix(rawURL, "https://files.slack.com/")
}
