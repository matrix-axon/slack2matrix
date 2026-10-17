package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules"

	"slack2matrix/internal/directory"
	"slack2matrix/internal/slackexport"
)

// SetupUsers checks real accounts and registers ghosts with Slack profiles.
func (imp *Importer) SetupUsers(ctx context.Context) error {
	for _, mxid := range imp.Recipients() {
		if err := imp.MX.CheckUserExists(ctx, mxid); err != nil {
			return err
		}
	}
	for _, ident := range imp.Dir.All() {
		if err := imp.ensureIdentity(ctx, ident); err != nil {
			return err
		}
	}
	return nil
}

// ensureIdentity sets up a ghost the first time it's needed. Most ghosts are
// created by SetupUsers, but some (e.g. users only seen in reactions) are
// discovered during the import.
func (imp *Importer) ensureIdentity(ctx context.Context, ident *directory.Identity) error {
	if ident.Real {
		return nil
	}
	imp.ghostsMu.Lock()
	defer imp.ghostsMu.Unlock()
	if imp.ghostsReady[ident.MXID] {
		return nil
	}
	if err := imp.setupGhost(ctx, ident); err != nil {
		return fmt.Errorf("setting up ghost %s: %w", ident.MXID, err)
	}
	imp.ghostsReady[ident.MXID] = true
	return nil
}

func (imp *Importer) setupGhost(ctx context.Context, ident *directory.Identity) error {
	name, _, ok, err := imp.Store.Ghost(ctx, ident.MXID)
	if err != nil {
		return err
	}
	if ok && name == ident.DisplayName {
		return nil
	}
	if err := imp.MX.EnsureGhost(ctx, ident.MXID); err != nil {
		var httpErr mautrix.HTTPError
		if errors.As(err, &httpErr) && httpErr.RespError != nil && httpErr.RespError.ErrCode == "M_EXCLUSIVE" {
			return fmt.Errorf("%s is claimed by another appservice (e.g. a Slack bridge); choose a different appservice.ghost_prefix, regenerate the registration and restart Synapse", ident.MXID)
		}
		return err
	}
	intent := imp.MX.AS.Intent(ident.MXID)
	if err := intent.SetDisplayName(ctx, ident.DisplayName); err != nil {
		return err
	}
	var avatar id.ContentURI
	if ident.AvatarURL != "" {
		avatar, err = imp.uploadRemoteAvatar(ctx, ident.MXID, ident.AvatarURL)
		if err != nil {
			// Old Slack CDN links may be gone; a missing avatar isn't fatal.
			imp.Log.Warn().Err(err).Stringer("user", ident.MXID).Str("url", ident.AvatarURL).Msg("Couldn't copy avatar")
		} else if err := intent.SetAvatarURL(ctx, avatar); err != nil {
			return err
		}
	}
	imp.Log.Info().Stringer("user", ident.MXID).Str("name", ident.DisplayName).Msg("Ghost ready")
	return imp.Store.SetGhost(ctx, ident.MXID, ident.DisplayName, avatar.String())
}

// uploadRemoteAvatar copies a profile picture. Avatars are public profile
// data, so they are uploaded unencrypted like any client would.
func (imp *Importer) uploadRemoteAvatar(ctx context.Context, user id.UserID, url string) (id.ContentURI, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return id.ContentURI{}, err
	}
	resp, err := imp.httpClient.Do(req)
	if err != nil {
		return id.ContentURI{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return id.ContentURI{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return id.ContentURI{}, err
	}
	up, err := imp.MX.Client(user).UploadMedia(ctx, mautrix.ReqUploadMedia{
		ContentBytes: data,
		ContentType:  resp.Header.Get("Content-Type"),
		FileName:     "avatar",
	})
	if err != nil {
		return id.ContentURI{}, err
	}
	return up.ContentURI, nil
}

// profileOf returns the display name and avatar to put in a user's member
// events, mirroring what a normal join would copy from the global profile.
func (imp *Importer) profileOf(ctx context.Context, user id.UserID) profile {
	imp.profilesMu.Lock()
	defer imp.profilesMu.Unlock()
	if p, ok := imp.profiles[user]; ok {
		return p
	}
	var p profile
	if resp, err := imp.MX.Client(user).GetProfile(ctx, user); err == nil {
		p.displayName = resp.DisplayName
		p.avatar = resp.AvatarURL.String()
	}
	imp.profiles[user] = p
	return p
}

// EnsureSpace creates the parent space and joins the real users to it.
func (imp *Importer) EnsureSpace(ctx context.Context) (id.RoomID, error) {
	if s, err := imp.Store.Get(ctx, "space_room_id"); err != nil || s != "" {
		return id.RoomID(s), err
	}
	bot := imp.MX.Bot()
	resp, err := bot.CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Name:            imp.Cfg.Space.Name,
		Topic:           imp.Cfg.Space.Topic,
		Preset:          "private_chat",
		CreationContent: map[string]any{"type": event.RoomTypeSpace},
		RoomVersion:     imp.roomVersion,
		InitialState: []*event.Event{
			{Type: event.StateHistoryVisibility, Content: event.Content{Parsed: &event.HistoryVisibilityEventContent{HistoryVisibility: event.HistoryVisibilityShared}}},
			{Type: event.StateGuestAccess, Content: event.Content{Parsed: &event.GuestAccessEventContent{GuestAccess: event.GuestAccessForbidden}}},
		},
		PowerLevelOverride: imp.powerLevels(),
	})
	if err != nil {
		return "", fmt.Errorf("creating space: %w", err)
	}
	space := resp.RoomID
	if err := imp.Store.Set(ctx, "space_room_id", string(space)); err != nil {
		return "", err
	}
	imp.Log.Info().Stringer("space", space).Msg("Created space")
	for _, user := range imp.Recipients() {
		if err := imp.ensureJoined(ctx, space, user, 0); err != nil {
			return "", err
		}
	}
	return space, nil
}

