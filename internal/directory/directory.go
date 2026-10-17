// Package directory decides which Matrix user each Slack user or bot becomes.
package directory

import (
	"fmt"
	"sort"
	"strings"

	"maunium.net/go/mautrix/id"

	"slack2matrix/internal/config"
	"slack2matrix/internal/slackexport"
)

// Identity is one Matrix account that imported events are sent as.
type Identity struct {
	MXID        id.UserID
	DisplayName string
	// AvatarURL is the remote (Slack CDN) avatar to copy, if any.
	AvatarURL string
	// Real accounts already exist on the homeserver and belong to people; the
	// importer must never change their profile or cross-signing identity.
	Real bool
	Bot  bool
}

// Directory resolves Slack senders and mentions to Matrix identities. Its
// output depends only on the export and config, so ghost IDs are stable
// across runs.
type Directory struct {
	cfg        *config.Config
	exp        *slackexport.Export
	byMXID     map[id.UserID]*Identity
	bySlackID  map[string]*Identity
	botNames   map[string]string
	botAvatars map[string]string
}

const slackbotID = "USLACKBOT"

// Build constructs the directory. scan must call its argument for every
// message in the export; it's used to name bots. Pass nil to skip scanning.
func Build(cfg *config.Config, exp *slackexport.Export, scan func(func(*slackexport.Message)) error) (*Directory, error) {
	d := &Directory{
		cfg:        cfg,
		exp:        exp,
		byMXID:     map[id.UserID]*Identity{},
		bySlackID:  map[string]*Identity{},
		botNames:   map[string]string{},
		botAvatars: map[string]string{},
	}
	for slackKey := range cfg.Users {
		if exp.User(slackKey) == nil && userByName(exp, slackKey) == nil {
			return nil, fmt.Errorf("users: %q is neither a Slack user ID nor a username in users.json", slackKey)
		}
	}
	for _, u := range exp.Users {
		d.addSlackUser(u)
	}
	if scan != nil {
		if err := d.scanBots(scan); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func userByName(exp *slackexport.Export, name string) *slackexport.User {
	for _, u := range exp.Users {
		if u.Name == name {
			return u
		}
	}
	return nil
}

func (d *Directory) addSlackUser(u *slackexport.User) *Identity {
	if ident, ok := d.bySlackID[u.ID]; ok {
		return ident
	}
	var ident *Identity
	if mxid, ok := d.cfg.Users[u.ID]; ok {
		ident = d.real(mxid)
	} else if mxid, ok := d.cfg.Users[u.Name]; ok {
		ident = d.real(mxid)
	} else {
		ident = d.ghost(u.Name, u.DisplayName(), u.Profile.BestImage(), u.IsBot)
	}
	if ident.DisplayName == "" {
		ident.DisplayName = u.DisplayName()
	}
	d.bySlackID[u.ID] = ident
	if u.Profile.BotID != "" {
		d.bySlackID[u.Profile.BotID] = ident
	}
	return ident
}

func (d *Directory) real(mxid id.UserID) *Identity {
	if ident, ok := d.byMXID[mxid]; ok {
		return ident
	}
	ident := &Identity{MXID: mxid, Real: true}
	d.byMXID[mxid] = ident
	return ident
}

// ghost returns the ghost for a Slack name, creating it on first use. Ghosts
// are keyed by sanitized name, so bots that share a name share a ghost.
func (d *Directory) ghost(name, displayName, avatar string, bot bool) *Identity {
	mxid := id.NewUserID(d.cfg.AppService.GhostPrefix+Localpart(name), d.cfg.Homeserver.ServerName)
	if ident, ok := d.byMXID[mxid]; ok {
		if ident.AvatarURL == "" {
			ident.AvatarURL = avatar
		}
		return ident
	}
	ident := &Identity{MXID: mxid, DisplayName: displayName, AvatarURL: avatar, Bot: bot}
	d.byMXID[mxid] = ident
	return ident
}

// Localpart sanitizes a Slack name into a valid, lowercase Matrix localpart.
func Localpart(name string) string {
	var sb strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '=':
			sb.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				sb.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	out := strings.Trim(sb.String(), "_")
	if out == "" {
		out = "unknown"
	}
	return out
}

// scanBots collects a display name and avatar for each bot ID. Slack exports
// don't include bot metadata, so names come from message usernames, bot
// profiles or, failing that, the service named in the bot's attachments.
func (d *Directory) scanBots(scan func(func(*slackexport.Message)) error) error {
	services := map[string]map[string]int{}
	var botIDs []string
	err := scan(func(m *slackexport.Message) {
		if m.User != "" {
			return
		}
		if m.Username != "" {
			// Register named bot ghosts now so setup can create them.
			d.Sender(m)
			return
		}
		if m.BotID == "" {
			return
		}
		if _, seen := services[m.BotID]; !seen {
			services[m.BotID] = map[string]int{}
			botIDs = append(botIDs, m.BotID)
		}
		if m.BotProfile != nil && m.BotProfile.Name != "" && d.botNames[m.BotID] == "" {
			d.botNames[m.BotID] = m.BotProfile.Name
		}
		if m.Icons != nil && d.botAvatars[m.BotID] == "" {
			d.botAvatars[m.BotID] = firstNonEmpty(m.Icons.Image72, m.Icons.Image64, m.Icons.Image48)
		}
		for _, a := range m.Attachments {
			if svc := firstNonEmpty(a.ServiceName, a.Footer); svc != "" {
				services[m.BotID][svc]++
			}
		}
	})
	if err != nil {
		return err
	}
	for botID, counts := range services {
		if d.botNames[botID] != "" {
			continue
		}
		best, bestN := "", 0
		for svc, n := range counts {
			if n > bestN || (n == bestN && svc < best) {
				best, bestN = svc, n
			}
		}
		d.botNames[botID] = displayCase(best)
	}
	for _, botID := range botIDs {
		d.Sender(&slackexport.Message{BotID: botID})
	}
	return nil
}

func displayCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// User resolves a Slack user ID (as used in mentions and reactions).
func (d *Directory) User(slackID string) *Identity {
	if ident, ok := d.bySlackID[slackID]; ok {
		return ident
	}
	if slackID == slackbotID {
		ident := d.ghost("slackbot", "Slackbot", "", true)
		d.bySlackID[slackID] = ident
		return ident
	}
	if slackID == "" {
		return nil
	}
	// A user missing from users.json, e.g. from another workspace.
	ident := d.ghost(slackID, slackID, "", false)
	d.bySlackID[slackID] = ident
	return ident
}

// Sender resolves who a message should be sent as.
func (d *Directory) Sender(m *slackexport.Message) *Identity {
	if m.User != "" {
		return d.User(m.User)
	}
	if m.Username != "" {
		avatar := ""
		if m.Icons != nil {
			avatar = firstNonEmpty(m.Icons.Image72, m.Icons.Image64, m.Icons.Image48)
		}
		return d.ghost(m.Username, m.Username, avatar, true)
	}
	if m.BotID != "" {
		if ident, ok := d.bySlackID[m.BotID]; ok {
			return ident
		}
		name := d.botNames[m.BotID]
		if name == "" {
			name = "Bot " + m.BotID
		}
		return d.ghost(name, name, d.botAvatars[m.BotID], true)
	}
	return d.User(slackbotID)
}

// All returns every identity known so far, sorted by Matrix ID.
func (d *Directory) All() []*Identity {
	out := make([]*Identity, 0, len(d.byMXID))
	for _, ident := range d.byMXID {
		out = append(out, ident)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MXID < out[j].MXID })
	return out
}

// RealUsers returns the identities that map to existing Matrix accounts.
func (d *Directory) RealUsers() []*Identity {
	var out []*Identity
	for _, ident := range d.All() {
		if ident.Real {
			out = append(out, ident)
		}
	}
	return out
}
