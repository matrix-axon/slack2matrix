package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"slack2matrix/internal/slackexport"
	"slack2matrix/internal/transform"
)

// SourceKey is added to every imported event's (encrypted) content so the
// original Slack message can be identified later, e.g. by verify.
const SourceKey = "net.slack2matrix.source"

// Source identifies the Slack message an event came from.
type Source struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	Part    int    `json:"part"`
}

// Filter limits which messages are imported, for test runs.
type Filter struct {
	Since time.Time
	Until time.Time
	// Limit caps the number of messages per channel (0 = no limit).
	Limit int
}

func (f *Filter) include(m *slackexport.Message) bool {
	t := m.TS.Time()
	if !f.Since.IsZero() && t.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !t.Before(f.Until) {
		return false
	}
	return true
}

// roomRun is the state of one channel's import.
type roomRun struct {
	imp  *Importer
	ch   *slackexport.Channel
	room id.RoomID
	// threadLatest tracks the newest event in each thread for the reply
	// fallback of the next thread message.
	threadLatest  map[slackexport.TS]id.EventID
	sent, skipped int
}

// ImportChannel sends every message of a channel in chronological order.
// Already-sent parts are skipped, so it can be re-run after interruption.
func (imp *Importer) ImportChannel(ctx context.Context, ch *slackexport.Channel, f Filter) error {
	room := imp.room(ch.ID)
	if room == "" {
		return fmt.Errorf("no room for #%s; run setup first", ch.Name)
	}
	msgs, err := imp.Exp.Messages(ch)
	if err != nil {
		return err
	}
	run := &roomRun{imp: imp, ch: ch, room: room, threadLatest: map[slackexport.TS]id.EventID{}}
	log := imp.Log.With().Str("channel", ch.Name).Logger()
	if err := imp.reconcile(ctx, ch.ID, room); err != nil {
		return fmt.Errorf("#%s: reconciling interrupted sends: %w", ch.Name, err)
	}
	start := time.Now()
	count := 0
	for _, m := range msgs {
		if !f.include(m) {
			continue
		}
		if f.Limit > 0 && count >= f.Limit {
			break
		}
		count++
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := run.message(ctx, m); err != nil {
			return fmt.Errorf("#%s message %s: %w", ch.Name, m.TS, err)
		}
		if count%1000 == 0 {
			log.Info().Int("messages", count).Int("events_sent", run.sent).Dur("elapsed", time.Since(start)).Msg("Progress")
		}
	}
	log.Info().Int("messages", count).Int("events_sent", run.sent).Int("already_sent", run.skipped).Dur("elapsed", time.Since(start)).Msg("Channel done")
	return nil
}

// txnID derives a deterministic transaction ID. The sender is included
// because Synapse's deduplication key ignores which user an appservice acts as.
func txnID(channel string, ts slackexport.TS, part int, sender id.UserID) string {
	h := sha256.Sum256([]byte(channel + "|" + string(ts) + "|" + strconv.Itoa(part) + "|" + string(sender)))
	return "s2m" + hex.EncodeToString(h[:12])
}

func (r *roomRun) message(ctx context.Context, m *slackexport.Message) error {
	drafts := r.imp.Tr.Message(m)
	if len(drafts) == 0 {
		return nil
	}
	var threadRoot id.EventID
	if m.IsThreadReply() {
		var err error
		if threadRoot, err = r.threadRoot(ctx, m); err != nil {
			return err
		}
	}
	var anchor id.EventID
	for _, d := range drafts {
		row, err := r.imp.Store.Event(ctx, r.ch.ID, string(m.TS), d.Part)
		if err != nil {
			return err
		}
		if row != nil && row.Status == "sent" {
			r.skipped++
			if anchor == "" && (d.Kind == transform.KindMessage || d.Kind == transform.KindMedia) {
				anchor = row.EventID
			}
			if threadRoot != "" && row.EventID != "" && d.Kind != transform.KindReaction {
				r.threadLatest[m.ThreadTS] = row.EventID
			}
			continue
		}
		evtID, err := r.send(ctx, m, d, threadRoot, anchor)
		if err != nil {
			return err
		}
		if evtID == "" {
			continue
		}
		if anchor == "" && (d.Kind == transform.KindMessage || d.Kind == transform.KindMedia) {
			anchor = evtID
		}
		if threadRoot != "" && d.Kind != transform.KindReaction && d.Kind != transform.KindMembership {
			r.threadLatest[m.ThreadTS] = evtID
		}
	}
	return nil
}

// threadRoot finds the event of a thread's root message, sending a
// placeholder if the root isn't in the export (or was filtered out).
func (r *roomRun) threadRoot(ctx context.Context, m *slackexport.Message) (id.EventID, error) {
	root, err := r.imp.Store.FirstEvent(ctx, r.ch.ID, string(m.ThreadTS))
	if err != nil || root != "" {
		return root, err
	}
	sender := r.imp.Dir.User(m.ParentUserID)
	if m.ParentUserID == "" {
		sender = r.imp.Dir.Sender(m)
	}
	ts := m.ThreadTS.Millis()
	if ts == 0 || ts > m.TS.Millis() {
		ts = m.TS.Millis() - 1
	}
	d := &transform.Draft{
		Kind:   transform.KindMessage,
		Sender: sender,
		TS:     ts,
		Content: &event.MessageEventContent{
			MsgType: event.MsgNotice,
			Body:    "(The message that started this thread is not in the Slack export.)",
		},
	}
	placeholder := &slackexport.Message{TS: m.ThreadTS}
	return r.send(ctx, placeholder, d, "", "")
}

