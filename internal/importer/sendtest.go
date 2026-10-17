package importer

import (
	"context"
	"fmt"
	"html"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"slack2matrix/internal/slackexport"
	"slack2matrix/internal/transform"
)

// SendTest renders one Slack message as it would be imported and posts its
// text event to the channel's room now, as the given user, marked as a test.
// Nothing is recorded in the import state, so the real import is unaffected.
func (imp *Importer) SendTest(ctx context.Context, ch *slackexport.Channel, ts slackexport.TS, as id.UserID) (id.EventID, error) {
	room := imp.room(ch.ID)
	if room == "" {
		return "", fmt.Errorf("#%s has no room yet", ch.Name)
	}
	msgs, err := imp.Exp.Messages(ch)
	if err != nil {
		return "", err
	}
	var draft *transform.Draft
	for _, m := range msgs {
		if m.TS != ts {
			continue
		}
		for _, d := range imp.Tr.Message(m) {
			if d.Kind == transform.KindMessage {
				draft = d
				break
			}
		}
	}
	if draft == nil {
		return "", fmt.Errorf("no text message with ts %s in #%s", ts, ch.Name)
	}
	if err := imp.ensureJoined(ctx, room, as, 0); err != nil {
		return "", err
	}
	const label = "[slack2matrix test] "
	c := *draft.Content
	c.Body = label + c.Body
	if c.FormattedBody != "" {
		c.FormattedBody = "<em>" + html.EscapeString(label) + "</em>" + c.FormattedBody
	}
	txn := fmt.Sprintf("s2mtest%d", time.Now().UnixNano())
	return imp.E2EE.Send(ctx, room, as, event.EventMessage, &c, time.Now().UnixMilli(), txn)
}