// powerLevels grants admins full power. The bot creates every room: from
// room version 12 creators have unlimited power and must not appear in the
// power levels, while older versions require the creator to be listed.
func (imp *Importer) powerLevels() *event.PowerLevelsEventContent {
	users := map[id.UserID]int{}
	if !imp.roomVersion.PrivilegedRoomCreators() {
		users[imp.Cfg.BotUserID()] = 100
	}
	for _, a := range imp.Cfg.Admins {
		users[a] = 100
	}
	return &event.PowerLevelsEventContent{Users: users}
}

// roomName gives the Matrix room name for a channel.
func (imp *Importer) roomName(ch *slackexport.Channel) string {
	return imp.Cfg.Space.RoomNamePrefix + ch.Name
}

func channelTopic(ch *slackexport.Channel) string {
	topic := strings.TrimSpace(ch.Topic.Value)
	purpose := strings.TrimSpace(ch.Purpose.Value)
	switch {
	case topic != "" && purpose != "" && topic != purpose:
		return topic + " — " + purpose
	case topic != "":
		return topic
	}
	return purpose
}

// EnsureRoom creates the encrypted room for a channel inside the space and
// joins the real users who belong to it.
func (imp *Importer) EnsureRoom(ctx context.Context, space id.RoomID, ch *slackexport.Channel) (id.RoomID, error) {
	room, done, err := imp.Store.Room(ctx, ch.ID)
	if err != nil {
		return "", err
	}
	if room == "" {
		room, err = imp.createRoom(ctx, space, ch)
		if err != nil {
			return "", err
		}
	}
	if done {
		return room, nil
	}
	server := imp.Cfg.Homeserver.ServerName
	createdTS := ch.Created * 1000
	if _, err := imp.MX.SendState(ctx, imp.Cfg.BotUserID(), space, event.StateSpaceChild, string(room),
		&event.SpaceChildEventContent{Via: []string{server}}, createdTS); err != nil {
		return "", fmt.Errorf("adding %s to space: %w", room, err)
	}
	// Real users are members from the channel's creation so the room shows
	// up for them; ghosts join just before they first speak.
	for _, user := range imp.Recipients() {
		if err := imp.ensureJoined(ctx, room, user, createdTS); err != nil {
			return "", err
		}
		if err := imp.mute(ctx, room, user); err != nil {
			return "", err
		}
	}
	if err := imp.Store.SetRoomSetupDone(ctx, ch.ID); err != nil {
		return "", err
	}
	return room, nil
}

