package eventbus

import (
	"context"
	"time"

	"github.com/thupham/hive/internal/event"
)

// Outbox supplies durable events that have not reached the bus yet.
type Outbox interface {
	PendingOutbox(ctx context.Context, limit int) ([]*event.Event, error)
	MarkPublishedBatch(ctx context.Context, eventIDs ...string) error
}

// Publisher moves durable events from the outbox to the bus.
//
// Events are published only after they are durable, so a subscriber that has
// seen an event can always find it in the event store. Publication is
// at-least-once: marking is best-effort bookkeeping, so a crash between
// publishing and marking republishes, and consumers deduplicate by event id.
type Publisher struct {
	bus    *Bus
	outbox Outbox
	batch  int
	tick   time.Duration

	// OnError, when set, receives errors from a drain attempt. The loop keeps
	// running: a transient store error must not stop realtime delivery.
	OnError func(error)
}

// NewPublisher returns a publisher with a default batch size and interval.
func NewPublisher(bus *Bus, outbox Outbox) *Publisher {
	return &Publisher{
		bus:    bus,
		outbox: outbox,
		batch:  256,
		tick:   250 * time.Millisecond,
	}
}

// Drain publishes one batch and returns the number of events published.
func (p *Publisher) Drain(ctx context.Context) (int, error) {
	pending, err := p.outbox.PendingOutbox(ctx, p.batch)
	if err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}

	ids := make([]string, 0, len(pending))
	for _, ev := range pending {
		p.bus.Publish(ev)
		ids = append(ids, ev.ID)
	}
	if err := p.outbox.MarkPublishedBatch(ctx, ids...); err != nil {
		return len(pending), err
	}
	return len(pending), nil
}

// Run drains until ctx is cancelled. It returns nil on cancellation.
func (p *Publisher) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.tick)
	defer ticker.Stop()

	for {
		if _, err := p.Drain(ctx); err != nil && p.OnError != nil && ctx.Err() == nil {
			// A cancelled context is an ordinary shutdown, not a delivery
			// failure worth reporting.
			p.OnError(err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
