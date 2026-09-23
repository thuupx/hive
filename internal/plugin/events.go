package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/thupham/hive/internal/event"
	"github.com/thupham/hive/internal/eventbus"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// EventBuffer is the bus subscription depth used for plugin delivery.
const EventBuffer = 1024

// Events serves the plugin event subscription surface.
//
// Delivery is at-least-once. A plugin acknowledges the highest sequence it has
// processed, and an unacknowledged event is delivered again rather than lost. A
// plugin that cannot keep up loses realtime delivery and recovers from the
// durable store using its cursor.
type Events struct {
	bus *eventbus.Bus
	sup *Supervisor
	log *slog.Logger

	mu     sync.Mutex
	next   int64
	byID   map[string]*subscription
	byInst map[*Instance]map[string]*subscription
}

type subscription struct {
	id        string
	instance  *Instance
	sessionID string

	mu    sync.Mutex
	acked int64
}

// NewEvents returns an event delivery surface for plugin subscriptions.
func NewEvents(bus *eventbus.Bus, sup *Supervisor, log *slog.Logger) *Events {
	return &Events{
		bus:    bus,
		sup:    sup,
		log:    log,
		byID:   make(map[string]*subscription),
		byInst: make(map[*Instance]map[string]*subscription),
	}
}

// Register installs the subscribe and acknowledge handlers on a supervisor.
func (e *Events) Register(sup *Supervisor) {
	sup.Handle(v1.MethodEventSubscribe, e.subscribe)
	sup.Handle(v1.MethodEventAck, e.ack)
}

// Run forwards bus events to subscribed plugins until ctx is cancelled.
func (e *Events) Run(ctx context.Context) error {
	sub := e.bus.Subscribe("plugin-events", EventBuffer)
	defer sub.Close()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-sub.C():
			if !ok {
				return nil
			}
			e.deliver(ev)
		}
	}
}

// SubscriptionCount returns the number of live subscriptions.
func (e *Events) SubscriptionCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.byID)
}

// AckedThrough returns the highest sequence acknowledged by an instance.
func (e *Events) AckedThrough(inst *Instance) int64 {
	e.mu.Lock()
	subs := e.byInst[inst]
	e.mu.Unlock()

	var highest int64
	for _, s := range subs {
		s.mu.Lock()
		if s.acked > highest {
			highest = s.acked
		}
		s.mu.Unlock()
	}
	return highest
}

// Forget drops every subscription of an instance.
func (e *Events) Forget(inst *Instance) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id := range e.byInst[inst] {
		delete(e.byID, id)
	}
	delete(e.byInst, inst)
}

func (e *Events) subscribe(_ context.Context, call Call) (any, error) {
	var req v1.SubscribeRequest
	if len(call.Params) > 0 {
		if err := json.Unmarshal(call.Params, &req); err != nil {
			return nil, v1.InvalidParams("invalid subscribe request")
		}
	}

	e.mu.Lock()
	e.next++
	sub := &subscription{
		id:        fmt.Sprintf("sub_%d", e.next),
		instance:  call.Instance,
		sessionID: req.SessionID,
		acked:     req.FromSequence,
	}
	e.byID[sub.id] = sub
	if e.byInst[call.Instance] == nil {
		e.byInst[call.Instance] = make(map[string]*subscription)
	}
	e.byInst[call.Instance][sub.id] = sub
	e.mu.Unlock()

	return v1.SubscribeResponse{SubscriptionID: sub.id, FromSequence: req.FromSequence}, nil
}

func (e *Events) ack(_ context.Context, call Call) (any, error) {
	var req v1.AckRequest
	if err := json.Unmarshal(call.Params, &req); err != nil {
		return nil, v1.InvalidParams("invalid ack request")
	}

	e.mu.Lock()
	sub := e.byID[req.SubscriptionID]
	e.mu.Unlock()

	if sub == nil {
		return nil, v1.NotFound("unknown subscription %s", req.SubscriptionID)
	}
	if sub.instance != call.Instance {
		return nil, v1.Unauthorized("subscription %s belongs to another instance", req.SubscriptionID)
	}

	sub.mu.Lock()
	if req.Sequence > sub.acked {
		sub.acked = req.Sequence
	}
	sub.mu.Unlock()

	return map[string]any{"acked": req.Sequence}, nil
}

func (e *Events) deliver(ev *event.Event) {
	e.mu.Lock()
	targets := make([]*subscription, 0, len(e.byID))
	for _, sub := range e.byID {
		if sub.sessionID != "" && sub.sessionID != ev.SessionID {
			continue
		}
		targets = append(targets, sub)
	}
	e.mu.Unlock()

	for _, sub := range targets {
		// Re-authorizing on every delivery also drops a superseded instance:
		// after a restart the old connection must stop receiving events.
		if err := e.sup.Authorize(sub.instance, v1.MethodEventSubscribe); err != nil {
			e.Forget(sub.instance)
			continue
		}

		sub.mu.Lock()
		alreadyAcked := ev.Sequence <= sub.acked
		sub.mu.Unlock()
		if alreadyAcked {
			continue
		}

		if err := sub.instance.Notify(v1.NotificationEvent, v1.DeliveredEvent{
			SubscriptionID: sub.id,
			Event:          wireEvent(ev),
		}); err != nil {
			e.log.Debug("event delivery failed", "plugin", sub.instance.PluginID, "error", err)
		}
	}
}

func wireEvent(ev *event.Event) v1.Event {
	return v1.Event{
		ID:         ev.ID,
		Sequence:   ev.Sequence,
		SessionID:  ev.SessionID,
		RunID:      ev.RunID,
		OriginNode: ev.OriginNode,
		Timestamp:  ev.Timestamp,
		Type:       ev.Type,
		Version:    ev.Version,
		Protocol:   ev.Protocol,
		Method:     ev.Method,
		Payload:    ev.Payload,
	}
}
