package storage

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound reports that a requested record does not exist.
var ErrNotFound = errors.New("storage: not found")

// Session is the persisted form of a Hive Session.
//
// Session state transitions are validated by the domain layer, not here.
// DefaultInteractiveRunID is a domain field used by deterministic prompt
// routing; it is not metadata.
type Session struct {
	ID                      string
	State                   string
	WorkspaceID             string
	DefaultInteractiveRunID string
	Metadata                json.RawMessage
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// Event is a durable session-stream event.
//
// ID is the idempotency key: the same ID is persisted at most once.
// Sequence is assigned by the store when the event becomes durable and is
// strictly monotonic within SessionID. It is stream ordering only, not a
// causal ordering between runs or nodes.
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

// CommandState is the lifecycle state of a mutating command.
type CommandState string

const (
	CommandReceived  CommandState = "received"
	CommandAccepted  CommandState = "accepted"
	CommandCompleted CommandState = "completed"
	CommandRejected  CommandState = "rejected"
	CommandFailed    CommandState = "failed"
	CommandRetryable CommandState = "retryable"
)

// Command is the durable record of a mutating command.
//
// ID is the idempotency key for the logical operation and is stable across
// retries. SourceID correlates the command with the originating transport
// delivery when the transport provides a stable identifier. AcceptedTerm
// is always 0 in v1 and is reserved for coordinator failover.
type Command struct {
	ID           string
	Actor        string
	SourceID     string
	Method       string
	AcceptedTerm int64
	Target       string
	State        CommandState
	Result       json.RawMessage
	Error        json.RawMessage
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Snapshot is a point-in-time representation of the context required to
// resume or hand off a session.
//
// SchemaVersion identifies the context schema used to build the snapshot so
// a later version can migrate or reject it explicitly.
type Snapshot struct {
	ID            string
	SessionID     string
	SchemaVersion int
	Sequence      int64
	Payload       json.RawMessage
	CreatedAt     time.Time
}
