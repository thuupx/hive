package eventbus_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/thupham/hive/internal/event"
	"github.com/thupham/hive/internal/eventbus"
)

func newEvent(id string) *event.Event {
	return &event.Event{ID: id, SessionID: "sess_1", Type: event.TypeMessage, Version: 1}
}

func recv(t *testing.T, sub *eventbus.Subscription) *event.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.C():
		if !ok {
			t.Fatalf("%s: channel closed", sub.Name())
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: timed out waiting for an event", sub.Name())
		return nil
	}
}

func TestPublishFansOutInOrder(t *testing.T) {
	bus := eventbus.NewBus()
	defer bus.Close()

	first := bus.Subscribe("first", 8)
	second := bus.Subscribe("second", 8)

	bus.Publish(newEvent("e1"))
	bus.Publish(newEvent("e2"))
	bus.Publish(newEvent("e3"))

	for _, sub := range []*eventbus.Subscription{first, second} {
		for _, want := range []string{"e1", "e2", "e3"} {
			if got := recv(t, sub); got.ID != want {
				t.Errorf("%s: got %s, want %s", sub.Name(), got.ID, want)
			}
		}
	}
}

// Publication must never block agent execution, so a slow subscriber loses
// events and is told it is lagging instead of stalling the publisher.
func TestPublishNeverBlocksAndMarksLagged(t *testing.T) {
	bus := eventbus.NewBus()
	defer bus.Close()

	slow := bus.Subscribe("slow", 2)
	roomy := bus.Subscribe("roomy", 128)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			bus.Publish(newEvent(fmt.Sprintf("e%d", i)))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full subscriber")
	}

	if !slow.Lagged() {
		t.Error("the full subscriber should be marked lagging")
	}
	if slow.Dropped() == 0 {
		t.Error("Dropped() should be non-zero for the full subscriber")
	}
	if roomy.Lagged() {
		t.Error("a subscriber with room must not be marked lagging")
	}

	slow.ResetLag()
	if slow.Lagged() {
		t.Error("ResetLag should clear the flag")
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	bus := eventbus.NewBus()
	defer bus.Close()

	sub := bus.Subscribe("a", 4)
	bus.Publish(newEvent("e1"))
	sub.Close()

	// An already-buffered event is still readable.
	if got := recv(t, sub); got.ID != "e1" {
		t.Fatalf("got %s, want e1", got.ID)
	}
	if _, ok := <-sub.C(); ok {
		t.Fatal("the channel should be closed after Close")
	}
	if bus.Subscribers() != 0 {
		t.Fatalf("subscribers = %d, want 0", bus.Subscribers())
	}
}

func TestCloseClosesEverySubscription(t *testing.T) {
	bus := eventbus.NewBus()
	a := bus.Subscribe("a", 4)
	b := bus.Subscribe("b", 4)

	bus.Close()

	for _, sub := range []*eventbus.Subscription{a, b} {
		if _, ok := <-sub.C(); ok {
			t.Errorf("%s: channel should be closed", sub.Name())
		}
	}
	if bus.Subscribers() != 0 {
		t.Fatalf("subscribers = %d, want 0", bus.Subscribers())
	}
}

func TestPublishAfterCloseIsSafe(t *testing.T) {
	bus := eventbus.NewBus()
	bus.Close()

	bus.Publish(newEvent("e1"))

	late := bus.Subscribe("late", 4)
	if _, ok := <-late.C(); ok {
		t.Fatal("a subscription on a closed bus should be closed immediately")
	}
}

func TestConcurrentPublishSubscribeAndClose(t *testing.T) {
	bus := eventbus.NewBus()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub := bus.Subscribe("worker", 4)
			defer sub.Close()
			for j := 0; j < 200; j++ {
				bus.Publish(newEvent("e"))
			}
		}()
	}
	wg.Wait()
	bus.Close()
}
