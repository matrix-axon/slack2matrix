// Package importer drives the migration: users, space, rooms and events.
package importer

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"slack2matrix/internal/config"
	"slack2matrix/internal/convert"
	"slack2matrix/internal/directory"
	"slack2matrix/internal/e2ee"
	"slack2matrix/internal/matrix"
	"slack2matrix/internal/slackexport"
	"slack2matrix/internal/store"
	"slack2matrix/internal/transform"
)

// Importer holds everything needed to migrate the export.
type Importer struct {
	Cfg   *config.Config
	Exp   *slackexport.Export
	Dir   *directory.Directory
	Tr    *transform.Transformer
	Store *store.Store
	MX    *matrix.Matrix
	E2EE  *e2ee.Manager
	Log   zerolog.Logger

	httpClient *http.Client

	roomsMu sync.RWMutex
	rooms   map[string]id.RoomID

	profilesMu sync.Mutex
	profiles   map[id.UserID]profile

	ghostsMu    sync.Mutex
	ghostsReady map[id.UserID]bool

	// roomVersion is the homeserver's default, used for every created room.
	roomVersion id.RoomVersion
}

type profile struct {
	displayName string
	avatar      string
}

// New wires up an importer. The transformer's converter is pointed at the
// importer so channel mentions link to imported rooms.
func New(ctx context.Context, cfg *config.Config, exp *slackexport.Export, dir *directory.Directory, st *store.Store, mx *matrix.Matrix, log zerolog.Logger) (*Importer, error) {
	imp := &Importer{
		Cfg:         cfg,
		Exp:         exp,
		Dir:         dir,
		Store:       st,
		MX:          mx,
		Log:         log,
		httpClient:  &http.Client{Timeout: 2 * time.Minute},
		profiles:    map[id.UserID]profile{},
		ghostsReady: map[id.UserID]bool{},
	}
	imp.Tr = &transform.Transformer{
		Conv: &convert.Converter{Resolver: imp, ServerName: cfg.Homeserver.ServerName},
		Dir:  dir,
		Exp:  exp,
		Opts: transform.Options{
			UnfurlCards:        cfg.Import.Unfurls == "card",
			LinkPreviews:       cfg.Import.LinkPreviews,
			FetchCardImages:    cfg.Import.FetchCardImages,
			MarkEdited:         cfg.Import.MarkEdited,
			SnippetInlineLimit: 16 * 1024,
		},
	}
	caps, err := mx.Bot().Capabilities(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading homeserver capabilities: %w", err)
	}
	if caps.RoomVersions != nil && caps.RoomVersions.Default != "" {
		imp.roomVersion = id.RoomVersion(caps.RoomVersions.Default)
	}
	if !imp.roomVersion.RestrictedJoins() {
		return nil, fmt.Errorf("homeserver default room version %q doesn't support restricted joins (needs 8 or newer)", imp.roomVersion)
	}
	rooms, err := st.Rooms(ctx)
	if err != nil {
		return nil, err
	}
	imp.rooms = rooms
	mgr, err := e2ee.New(ctx, cfg, mx, st, log, imp.Recipients())
	if err != nil {
		return nil, err
	}
	imp.E2EE = mgr
	return imp, nil
}

// Recipients are the real Matrix users who will read the history: mapped
// Slack users plus configured admins.
func (imp *Importer) Recipients() []id.UserID {
	var out []id.UserID
	for _, ident := range imp.Dir.RealUsers() {
		out = append(out, ident.MXID)
	}
	for _, a := range imp.Cfg.Admins {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	slices.Sort(out)
	return out
}

// User implements convert.Resolver.
func (imp *Importer) User(slackID string) *directory.Identity { return imp.Dir.User(slackID) }

// Channel implements convert.Resolver.
func (imp *Importer) Channel(slackID string) (string, id.RoomID) {
	c := imp.Exp.Channel(slackID)
	if c == nil {
		return "", ""
	}
	imp.roomsMu.RLock()
	defer imp.roomsMu.RUnlock()
	return c.Name, imp.rooms[slackID]
}

// Message implements convert.Resolver.
func (imp *Importer) Message(channelID, ts string) id.EventID {
	evt, err := imp.Store.FirstEvent(context.Background(), channelID, ts)
	if err != nil {
		imp.Log.Warn().Err(err).Str("channel", channelID).Str("ts", ts).Msg("Looking up linked message failed")
	}
	return evt
}

func (imp *Importer) room(channelID string) id.RoomID {
	imp.roomsMu.RLock()
	defer imp.roomsMu.RUnlock()
	return imp.rooms[channelID]
}

// SelectChannels resolves channel names (or all channels if names is empty).
func (imp *Importer) SelectChannels(names []string) ([]*slackexport.Channel, error) {
	if len(names) == 0 {
		return imp.Exp.Channels, nil
	}
	var out []*slackexport.Channel
	for _, n := range names {
		c := imp.Exp.ChannelByName(n)
		if c == nil {
			return nil, fmt.Errorf("no channel named %q", n)
		}
		out = append(out, c)
	}
	return out, nil
}
