package importer

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"slack2matrix/internal/convert"
	"slack2matrix/internal/slackexport"
)

// ImportChannels imports channels concurrently. Every channel runs at once,
// but at most concurrency messages are in flight; when slots are contended
// the largest channels go first, since they bound the total time. A message
// that links to an earlier message in another channel waits until that
// channel has imported it, so the link can point at the Matrix event.
//
// Waiting can't deadlock: a channel only waits for another channel to move
// past an earlier timestamp, and waiting channels don't hold a slot, so the
// channel whose next message is earliest overall can always proceed.
func (imp *Importer) ImportChannels(ctx context.Context, chans []*slackexport.Channel, f Filter, concurrency int) error {
	chans = slices.Clone(chans)
	sizes := map[string]int64{}
	for _, ch := range chans {
		sizes[ch.ID] = imp.Exp.ChannelSize(ch)
	}
	sort.SliceStable(chans, func(i, j int) bool { return sizes[chans[i].ID] > sizes[chans[j].ID] })

	b := &batch{
		sem:     &prioritySem{free: max(concurrency, 1)},
		next:    map[string]slackexport.TS{},
		changed: make(chan struct{}),
	}
	for _, ch := range chans {
		b.next[ch.ID] = ""
		times, err := imp.Exp.Times(ch)
		if err != nil {
			return err
		}
		b.total += int64(f.count(times))
	}
	b.progress = newProgress(imp, b)
	imp.Log.Info().Int("channels", len(chans)).Msgf("Importing %s messages", commas(b.total))

	stop := make(chan struct{})
	go b.progress.heartbeat(stop)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	for prio, ch := range chans {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := imp.importChannel(ctx, b, ch, prio, f)
			b.finish(ch.ID)
			if err != nil {
				imp.Log.Error().Err(err).Str("channel", ch.Name).Msg("Channel import failed")
				mu.Lock()
				failures = append(failures, ch.Name)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(stop)
	b.progress.report(true)
	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("%d channel(s) failed: %s (re-run to resume)", len(failures), strings.Join(failures, ", "))
	}
	return ctx.Err()
}

// batch coordinates the channels of one ImportChannels call.
type batch struct {
	sem      *prioritySem
	total    int64
	done     atomic.Int64
	waiting  atomic.Int64
	progress *progress

	mu sync.Mutex
	// next is the message each unfinished channel is working on; everything
	// before it has been imported. Finished channels are removed.
	next map[string]slackexport.TS
	// changed is closed and replaced whenever next changes.
	changed chan struct{}
}

// arrive records that a channel has imported everything before ts.
func (b *batch) arrive(channelID string, ts slackexport.TS) {
	b.mu.Lock()
	b.next[channelID] = ts
	close(b.changed)
	b.changed = make(chan struct{})
	b.mu.Unlock()
}

func (b *batch) finish(channelID string) {
	b.mu.Lock()
	delete(b.next, channelID)
	close(b.changed)
	b.changed = make(chan struct{})
	b.mu.Unlock()
}

// waitFor blocks until the channel has moved past ts, or isn't part of this
// batch (or has stopped).
func (b *batch) waitFor(ctx context.Context, channelID string, ts slackexport.TS) error {
	for {
		b.mu.Lock()
		next, running := b.next[channelID]
		changed := b.changed
		b.mu.Unlock()
		if !running || ts.Less(next) {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

var slackPermalink = regexp.MustCompile(`\.slack\.com/archives/([A-Z0-9]+)/(p\d{16})`)

// waitForLinks waits until every earlier message in another channel that m
// links to has been imported.
func (r *roomRun) waitForLinks(ctx context.Context, b *batch, m *slackexport.Message) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var waited bool
	for _, match := range slackPermalink.FindAllSubmatch(raw, -1) {
		channelID := string(match[1])
		ts, _ := convert.PermalinkTS(string(match[2]))
		target := slackexport.TS(ts)
		if channelID == r.ch.ID || !target.Less(m.TS) {
			continue
		}
		if !waited {
			waited = true
			b.waiting.Add(1)
			defer b.waiting.Add(-1)
		}
		if err := b.waitFor(ctx, channelID, target); err != nil {
			return err
		}
	}
	return nil
}

// prioritySem is a counting semaphore that hands free slots to the waiter
// with the lowest priority number.
type prioritySem struct {
	mu      sync.Mutex
	free    int
	waiters waiterHeap
}

type semWaiter struct {
	prio      int
	ready     chan struct{}
	granted   bool
	cancelled bool
}

func (s *prioritySem) acquire(ctx context.Context, prio int) error {
	s.mu.Lock()
	if s.free > 0 {
		s.free--
		s.mu.Unlock()
		return nil
	}
	w := &semWaiter{prio: prio, ready: make(chan struct{})}
	heap.Push(&s.waiters, w)
	s.mu.Unlock()
	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		w.cancelled = true
		granted := w.granted
		s.mu.Unlock()
		if granted {
			s.release()
		}
		return ctx.Err()
	}
}

func (s *prioritySem) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.waiters.Len() > 0 {
		w := heap.Pop(&s.waiters).(*semWaiter)
		if w.cancelled {
			continue
		}
		w.granted = true
		close(w.ready)
		return
	}
	s.free++
}

// queued reports how many acquirers are waiting (for tests).
func (s *prioritySem) queued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waiters.Len()
}

