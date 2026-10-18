package importer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"slack2matrix/internal/slackexport"
	"slack2matrix/internal/transform"
)

func TestPrioritySemOrder(t *testing.T) {
	s := &prioritySem{free: 1}
	ctx := context.Background()
	if err := s.acquire(ctx, 5); err != nil {
		t.Fatal(err)
	}
	got := make(chan int, 3)
	for _, prio := range []int{3, 1, 2} {
		go func() {
			if err := s.acquire(ctx, prio); err == nil {
				got <- prio
			}
		}()
	}
	// Let all three queue up before freeing the slot.
	for s.queued() < 3 {
		time.Sleep(time.Millisecond)
	}
	for _, want := range []int{1, 2, 3} {
		s.release()
		if p := <-got; p != want {
			t.Fatalf("slot went to priority %d, want %d", p, want)
		}
	}
}

func TestPrioritySemCancel(t *testing.T) {
	s := &prioritySem{free: 1}
	_ = s.acquire(context.Background(), 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.acquire(ctx, 1) }()
	for s.queued() < 1 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled acquire succeeded")
	}
	s.release()
	// The cancelled waiter must not have swallowed the slot.
	if err := s.acquire(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
}

func TestBatchWaitFor(t *testing.T) {
	b := &batch{next: map[string]slackexport.TS{"A": ""}, changed: make(chan struct{})}
	ctx := context.Background()
	// Channels outside the batch never block.
	if err := b.waitFor(ctx, "B", "100.000001"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = b.waitFor(ctx, "A", "100.000001")
		close(done)
	}()
	b.arrive("A", "100.000001") // working on the target itself: still waiting
	select {
	case <-done:
		t.Fatal("returned before the target was imported")
	case <-time.After(20 * time.Millisecond):
	}
	b.arrive("A", "100.000002")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("still waiting after the channel moved past the target")
	}
	// A finished (or failed) channel releases its waiters.
	b.arrive("A", "50.000000")
	done2 := make(chan struct{})
	go func() {
		_ = b.waitFor(ctx, "A", "100.000005")
		close(done2)
	}()
	b.finish("A")
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("still waiting after the channel finished")
	}
}

func TestCommas(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 700759: "700,759", 1234567: "1,234,567", -4200: "-4,200"} {
		if got := commas(n); got != want {
			t.Errorf("commas(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		250 * time.Second:                "4m10s",
		3*time.Hour + 58*time.Minute + 7: "3h58m",
		5 * time.Hour:                    "5h0m",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestUploadsDedup(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	active := map[string]bool{}
	release := make(chan struct{})
	u := &uploads{jobs: map[string]*uploadJob{}, log: zerolog.Nop()}
	// Like storeUpload, skip media that's already been uploaded.
	u.upload = func(ctx context.Context, key string, _ id.UserID, _ *transform.Media) error {
		mu.Lock()
		if active[key] {
			t.Errorf("%s uploaded twice at once", key)
		}
		if calls[key] > 0 {
			mu.Unlock()
			return nil
		}
		calls[key]++
		active[key] = true
		mu.Unlock()
		<-release
		mu.Lock()
		active[key] = false
		mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workers := u.startWorkers(ctx, 1)
	u.prefetch(ctx, "a", "", nil)
	// Wait for the worker to start "a", then queue "b" behind it.
	for {
		u.mu.Lock()
		started := u.jobs["a"] != nil && u.jobs["a"].claimed
		u.mu.Unlock()
		if started {
			break
		}
		time.Sleep(time.Millisecond)
	}
	u.prefetch(ctx, "b", "", nil)
	u.prefetch(ctx, "a", "", nil) // already in progress: not queued again

	// A message needing "a" waits for the worker; one needing "b" (queued,
	// not started) uploads it itself.
	gotA := make(chan error)
	go func() { gotA <- u.ensure(ctx, "a", "", nil) }()
	gotB := make(chan error)
	go func() { gotB <- u.ensure(ctx, "b", "", nil) }()
	close(release)
	if err := <-gotA; err != nil {
		t.Fatal(err)
	}
	if err := <-gotB; err != nil {
		t.Fatal(err)
	}
	cancel()
	workers.Wait()
	if calls["a"] != 1 || calls["b"] != 1 {
		t.Fatalf("upload calls = %v, want one each", calls)
	}
}

func TestUploadsRetryFailedPrefetch(t *testing.T) {
	var calls atomic.Int32
	u := &uploads{jobs: map[string]*uploadJob{}, log: zerolog.Nop()}
	u.upload = func(ctx context.Context, key string, _ id.UserID, _ *transform.Media) error {
		if calls.Add(1) == 1 {
			return errors.New("transient")
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	workers := u.startWorkers(ctx, 1)
	u.prefetch(ctx, "a", "", nil)
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if err := u.ensure(ctx, "a", "", nil); err != nil {
		t.Fatalf("ensure after failed prefetch: %v", err)
	}
	cancel()
	workers.Wait()
	if n := calls.Load(); n != 2 {
		t.Fatalf("upload calls = %d, want 2", n)
	}
}

func TestCursor(t *testing.T) {
	c := newCursor()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() { done <- c.waitUntil(ctx, 5) }()
	c.set(4)
	select {
	case <-done:
		t.Fatal("returned before the cursor reached 5")
	case <-time.After(20 * time.Millisecond):
	}
	c.set(5)
	if !<-done {
		t.Fatal("waitUntil failed")
	}
	go func() { done <- c.waitUntil(ctx, 10) }()
	cancel()
	if <-done {
		t.Fatal("waitUntil succeeded after cancel")
	}
}