func (imp *Importer) createRoom(ctx context.Context, space id.RoomID, ch *slackexport.Channel) (id.RoomID, error) {
	server := imp.Cfg.Homeserver.ServerName
	resp, err := imp.MX.Bot().CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Name:        imp.roomName(ch),
		Topic:       channelTopic(ch),
		Preset:      "private_chat",
		RoomVersion: imp.roomVersion,
		InitialState: []*event.Event{
			{Type: event.StateEncryption, Content: event.Content{Parsed: &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}}},
			{Type: event.StateHistoryVisibility, Content: event.Content{Parsed: &event.HistoryVisibilityEventContent{HistoryVisibility: event.HistoryVisibilityShared}}},
			{Type: event.StateGuestAccess, Content: event.Content{Parsed: &event.GuestAccessEventContent{GuestAccess: event.GuestAccessForbidden}}},
			{Type: event.StateJoinRules, Content: event.Content{Parsed: &event.JoinRulesEventContent{
				JoinRule: event.JoinRuleRestricted,
				Allow:    []event.JoinRuleAllow{{Type: event.JoinRuleAllowRoomMembership, RoomID: space}},
			}}},
			{Type: event.StateSpaceParent, StateKey: ptr(string(space)), Content: event.Content{Parsed: &event.SpaceParentEventContent{Via: []string{server}, Canonical: true}}},
		},
		PowerLevelOverride: imp.powerLevels(),
	})
	if err != nil {
		return "", fmt.Errorf("creating room for #%s: %w", ch.Name, err)
	}
	if err := imp.Store.SetRoom(ctx, ch.ID, resp.RoomID); err != nil {
		return "", err
	}
	imp.roomsMu.Lock()
	imp.rooms[ch.ID] = resp.RoomID
	imp.roomsMu.Unlock()
	imp.Log.Info().Str("channel", ch.Name).Stringer("room", resp.RoomID).Msg("Created room")
	return resp.RoomID, nil
}

func ptr[T any](v T) *T { return &v }

// ensureJoined makes user a member of room, inviting them first. ts=0 uses
// the current time.
func (imp *Importer) ensureJoined(ctx context.Context, room id.RoomID, user id.UserID, ts int64) error {
	m, err := imp.Store.Membership(ctx, room, user)
	if err != nil || m == string(event.MembershipJoin) {
		return err
	}
	if user == imp.Cfg.BotUserID() {
		return nil
	}
	if m != string(event.MembershipInvite) {
		if err := imp.MX.SetMembership(ctx, room, imp.Cfg.BotUserID(), user, event.MembershipInvite, "", "", ts); err != nil {
			return fmt.Errorf("inviting %s to %s: %w", user, room, err)
		}
	}
	p := imp.profileOf(ctx, user)
	if err := imp.MX.SetMembership(ctx, room, imp.Cfg.BotUserID(), user, event.MembershipJoin, p.displayName, p.avatar, ts); err != nil {
		return fmt.Errorf("joining %s to %s: %w", user, room, err)
	}
	return imp.Store.SetMembership(ctx, room, user, string(event.MembershipJoin))
}

func (imp *Importer) leave(ctx context.Context, room id.RoomID, user id.UserID, ts int64) error {
	m, err := imp.Store.Membership(ctx, room, user)
	if err != nil || m != string(event.MembershipJoin) {
		return err
	}
	if err := imp.MX.SetMembership(ctx, room, "", user, event.MembershipLeave, "", "", ts); err != nil {
		return err
	}
	return imp.Store.SetMembership(ctx, room, user, string(event.MembershipLeave))
}

func muteRuleID(room id.RoomID) string {
	return "slack2matrix-mute-" + strings.NewReplacer("!", "", ":", "_", ".", "_").Replace(string(room))
}

// mute stops a real user's devices notifying for imported history. It is an
// override rule so it also beats mention rules; Finalize removes it.
func (imp *Importer) mute(ctx context.Context, room id.RoomID, user id.UserID) error {
	err := imp.MX.Client(user).PutPushRule(ctx, "global", pushrules.OverrideRule, muteRuleID(room), &mautrix.ReqPutPushRule{
		Actions: []*pushrules.PushAction{},
		Conditions: []*pushrules.PushCondition{{
			Kind:    pushrules.KindEventMatch,
			Key:     "room_id",
			Pattern: string(room),
		}},
	})
	if err != nil {
		return fmt.Errorf("muting %s for %s: %w", room, user, err)
	}
	return nil
}

// Unmute removes the import-time push rule.
func (imp *Importer) Unmute(ctx context.Context, room id.RoomID, user id.UserID) error {
	err := imp.MX.Client(user).DeletePushRule(ctx, "global", pushrules.OverrideRule, muteRuleID(room))
	if err != nil && !isNotFoundErr(err) {
		return err
	}
	return nil
}

func isNotFoundErr(err error) bool {
	var httpErr mautrix.HTTPError
	return errors.As(err, &httpErr) && httpErr.RespError != nil && httpErr.RespError.ErrCode == "M_NOT_FOUND"
}
