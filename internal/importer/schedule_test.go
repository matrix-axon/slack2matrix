package importer

import (
	"context"
	"testing"
	"time"

	"slack2matrix/internal/slackexport"
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
