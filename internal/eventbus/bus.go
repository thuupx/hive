// Package eventbus provides realtime fan-out of durable events.
//
// The realtime path and the durability path are separate. Publication never
// blocks agent execution: a subscriber whose bounded queue is full loses
// events and is reported as lagging, and it must resync from its durable
// cursor rather than assume it has seen everything.
package eventbus

import (
	"sync"
	"sync/atomic"

	"github.com/thupham/hive/internal/event"
)

// DefaultCapacity is the per-subscriber queue depth.
const DefaultCapacity = 256

// Bus fans durable events out to subscribers.
//
// A Bus is safe for concurrent use. Delivery order within a subscription
// matches publish order, which for a single publisher is stream order.
type Bus struct {
	mu     sync.RWMutex
	subs   map[*Subscription]struct{}
	closed bool
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[*Subscription]struct{})}
}

// Subscribe registers a subscriber with its own bounded queue.
func (b *Bus) Subscribe(name string, capacity int) *Subscription {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	sub := &Subscription{
		bus:  b,
		name: name,
		ch:   make(chan *event.Event, capacity),
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		sub.markClosed()
		return sub
	}
	b.subs[sub] = struct{}{}
	return sub
}

// Publish delivers ev to every subscriber without blocking.
//
// A subscriber whose queue is full drops the event and is marked lagging. The
// event is still durable, so the subscriber recovers by replaying from its
// cursor.
func (b *Bus) Publish(ev *event.Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for sub := range b.subs {
		sub.deliver(ev)
	}
}

// Subscribers returns the number of active subscribers.
func (b *Bus) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// Unsubscribe removes a subscriber and closes its channel.
func (b *Bus) Unsubscribe(sub *Subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[sub]; !ok {
		return
	}
	delete(b.subs, sub)
	sub.markClosed()
}

// Close removes every subscriber and closes their channels.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for sub := range b.subs {
		sub.markClosed()
	}
	b.subs = make(map[*Subscription]struct{})
}

// Subscription is a bounded realtime delivery queue.
type Subscription struct {
	bus  *Bus
	name string
	ch   chan *event.Event

	dropped atomic.Int64
	mu      sync.Mutex
	closed  bool
}

// Name returns the subscriber name given at Subscribe.
func (s *Subscription) Name() string { return s.name }

// C returns the delivery channel. It is closed when the subscription is
// closed or the bus shuts down, so a consumer may range over it.
func (s *Subscription) C() <-chan *event.Event { return s.ch }

// Dropped returns how many events were dropped because the queue was full.
func (s *Subscription) Dropped() int64 { return s.dropped.Load() }

// Lagged reports whether any event has been dropped since the last call to
// ResetLag.
func (s *Subscription) Lagged() bool { return s.dropped.Load() > 0 }

// ResetLag clears the lagging flag after the subscriber has resynced from its
// durable cursor.
func (s *Subscription) ResetLag() { s.dropped.Store(0) }

// Close removes the subscription from its bus and closes the channel.
func (s *Subscription) Close() { s.bus.Unsubscribe(s) }

func (s *Subscription) deliver(ev *event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- ev:
	default:
		s.dropped.Add(1)
	}
}

// markClosed closes the channel at most once. It is called while holding the
// bus lock, and deliver holds only the subscription lock, so the send and
// close are ordered without a send-on-closed-channel race.
func (s *Subscription) markClosed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}
