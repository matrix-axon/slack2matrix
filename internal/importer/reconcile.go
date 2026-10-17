package importer

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// reconcileWindow is how many of a room's latest events are searched for
// sends that were in flight when a previous run stopped. Each room is
// imported sequentially, so in-flight sends are always near the end.
const reconcileWindow = 200

// reconcile resolves events left 'pending' by an interrupted run. Synapse
// only deduplicates transaction IDs for a short time, so instead the room's
// latest events are decrypted (the importer holds every session) and
// matched against the Slack source recorded inside each event. Matches are
// marked sent; anything not found is left pending and will be re-sent.
func (imp *Importer) reconcile(ctx context.Context, channelID string, room id.RoomID) error {
	pending, err := imp.Store.PendingEvents(ctx, channelID)
	if err != nil || len(pending) == 0 {
		return err
	}
	want := map[string]bool{}
	for _, p := range pending {
		want[p] = true
	}
	resp, err := imp.MX.Bot().Messages(ctx, room, "", "", mautrix.DirectionBackward, nil, reconcileWindow)
	if err != nil {
		return err
	}
	found := 0
	for _, evt := range resp.Chunk {
		if evt.Type != event.EventEncrypted {
			continue
		}
		src := imp.decryptSource(ctx, evt)
		if src == nil || src.Channel != channelID {
			continue
		}
		key := src.TS + "/" + strconv.Itoa(src.Part)
		if !want[key] {
			continue
		}
		if err := imp.Store.MarkSent(ctx, channelID, src.TS, src.Part, evt.ID); err != nil {
			return err
		}
		delete(want, key)
		found++
	}
	imp.Log.Info().Str("channel", channelID).Int("pending", len(pending)).Int("already_delivered", found).Msg("Reconciled interrupted sends")
	return nil
}

// decryptSource decrypts an imported event with its sender's machine and
// returns the Slack source embedded in it.
func (imp *Importer) decryptSource(ctx context.Context, evt *event.Event) *Source {
	if err := evt.Content.ParseRaw(evt.Type); err != nil {
		return nil
	}
	// Only accounts the importer sends as can have imported events; don't
	// create a device for anyone else.
	if dev, _, err := imp.Store.Device(ctx, evt.Sender); err != nil || dev == "" {
		return nil
	}
	mach, err := imp.E2EE.Machine(ctx, evt.Sender)
	if err != nil {
		return nil
	}
	dec, err := mach.DecryptMegolmEvent(ctx, evt)
	if err != nil {
		return nil
	}
	raw, ok := dec.Content.Raw[SourceKey]
	if !ok {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var src Source
	if json.Unmarshal(data, &src) != nil || !strings.Contains(src.TS, ".") {
		return nil
	}
	return &src
}
