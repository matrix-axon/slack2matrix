// Package config loads the importer's YAML configuration.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/id"
)

type Config struct {
	// ExportDir is the unpacked Slack export (users.json, channels.json, ...).
	ExportDir string `yaml:"export_dir"`
	// MediaDir holds the mirrored file downloads; defaults to <export_dir>/media.
	MediaDir string `yaml:"media_dir"`
	// StateDir holds the importer's SQLite databases and key material.
	StateDir string `yaml:"state_dir"`
	// TmpDir is used for thumbnails and transcoding scratch files.
	TmpDir string `yaml:"tmp_dir"`

	Homeserver Homeserver `yaml:"homeserver"`
	AppService AppService `yaml:"appservice"`

	// Users maps Slack user IDs (or names) to existing Matrix accounts. Slack
	// users not listed here are imported as ghost users.
	Users map[string]id.UserID `yaml:"users"`
	// Admins get power level 100 in the space and every room and are invited.
	Admins []id.UserID `yaml:"admins"`

	Space  Space  `yaml:"space"`
	Import Import `yaml:"import"`
	Crypto Crypto `yaml:"crypto"`
}

type Homeserver struct {
	URL        string `yaml:"url"`
	ServerName string `yaml:"server_name"`
}

type AppService struct {
	ID              string `yaml:"id"`
	ASToken         string `yaml:"as_token"`
	HSToken         string `yaml:"hs_token"`
	SenderLocalpart string `yaml:"sender_localpart"`
	// GhostPrefix is prepended to sanitized Slack names to form ghost localparts.
	GhostPrefix string `yaml:"ghost_prefix"`
}

type Space struct {
	Name   string `yaml:"name"`
	Topic  string `yaml:"topic"`
	Avatar string `yaml:"avatar"`
	// RoomNamePrefix is prepended to every channel name, e.g. "#".
	RoomNamePrefix string `yaml:"room_name_prefix"`
}

type Import struct {
	// Unfurls is "skip" (default) or "card" for Slack link previews.
	Unfurls string `yaml:"unfurls"`
	// FetchCardImages downloads images referenced by bot/app cards and re-hosts them.
	FetchCardImages bool `yaml:"fetch_card_images"`
	// MarkEdited appends "(edited)" to messages that were edited in Slack.
	MarkEdited       bool `yaml:"mark_edited"`
	RoomConcurrency  int  `yaml:"room_concurrency"`
	MediaConcurrency int  `yaml:"media_concurrency"`
}

type Crypto struct {
	// PickleKey encrypts Olm account and session pickles in the crypto DB.
	PickleKey string `yaml:"pickle_key"`
	// RecoveryKeys maps real Matrix users to a file holding their recovery key,
	// used to cross-sign the importer's device for that user.
	RecoveryKeys map[id.UserID]string `yaml:"recovery_keys"`
	// KeyExportFile is where the Element-compatible megolm key export is written.
	KeyExportFile string `yaml:"key_export_file"`
	// KeyExportPassphraseFile holds the passphrase protecting the key export.
	KeyExportPassphraseFile string `yaml:"key_export_passphrase_file"`
	// DeviceID is the device created on every account the importer acts as.
	DeviceID id.DeviceID `yaml:"device_id"`
	// DeviceName is the human-readable name of that device.
	DeviceName string `yaml:"device_name"`
}

// Load reads and validates a config file. Relative paths are resolved against
// the config file's directory.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	c.setDefaults()
	base := filepath.Dir(path)
	for _, p := range []*string{&c.ExportDir, &c.MediaDir, &c.StateDir, &c.TmpDir, &c.Crypto.KeyExportFile, &c.Crypto.KeyExportPassphraseFile, &c.Space.Avatar} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	for user, p := range c.Crypto.RecoveryKeys {
		if p != "" && !filepath.IsAbs(p) {
			c.Crypto.RecoveryKeys[user] = filepath.Join(base, p)
		}
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) setDefaults() {
	if c.MediaDir == "" && c.ExportDir != "" {
		c.MediaDir = filepath.Join(c.ExportDir, "media")
	}
	if c.StateDir == "" {
		c.StateDir = "state"
	}
	if c.TmpDir == "" {
		// Not os.TempDir(): /tmp is often a small tmpfs and video thumbnails
		// can need a lot of scratch space.
		c.TmpDir = filepath.Join(c.StateDir, "tmp")
	}
	if c.AppService.ID == "" {
		c.AppService.ID = "slack-import"
	}
	if c.AppService.SenderLocalpart == "" {
		c.AppService.SenderLocalpart = "slackimport"
	}
	if c.AppService.GhostPrefix == "" {
		// Not "slack_": that's the mautrix-slack bridge's default namespace.
		c.AppService.GhostPrefix = "slackimport_"
	}
	if c.Import.Unfurls == "" {
		c.Import.Unfurls = "skip"
	}
	if c.Import.RoomConcurrency <= 0 {
		c.Import.RoomConcurrency = 4
	}
	if c.Import.MediaConcurrency <= 0 {
		c.Import.MediaConcurrency = 8
	}
	if c.Crypto.DeviceID == "" {
		c.Crypto.DeviceID = "SLACKIMPORT"
	}
	if c.Crypto.DeviceName == "" {
		c.Crypto.DeviceName = "Slack import"
	}
}

func (c *Config) validate() error {
	var missing []string
	if c.ExportDir == "" {
		missing = append(missing, "export_dir")
	}
	if c.Homeserver.URL == "" {
		missing = append(missing, "homeserver.url")
	}
	if c.Homeserver.ServerName == "" {
		missing = append(missing, "homeserver.server_name")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config is missing required fields: %s", strings.Join(missing, ", "))
	}
	if c.Import.Unfurls != "skip" && c.Import.Unfurls != "card" {
		return fmt.Errorf("import.unfurls must be \"skip\" or \"card\", not %q", c.Import.Unfurls)
	}
	for slackID, mxid := range c.Users {
		if _, server, err := mxid.Parse(); err != nil {
			return fmt.Errorf("users.%s: invalid Matrix ID %q: %w", slackID, mxid, err)
		} else if server != c.Homeserver.ServerName {
			return fmt.Errorf("users.%s: %s is not on %s; only local accounts can be imported into", slackID, mxid, c.Homeserver.ServerName)
		}
	}
	return nil
}

// BotUserID is the appservice's own sender user.
func (c *Config) BotUserID() id.UserID {
	return id.NewUserID(c.AppService.SenderLocalpart, c.Homeserver.ServerName)
}

// RandomToken returns a random hex token suitable for as_token/hs_token.
func RandomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
