package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand/v2"
	"strings"

	"maunium.net/go/mautrix/id"

	"slack2matrix/internal/config"
	"slack2matrix/internal/convert"
	"slack2matrix/internal/directory"
	"slack2matrix/internal/slackexport"
	"slack2matrix/internal/transform"
)

// exportResolver resolves mentions using the export alone, plus an optional
// channel→room lookup once rooms exist.
type exportResolver struct {
	dir   *directory.Directory
	exp   *slackexport.Export
	rooms func(channelID string) id.RoomID
}

func (r *exportResolver) User(slackID string) *directory.Identity { return r.dir.User(slackID) }

func (r *exportResolver) Channel(slackID string) (string, id.RoomID) {
	c := r.exp.Channel(slackID)
	if c == nil {
		return "", ""
	}
	var room id.RoomID
	if r.rooms != nil {
		room = r.rooms(slackID)
	}
	return c.Name, room
}

// loadTransformer opens the export and builds the directory and transformer.
func loadTransformer(cfg *config.Config) (*slackexport.Export, *directory.Directory, *transform.Transformer, *exportResolver, error) {
	exp, err := slackexport.Open(cfg.ExportDir, cfg.MediaDir)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	dir, err := directory.Build(cfg, exp, func(visit func(*slackexport.Message)) error {
		for _, ch := range exp.Channels {
			msgs, err := exp.Messages(ch)
			if err != nil {
				return err
			}
			for _, m := range msgs {
				visit(m)
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	res := &exportResolver{dir: dir, exp: exp}
	tr := &transform.Transformer{
		Conv: &convert.Converter{Resolver: res, ServerName: cfg.Homeserver.ServerName},
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
	return exp, dir, tr, res, nil
}

func runRender(_ context.Context, cfg *config.Config, _ string, args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	channel := fs.String("channel", "", "channel name")
	ts := fs.String("ts", "", "message ts to render (default: all, or a random sample with -sample)")
	sample := fs.Int("sample", 0, "render this many random messages")
	grep := fs.String("grep", "", "only messages whose raw text contains this string")
	subtype := fs.String("subtype", "", "only messages with this subtype")
	limit := fs.Int("limit", 20, "maximum messages to render")
	_ = fs.Parse(args)

	exp, _, tr, _, err := loadTransformer(cfg)
	if err != nil {
		return err
	}
	var channels []*slackexport.Channel
	if *channel != "" {
		c := exp.ChannelByName(*channel)
		if c == nil {
			return fmt.Errorf("no channel named %q", *channel)
		}
		channels = []*slackexport.Channel{c}
	} else {
		channels = exp.Channels
	}
	var picked []*slackexport.Message
	var pickedChan []string
	for _, c := range channels {
		msgs, err := exp.Messages(c)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if *ts != "" && string(m.TS) != *ts {
				continue
			}
			if *grep != "" && !strings.Contains(m.Text, *grep) {
				continue
			}
			if *subtype != "" && m.Subtype != *subtype {
				continue
			}
			picked = append(picked, m)
			pickedChan = append(pickedChan, c.Name)
		}
	}
	idx := make([]int, len(picked))
	for i := range idx {
		idx[i] = i
	}
	if *sample > 0 {
		rand.Shuffle(len(idx), func(i, j int) { idx[i], idx[j] = idx[j], idx[i] })
		*limit = *sample
	}
	for n, i := range idx {
		if n >= *limit {
			break
		}
		m := picked[i]
		fmt.Printf("=== #%s ts=%s subtype=%q thread_ts=%s\n--- slack text: %q\n", pickedChan[i], m.TS, m.Subtype, m.ThreadTS, m.Text)
		drafts := tr.Message(m)
		if len(drafts) == 0 {
			fmt.Println("(no events)")
		}
		for _, d := range drafts {
			fmt.Println(d.String())
		}
		fmt.Println()
	}
	return nil
}
