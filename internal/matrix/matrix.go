// Package matrix wraps the appservice connection to the homeserver.
package matrix

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"slack2matrix/internal/config"
)

// Matrix is the importer's appservice connection.
type Matrix struct {
	AS  *appservice.AppService
	Cfg *config.Config
	Log zerolog.Logger
}

// New connects to the homeserver as the configured appservice.
func New(ctx context.Context, cfg *config.Config, log zerolog.Logger) (*Matrix, error) {
	reg := &appservice.Registration{
		ID:              cfg.AppService.ID,
		AppToken:        cfg.AppService.ASToken,
		ServerToken:     cfg.AppService.HSToken,
		SenderLocalpart: cfg.AppService.SenderLocalpart,
		RateLimited:     new(bool),
	}
	as, err := appservice.CreateFull(appservice.CreateOpts{
		Registration:     reg,
		HomeserverDomain: cfg.Homeserver.ServerName,
		HomeserverURL:    cfg.Homeserver.URL,
	})
	if err != nil {
		return nil, err
	}
	as.Log = log
	// Large media uploads need more than the default timeout.
	as.HTTPClient.Timeout = 30 * time.Minute
	m := &Matrix{AS: as, Cfg: cfg, Log: log}
	if _, err := as.BotIntent().Whoami(ctx); err != nil {
		return nil, fmt.Errorf("appservice token rejected by %s (is the registration installed?): %w", cfg.Homeserver.URL, err)
	}
	return m, nil
}

// Bot returns the appservice bot's intent.
func (m *Matrix) Bot() *appservice.IntentAPI { return m.AS.BotIntent() }

// Client returns the client acting as a user. For accounts with an importer
// device, the client also acts as that device.
func (m *Matrix) Client(user id.UserID) *mautrix.Client { return m.AS.Client(user) }

// EnsureGhost registers a ghost user if needed.
func (m *Matrix) EnsureGhost(ctx context.Context, user id.UserID) error {
	return m.AS.Intent(user).EnsureRegistered(ctx)
}

// CheckUserExists confirms a real account is registered and the appservice may
// act as it.
func (m *Matrix) CheckUserExists(ctx context.Context, user id.UserID) error {
	resp, err := m.AS.Client(user).Whoami(ctx)
	if err != nil {
		var httpErr mautrix.HTTPError
		if errors.As(err, &httpErr) && httpErr.RespError != nil && httpErr.RespError.ErrCode == "M_FORBIDDEN" {
			return fmt.Errorf("%s is not in the appservice's user namespace; regenerate the registration and restart Synapse", user)
		}
		return fmt.Errorf("checking %s: %w", user, err)
	}
	if resp.UserID != user {
		return fmt.Errorf("whoami for %s returned %s", user, resp.UserID)
	}
	return nil
}

// SendState sends a state event as a user with a massaged timestamp.
func (m *Matrix) SendState(ctx context.Context, as id.UserID, room id.RoomID, evtType event.Type, stateKey string, content any, ts int64) (id.EventID, error) {
	resp, err := m.Client(as).SendStateEvent(ctx, room, evtType, stateKey, content, mautrix.ReqSendEvent{Timestamp: ts})
	if err != nil {
		return "", err
	}
	return resp.EventID, nil
}

// SetMembership changes a user's membership via a timestamped state event.
// Invites are sent by inviter; joins and leaves by the user themselves.
func (m *Matrix) SetMembership(ctx context.Context, room id.RoomID, inviter, user id.UserID, membership event.Membership, displayName, avatar string, ts int64) error {
	content := &event.MemberEventContent{Membership: membership}
	if membership == event.MembershipJoin {
		content.Displayname = displayName
		content.AvatarURL = id.ContentURIString(avatar)
	}
	sender := user
	if membership == event.MembershipInvite {
		sender = inviter
	}
	_, err := m.SendState(ctx, sender, room, event.StateMember, string(user), content, ts)
	return err
}

// IsNotFound reports whether err is a 404 from the homeserver.
func IsNotFound(err error) bool {
	var httpErr mautrix.HTTPError
	return errors.As(err, &httpErr) && httpErr.Response != nil && httpErr.Response.StatusCode == http.StatusNotFound
}