// send delivers one draft and records it.
func (r *roomRun) send(ctx context.Context, m *slackexport.Message, d *transform.Draft, threadRoot, anchor id.EventID) (id.EventID, error) {
	imp := r.imp
	sender := d.Sender.MXID
	if err := imp.ensureIdentity(ctx, d.Sender); err != nil {
		return "", err
	}
	if d.Kind == transform.KindMembership {
		switch d.Membership {
		case event.MembershipJoin:
			if err := imp.ensureJoined(ctx, r.room, sender, d.TS); err != nil {
				return "", err
			}
		case event.MembershipLeave:
			if err := imp.leave(ctx, r.room, sender, d.TS); err != nil {
				return "", err
			}
		}
		// Membership events are state, recorded without an event ID.
		return "", imp.recordDone(ctx, r.ch.ID, m.TS, d, sender, "")
	}
	if err := imp.ensureJoined(ctx, r.room, sender, d.TS); err != nil {
		return "", err
	}

	var evtType event.Type
	var content any
	switch d.Kind {
	case transform.KindReaction:
		if anchor == "" {
			return "", nil
		}
		evtType = event.EventReaction
		content = map[string]any{
			"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": anchor, "key": d.ReactionKey},
			SourceKey:      Source{Channel: r.ch.ID, TS: string(m.TS), Part: d.Part},
		}
	case transform.KindMessage, transform.KindMedia:
		c := *d.Content
		if d.Kind == transform.KindMedia {
			ok, err := imp.attachMedia(ctx, sender, d.Media, &c)
			if err != nil {
				return "", err
			}
			if !ok {
				// The media couldn't be fetched: send a link or note instead.
				c = unavailableMedia(d)
			}
		}
		if d.Kind == transform.KindMessage {
			if err := imp.fitSize(ctx, sender, &c); err != nil {
				return "", err
			}
		}
		if threadRoot != "" {
			latest := r.threadLatest[m.ThreadTS]
			if latest == "" {
				latest = threadRoot
			}
			c.RelatesTo = (&event.RelatesTo{}).SetThread(threadRoot, latest)
		} else if d.ReplyToFile != "" {
			if fileRoom, fileEvt, err := imp.Store.FileEvent(ctx, d.ReplyToFile); err != nil {
				return "", err
			} else if fileEvt != "" && fileRoom == r.room {
				c.RelatesTo = (&event.RelatesTo{}).SetReplyTo(fileEvt)
			}
		}
		evtType = event.EventMessage
		content = &event.Content{Parsed: &c, Raw: map[string]any{SourceKey: Source{Channel: r.ch.ID, TS: string(m.TS), Part: d.Part}}}
	default:
		return "", fmt.Errorf("unknown draft kind %q", d.Kind)
	}

	txn := txnID(r.ch.ID, m.TS, d.Part, sender)
	if err := imp.Store.MarkPending(ctx, r.ch.ID, string(m.TS), d.Part, string(d.Kind), sender, txn); err != nil {
		return "", err
	}
	evtID, err := imp.E2EE.Send(ctx, r.room, sender, evtType, content, d.TS, txn)
	if d.Kind == transform.KindReaction && isErrCode(err, "M_DUPLICATE_ANNOTATION") {
		// The same reaction is already there; nothing more to send.
		return "", imp.Store.MarkSent(ctx, r.ch.ID, string(m.TS), d.Part, "")
	}
	if err != nil {
		return "", fmt.Errorf("sending part %d as %s: %w", d.Part, sender, err)
	}
	if err := imp.Store.MarkSent(ctx, r.ch.ID, string(m.TS), d.Part, evtID); err != nil {
		return "", err
	}
	if d.FileID != "" {
		if err := imp.Store.SetFileEvent(ctx, d.FileID, r.room, evtID); err != nil {
			return "", err
		}
	}
	r.sent++
	return evtID, nil
}

func isErrCode(err error, code string) bool {
	var httpErr mautrix.HTTPError
	return errors.As(err, &httpErr) && httpErr.RespError != nil && httpErr.RespError.ErrCode == code
}

func (imp *Importer) recordDone(ctx context.Context, channel string, ts slackexport.TS, d *transform.Draft, sender id.UserID, evtID id.EventID) error {
	if err := imp.Store.MarkPending(ctx, channel, string(ts), d.Part, string(d.Kind), sender, ""); err != nil {
		return err
	}
	return imp.Store.MarkSent(ctx, channel, string(ts), d.Part, evtID)
}

// unavailableMedia replaces a media draft whose content couldn't be fetched.
func unavailableMedia(d *transform.Draft) event.MessageEventContent {
	if d.Media.RemoteURL != "" {
		return event.MessageEventContent{
			MsgType:       event.MsgText,
			Body:          d.Media.RemoteURL,
			Format:        event.FormatHTML,
			FormattedBody: `<a href="` + escapeAttr(d.Media.RemoteURL) + `">` + escapeAttr(d.Media.RemoteURL) + `</a>`,
		}
	}
	return event.MessageEventContent{MsgType: event.MsgNotice, Body: "[file could not be imported: " + d.Media.Name + "]"}
}
