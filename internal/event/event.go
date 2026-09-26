// Package event defines the durable Hive event model.
//
// Hive normalizes only the semantics it owns. Protocol-specific information
// it does not understand is preserved as raw protocol data rather than
// collapsed into a generic shape.
package event

import (
	"encoding/json"
	"time"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// Core event families.
//
// Agent-specific streaming updates are deliberately not normalized here:
// text deltas, tool lifecycle, resource updates, and protocol-specific
// notifications stay protocol-native and travel as TypeAgentRaw unless Hive
// itself needs to act on them.
const (
	TypeMessage             = v1.EventMessage
	TypeStatus              = v1.EventStatus
	TypeRunStarted          = v1.EventRunStarted
	TypeRunFinished         = v1.EventRunFinished
	TypePermissionRequested = v1.EventPermissionRequested
	TypePermissionResponded = v1.EventPermissionResponded
	TypeHandoffCreated      = v1.EventHandoffCreated
	TypeError               = v1.EventError
	TypeAgentRaw            = v1.EventAgentRaw
	TypeTool                = v1.EventTool
	TypeUsage               = v1.EventUsage
)

// Event is a durable event on a session stream.
//
// ID is the idempotency key: the same ID is persisted at most once.
//
// Sequence is assigned by the authoritative event store when the event
// becomes durable, and is strictly monotonic within SessionID. It is stream
// ordering only. It is not a causal ordering between AgentRuns, nodes, or
// external side effects: a higher sequence does not imply that the later
// event observed, caused, or depends on the earlier one.
type Event struct {
	ID         string
	Sequence   int64
	SessionID  string
	RunID      string
	OriginNode string
	Timestamp  time.Time
	Type       string
	Version    int
	Protocol   string
	Method     string
	Payload    json.RawMessage
}

// Raw builds an event that preserves protocol data Hive does not interpret.
//
// A new agent protocol feature must not require a new core event type, so
// unknown methods are carried as raw data with their protocol and method
// recorded.
func Raw(sessionID, protocol, method string, payload json.RawMessage) *Event {
	return &Event{
		SessionID: sessionID,
		Type:      TypeAgentRaw,
		Version:   1,
		Protocol:  protocol,
		Method:    method,
		Payload:   payload,
	}
}
