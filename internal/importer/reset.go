package importer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/synapseadmin"

	"slack2matrix/internal/slackexport"
)

// ResetChannel deletes a channel's room from the homeserver, purging its
// events, and forgets it locally so the next import recreates the room and
// sends the channel's history again. adm must act as a Synapse server admin.
func (imp *Importer) ResetChannel(ctx context.Context, ch *slackexport.Channel, adm *synapseadmin.Client) error {
	room, _, err := imp.Store.Room(ctx, ch.ID)
	if err != nil {
		return err
	}
	if room == "" {
		return fmt.Errorf("#%s has no room to reset", ch.Name)
	}
	if err := deleteRoom(ctx, adm, room, imp.Log); err != nil {
		return err
	}
	if space, err := imp.Store.Get(ctx, "space_room_id"); err != nil {
		return err
	} else if space != "" {
		// Empty content removes the room from the space.
		if _, err := imp.MX.SendState(ctx, imp.Cfg.BotUserID(), id.RoomID(space), event.StateSpaceChild, string(room), struct{}{}, 0); err != nil {
			return fmt.Errorf("removing %s from space: %w", room, err)
		}
	}
	for _, user := range imp.Recipients() {
		if err := imp.Unmute(ctx, room, user); err != nil {
			return fmt.Errorf("removing mute rule for %s: %w", user, err)
		}
	}
	return imp.Store.ForgetChannel(ctx, ch.ID, room)
}

func deleteRoom(ctx context.Context, adm *synapseadmin.Client, room id.RoomID, log zerolog.Logger) error {
	resp, err := adm.DeleteRoom(ctx, room, synapseadmin.ReqDeleteRoom{Purge: true})
	if err != nil {
		var httpErr mautrix.HTTPError
		if errors.As(err, &httpErr) && httpErr.Response != nil && httpErr.Response.StatusCode == http.StatusForbidden {
			return fmt.Errorf("%s is not a server admin (pass -admin-token-file with a server admin's access token): %w", adm.Client.UserID, err)
		}
		return fmt.Errorf("deleting %s: %w", room, err)
	}
	log.Info().Stringer("room", room).Str("delete_id", resp.DeleteID).Msg("Deleting room")
	for {
		status, err := adm.DeleteRoomStatus(ctx, resp.DeleteID)
		if err != nil {
			return fmt.Errorf("checking deletion of %s: %w", room, err)
		}
		switch status.Status {
		case "complete":
			log.Info().Stringer("room", room).Int("kicked", len(status.ShutdownRoom.KickedUsers)).Msg("Room deleted and purged")
			return nil
		case "failed":
			return fmt.Errorf("deleting %s failed: %s", room, status.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
