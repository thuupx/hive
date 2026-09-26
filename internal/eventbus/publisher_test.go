package eventbus_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/thuupx/hive/internal/event"
	"github.com/thuupx/hive/internal/eventbus"
)

// fakeOutbox models the durable outbox: an event stays pending until it is
// marked published.
type fakeOutbox struct {
	mu        sync.Mutex
	events    []*event.Event
	published map[string]bool
	failMark  bool
}

func newFakeOutbox(ids ...string) *fakeOutbox {
	f := &fakeOutbox{published: map[string]bool{}}
	for _, id := range ids {
		f.events = append(f.events, newEvent(id))
	}
	return f
}

func (f *fakeOutbox) PendingOutbox(_ context.Context, limit int) ([]*event.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*event.Event
	for _, ev := range f.events {
		if f.published[ev.ID] {
			continue
		}
		out = append(out, ev)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeOutbox) MarkPublishedBatch(_ context.Context, ids ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failMark {
		return errors.New("mark failed")
	}
	for _, id := range ids {
		f.published[id] = true
	}
	return nil
}

func (f *fakeOutbox) pendingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, ev := range f.events {
		if !f.published[ev.ID] {
			n++
		}
	}
	return n
}

func TestPublisherDrainPublishesAndMarks(t *testing.T) {
	bus := eventbus.NewBus()
	defer bus.Close()

	sub := bus.Subscribe("consumer", 16)
	outbox := newFakeOutbox("e1", "e2", "e3")
	pub := eventbus.NewPublisher(bus, outbox)

	n, err := pub.Drain(context.Background())
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if n != 3 {
		t.Fatalf("published = %d, want 3", n)
	}
	for _, want := range []string{"e1", "e2", "e3"} {
		if got := recv(t, sub); got.ID != want {
			t.Errorf("got %s, want %s", got.ID, want)
		}
	}
	if outbox.pendingCount() != 0 {
		t.Fatalf("pending = %d, want 0", outbox.pendingCount())
	}

	// Nothing left to publish.
	if n, err := pub.Drain(context.Background()); err != nil || n != 0 {
		t.Fatalf("second Drain = %d, %v; want 0, nil", n, err)
	}
}

// Publication is at-least-once: if marking fails, the event is published again
// on the next drain. Consumers deduplicate by event id.
func TestPublisherRepublishesWhenMarkingFails(t *testing.T) {
	bus := eventbus.NewBus()
	defer bus.Close()

	sub := bus.Subscribe("consumer", 16)
	outbox := newFakeOutbox("e1")
	outbox.failMark = true
	pub := eventbus.NewPublisher(bus, outbox)

	if _, err := pub.Drain(context.Background()); err == nil {
		t.Fatal("expected the marking failure to surface")
	}
	if got := recv(t, sub); got.ID != "e1" {
		t.Fatalf("got %s, want e1", got.ID)
	}
	if outbox.pendingCount() != 1 {
		t.Fatal("the event should still be pending after a failed mark")
	}

	outbox.failMark = false
	if _, err := pub.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got := recv(t, sub); got.ID != "e1" {
		t.Fatalf("got %s, want a redelivery of e1", got.ID)
	}
	if outbox.pendingCount() != 0 {
		t.Fatal("the event should be published after a successful mark")
	}
}

func TestPublisherRunStopsOnCancel(t *testing.T) {
	bus := eventbus.NewBus()
	defer bus.Close()

	outbox := newFakeOutbox("e1")
	pub := eventbus.NewPublisher(bus, outbox)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pub.Run(ctx) }()

	deadline := time.After(3 * time.Second)
	for outbox.pendingCount() != 0 {
		select {
		case <-deadline:
			t.Fatal("Run did not publish the pending event")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
}

func TestPublisherReportsErrorsWithoutStopping(t *testing.T) {
	bus := eventbus.NewBus()
	defer bus.Close()

	outbox := newFakeOutbox("e1")
	outbox.failMark = true

	var (
		mu   sync.Mutex
		seen []error
	)
	pub := eventbus.NewPublisher(bus, outbox)
	pub.OnError = func(err error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pub.Run(ctx) }()

	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run never reported the error")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
}
