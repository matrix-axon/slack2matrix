package importer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"slack2matrix/internal/slackexport"
	"slack2matrix/internal/transform"
)

// prefetchWindow is how many messages a channel's prefetcher may run ahead
// of the messages being sent.
const prefetchWindow = 1000

// uploads makes sure each piece of media is uploaded once, whether by a
// background prefetch worker or inline by the message that needs it.
type uploads struct {
	// upload does the work and records the outcome.
	upload func(ctx context.Context, key string, sender id.UserID, m *transform.Media) error
	log    zerolog.Logger

	mu   sync.Mutex
	jobs map[string]*uploadJob
	// queue feeds the prefetch workers; nil when prefetching is off.
	queue chan *uploadJob
}

type uploadJob struct {
	key    string
	sender id.UserID
	media  *transform.Media
	// claimed is set once someone has started the upload.
	claimed bool
	done    chan struct{}
	err     error
}

func newUploads(imp *Importer) *uploads {
	return &uploads{upload: imp.storeUpload, log: imp.Log, jobs: map[string]*uploadJob{}}
}

// ensure uploads m unless that already happened, waiting for an upload
// already in progress. The outcome is recorded in the store: either the
// uploaded media or the reason it's unavailable.
func (u *uploads) ensure(ctx context.Context, key string, sender id.UserID, m *transform.Media) error {
	for {
		u.mu.Lock()
		job := u.jobs[key]
		if job == nil {
			job = &uploadJob{key: key, sender: sender, media: m, done: make(chan struct{})}
			u.jobs[key] = job
		}
		if !job.claimed {
			// Not started (or only queued): do it now rather than wait.
			job.claimed = true
			u.mu.Unlock()
			u.run(ctx, job)
			return job.err
		}
		u.mu.Unlock()
		select {
		case <-job.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if job.err == nil {
			return nil
		}
		// A prefetch failed (or was cancelled): try again ourselves, so the
		// error that stops the import is our own.
	}
}

// run uploads the job's media and records the outcome.
func (u *uploads) run(ctx context.Context, job *uploadJob) {
	job.err = u.upload(ctx, job.key, job.sender, job.media)
	u.mu.Lock()
	delete(u.jobs, job.key)
	u.mu.Unlock()
	close(job.done)
}

// prefetch queues m for a background upload, blocking while the queue is
// full. It returns false once ctx is done.
func (u *uploads) prefetch(ctx context.Context, key string, sender id.UserID, m *transform.Media) bool {
	u.mu.Lock()
	if u.jobs[key] != nil {
		u.mu.Unlock()
		return true
	}
	job := &uploadJob{key: key, sender: sender, media: m, done: make(chan struct{})}
	u.jobs[key] = job
	u.mu.Unlock()
	select {
	case u.queue <- job:
		return true
	case <-ctx.Done():
		// Leave the job for whoever needs it: ensure claims unstarted jobs.
		return false
	}
}

// startWorkers starts n prefetch workers, which run until ctx is done.
func (u *uploads) startWorkers(ctx context.Context, n int) *sync.WaitGroup {
	var wg sync.WaitGroup
	u.queue = make(chan *uploadJob, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				var job *uploadJob
				select {
				case job = <-u.queue:
				case <-ctx.Done():
					return
				}
				u.mu.Lock()
				if job.claimed {
					// The message got there first.
					u.mu.Unlock()
					continue
				}
				job.claimed = true
				u.mu.Unlock()
				u.run(ctx, job)
				if job.err != nil && ctx.Err() == nil {
					u.log.Warn().Err(job.err).Str("media", job.key).Msg("Media prefetch failed; will retry when the message is sent")
				}
			}
		}()
	}
	return &wg
}

// storeUpload uploads media unless the store already has it (or knows it's
// unavailable), and records the result.
func (imp *Importer) storeUpload(ctx context.Context, key string, sender id.UserID, m *transform.Media) error {
	if failure, err := imp.Store.MediaFailure(ctx, key); err != nil || failure != "" {
		return err
	}
	if stored, err := imp.Store.Media(ctx, key); err != nil || stored != "" {
		return err
	}
	sm, err := imp.uploadMedia(ctx, sender, m)
	if errors.Is(err, errUnavailable) {
		imp.Log.Warn().Err(err).Str("media", key).Msg("Media unavailable")
		return imp.Store.SetMediaFailure(ctx, key, err.Error())
	} else if err != nil {
		return err
	}
	data, err := json.Marshal(sm)
	if err != nil {
		return err
	}
	return imp.Store.SetMedia(ctx, key, string(data))
}

// cursor is how far a channel's sender has got, for its prefetcher.
type cursor struct {
	mu      sync.Mutex
	pos     int
	changed chan struct{}
}

func newCursor() *cursor { return &cursor{changed: make(chan struct{})} }

func (c *cursor) set(pos int) {
	c.mu.Lock()
	c.pos = pos
	close(c.changed)
	c.changed = make(chan struct{})
	c.mu.Unlock()
}

// waitUntil blocks until the sender has reached pos.
func (c *cursor) waitUntil(ctx context.Context, pos int) bool {
	for {
		c.mu.Lock()
		reached, changed := c.pos >= pos, c.changed
		c.mu.Unlock()
		if reached {
			return true
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}

// prefetchChannel queues the media of msgs (the messages the channel will
// send, in order) for upload, staying within prefetchWindow messages of cur.
func (imp *Importer) prefetchChannel(ctx context.Context, msgs []*slackexport.Message, cur *cursor) {
	for i, m := range msgs {
		if !mayHaveMedia(m) {
			continue
		}
		if !cur.waitUntil(ctx, i-prefetchWindow) {
			return
		}
		for _, d := range imp.Tr.Message(m) {
			if d.Kind != transform.KindMedia {
				continue
			}
			if err := imp.ensureIdentity(ctx, d.Sender); err != nil {
				// The sender will hit (and report) this too.
				return
			}
			if !imp.uploads.prefetch(ctx, mediaKey(d.Media), d.Sender.MXID, d.Media) {
				return
			}
		}
	}
}

// mayHaveMedia cheaply rules out messages that can't produce media drafts,
// so most messages aren't transformed twice.
func mayHaveMedia(m *slackexport.Message) bool {
	if len(m.Files) > 0 || len(m.Attachments) > 0 {
		return true
	}
	for _, b := range m.Blocks {
		if b.Type == "image" {
			return true
		}
	}
	return false
}
