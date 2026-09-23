package v1

// Core event types.
//
// Hive normalizes only the semantics it owns. Agent-specific streaming updates
// are deliberately not normalized: text deltas, tool lifecycle, resource updates,
// and protocol-specific notifications stay protocol-native and travel as
// EventAgentRaw unless Hive itself needs to act on them.
const (
	EventMessage             = "message"
	EventStatus              = "status"
	EventRunStarted          = "run.started"
	EventRunFinished         = "run.finished"
	EventPermissionRequested = "permission.requested"
	EventPermissionResponded = "permission.responded"
	EventHandoffCreated      = "handoff.created"
	EventError               = "error"
	EventAgentRaw            = "agent.raw"
)