type waiterHeap []*semWaiter

func (h waiterHeap) Len() int           { return len(h) }
func (h waiterHeap) Less(i, j int) bool { return h[i].prio < h[j].prio }
func (h waiterHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *waiterHeap) Push(x any)        { *h = append(*h, x.(*semWaiter)) }
func (h *waiterHeap) Pop() any {
	old := *h
	w := old[len(old)-1]
	*h = old[:len(old)-1]
	return w
}

// progress reports how far the whole import has got: every 1,000 messages,
// and at least once a minute so a slow stretch (say, a large upload) doesn't
// look like a hang.
type progress struct {
	imp   *Importer
	b     *batch
	start time.Time

	mu       sync.Mutex
	last     time.Time
	samples  []sample
	reported int64
}

type sample struct {
	at   time.Time
	done int64
}

const progressEvery = 1000

func newProgress(imp *Importer, b *batch) *progress {
	now := time.Now()
	return &progress{imp: imp, b: b, start: now, last: now, samples: []sample{{now, 0}}}
}

// messageDone counts one finished message.
func (p *progress) messageDone() {
	if n := p.b.done.Add(1); n%progressEvery == 0 {
		p.report(false)
	}
}

func (p *progress) heartbeat(stop <-chan struct{}) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			p.mu.Lock()
			p.samples = append(p.samples, sample{now, p.b.done.Load()})
			// Keep five minutes of samples for the rate.
			for len(p.samples) > 2 && now.Sub(p.samples[1].at) > 5*time.Minute {
				p.samples = p.samples[1:]
			}
			quiet := now.Sub(p.last) >= time.Minute
			p.mu.Unlock()
			if quiet {
				p.report(false)
			}
		}
	}
}

func (p *progress) report(final bool) {
	done, total := p.b.done.Load(), p.b.total
	now := time.Now()
	p.mu.Lock()
	if !final && done == p.reported && now.Sub(p.last) < time.Minute {
		p.mu.Unlock()
		return
	}
	p.last, p.reported = now, done
	first := p.samples[0]
	p.mu.Unlock()

	pct := 100.0
	if total > 0 {
		pct = float64(done) * 100 / float64(total)
	}
	evt := p.imp.Log.Info().Str("elapsed", shortDuration(now.Sub(p.start)))
	if !final {
		if span := now.Sub(first.at); span > 30*time.Second && done > first.done {
			rate := float64(done-first.done) / span.Seconds()
			evt = evt.Str("rate", fmt.Sprintf("%.0f/s", rate))
			evt = evt.Str("eta", shortDuration(time.Duration(float64(total-done)/rate)*time.Second))
		}
		if w := p.b.waiting.Load(); w > 0 {
			evt = evt.Int64("channels_waiting_on_links", w)
		}
	}
	evt.Msgf("Processed %s of %s messages (%.1f%%)", commas(done), commas(total), pct)
}

// shortDuration formats d to the minute once it's over an hour ("3h58m"),
// otherwise to the second ("4m10s").
func shortDuration(d time.Duration) string {
	if d < time.Hour {
		return d.Round(time.Second).String()
	}
	return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
}

// commas formats n with thousands separators.
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	if n < 0 {
		return "-" + commas(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
