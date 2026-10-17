package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"slack2matrix/internal/config"
)

const configTemplate = `# slack2matrix configuration. Relative paths are resolved against this file.
export_dir: ./data
# media_dir: ./data/media
state_dir: ./state
# Scratch space for thumbnails; keep it off small tmpfs mounts.
tmp_dir: ./state/tmp

homeserver:
  url: http://localhost:8008
  server_name: matrix.example.com

appservice:
  id: slack-import
  as_token: %s
  hs_token: %s
  sender_localpart: slackimport
  # Must not overlap another appservice's namespace (mautrix-slack uses slack_).
  ghost_prefix: slackimport_

# Slack user ID (or Slack username) -> existing Matrix account. Anyone not
# listed becomes a ghost user such as @slackimport_<name>:server.
users: {}
#  U0255AU2Y: "@adam:matrix.example.com"

# Matrix users who get admin rights in the space and every room.
admins: []

space:
  name: Slack import
  topic: ""
  room_name_prefix: ""

import:
  unfurls: skip            # skip | card (quoted text card)
  link_previews: false     # embed Slack's link previews (MSC4095, no images)
  fetch_card_images: true
  mark_edited: false
  room_concurrency: 4
  media_concurrency: 8

crypto:
  pickle_key: %s
  device_id: SLACKIMPORT
  device_name: Slack import
  # Real user -> file containing their recovery key (to cross-sign the import device).
  recovery_keys: {}
  key_export_file: ./state/megolm-keys.txt
  key_export_passphrase_file: ./state/megolm-keys.passphrase
`

func runInit(_ context.Context, _ *config.Config, configPath string, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	force := fs.Bool("force", false, "overwrite an existing config file")
	_ = fs.Parse(args)
	if _, err := os.Stat(configPath); err == nil && !*force {
		return fmt.Errorf("%s already exists (use -force to overwrite)", configPath)
	}
	content := fmt.Sprintf(configTemplate, config.RandomToken(), config.RandomToken(), config.RandomToken())
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s; edit it, then run `slack2matrix registration` to generate the appservice registration\n", configPath)
	return nil
}

// registration is the Synapse appservice registration file format.
type registration struct {
	ID              string                     `yaml:"id"`
	URL             *string                    `yaml:"url"`
	ASToken         string                     `yaml:"as_token"`
	HSToken         string                     `yaml:"hs_token"`
	SenderLocalpart string                     `yaml:"sender_localpart"`
	RateLimited     bool                       `yaml:"rate_limited"`
	Namespaces      map[string][]namespaceRule `yaml:"namespaces"`
	MSC4190         bool                       `yaml:"io.element.msc4190"`
}

type namespaceRule struct {
	Exclusive bool   `yaml:"exclusive"`
	Regex     string `yaml:"regex"`
}

func buildRegistration(cfg *config.Config) *registration {
	server := regexp.QuoteMeta(cfg.Homeserver.ServerName)
	users := []namespaceRule{{
		Exclusive: true,
		Regex:     "^@" + regexp.QuoteMeta(cfg.AppService.GhostPrefix) + ".*:" + server + "$",
	}}
	// Real accounts are claimed non-exclusively so the appservice may act as
	// them without preventing their owners from logging in.
	var real []string
	for _, mxid := range cfg.Users {
		if !slices.Contains(real, string(mxid)) {
			real = append(real, string(mxid))
		}
	}
	slices.Sort(real)
	for _, mxid := range real {
		users = append(users, namespaceRule{Exclusive: false, Regex: "^" + regexp.QuoteMeta(mxid) + "$"})
	}
	return &registration{
		ID:              cfg.AppService.ID,
		ASToken:         cfg.AppService.ASToken,
		HSToken:         cfg.AppService.HSToken,
		SenderLocalpart: cfg.AppService.SenderLocalpart,
		RateLimited:     false,
		MSC4190:         true,
		Namespaces: map[string][]namespaceRule{
			"users":   users,
			"aliases": {},
			"rooms":   {},
		},
	}
}

func runRegistration(_ context.Context, cfg *config.Config, _ string, args []string) error {
	fs := flag.NewFlagSet("registration", flag.ExitOnError)
	out := fs.String("out", "slack-import-registration.yaml", "where to write the registration file")
	_ = fs.Parse(args)
	if cfg.AppService.ASToken == "" || cfg.AppService.HSToken == "" {
		return fmt.Errorf("appservice.as_token and appservice.hs_token must be set")
	}
	data, err := yaml.Marshal(buildRegistration(cfg))
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, data, 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n\nAdd it to homeserver.yaml and restart Synapse:\n\n", *out)
	fmt.Println(strings.TrimSpace(`
app_service_config_files:
  - /path/to/` + *out + `
max_upload_size: 1G
`))
	return nil
}
