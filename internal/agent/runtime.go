package agent

import "context"

// Runtime is the small interface Hive requires from an agent runtime.
//
// It must stay small and capability-focused. It carries no agent-specific tool
// methods: running shells, writing files, checking out Git, installing
// packages, and browser automation belong to the underlying agent runtime, not
// to Hive.
type Runtime interface {
	// Start prepares the runtime for use.
	Start(ctx context.Context) error

	// Stop releases the runtime.
	Stop(ctx context.Context) error

	// CreateSession starts a new runtime session.
	CreateSession(ctx context.Context, req SessionRequest) (RuntimeSession, error)

	// ResumeSession restores a previously created runtime session.
	ResumeSession(ctx context.Context, req ResumeRequest) (RuntimeSession, error)

	// Prompt sends a prompt to a runtime session.
	Prompt(ctx context.Context, session RuntimeSession, prompt Prompt) error

	// Cancel cancels the current turn of a runtime session.
	Cancel(ctx context.Context, session RuntimeSession) error

	// RespondToRequest answers a pending permission request.
	RespondToRequest(ctx context.Context, resp RequestResponse) error
}

// SessionRequest asks a runtime to create a session.
type SessionRequest struct {
	AgentRunID    string
	WorkspacePath string
}

// ResumeRequest asks a runtime to restore a session.
//
// RuntimeSessionID is the identifier of the runtime that owns the session.
// Hive must not reinterpret one runtime's session identifier as another's, so
// resuming always stays within the same runtime.
type ResumeRequest struct {
	AgentRunID       string
	RuntimeSessionID string
	WorkspacePath    string
}

// RuntimeSession is a live session inside an agent runtime.
type RuntimeSession interface {
	// ID is the runtime's own session identifier.
	ID() string

	// Updates streams runtime notifications for this session.
	Updates() <-chan Update
}

// Update is a runtime notification that Hive does not interpret.
//
// Hive normalizes only the semantics it owns, so text deltas, tool lifecycle,
// resource updates, and protocol-specific notifications stay protocol-native.
// This is what prevents schema explosion as an agent protocol grows.
type Update struct {
	RunID    string
	Protocol string
	Method   string
	Payload  []byte
}

// Prompt is user intent sent to a runtime session.
type Prompt struct {
	Text string
}

// RequestResponse answers a permission request.
type RequestResponse struct {
	RequestID string
	Outcome   Outcome
}

// Outcome is the decision relayed back to the agent runtime.
//
// Hive decides approved or denied; expressing that decision in a specific
// agent protocol is the adapter's job, because only the adapter knows the
// protocol's own vocabulary.
type Outcome string

const (
	OutcomeApproved Outcome = "approved"
	OutcomeDenied   Outcome = "denied"
)

// ExecutionState is the known state of a run's execution on a node.
//
// It exists so the node execution controller can resolve a lost start
// acknowledgement without starting a second execution.
type ExecutionState string

const (
	// ExecutionAbsent means no execution exists for the generation.
	ExecutionAbsent ExecutionState = "absent"

	// ExecutionStarting means an execution exists but has not started running.
	ExecutionStarting ExecutionState = "starting"

	// ExecutionRunning means an execution is live.
	ExecutionRunning ExecutionState = "running"

	// ExecutionTerminal means the execution has ended.
	ExecutionTerminal ExecutionState = "terminal"

	// ExecutionAmbiguous means the state cannot be established safely. Hive
	// must not start another execution merely to make sure one is running.
	ExecutionAmbiguous ExecutionState = "ambiguous"
)

// Reconciler is an optional runtime capability.
//
// A runtime need not implement it. When it does, the node execution controller
// uses it to correlate a Hive AgentRun and execution generation with the
// runtime's own view, instead of inferring success from a local start flag.
type Reconciler interface {
	ExecutionState(ctx context.Context, agentRunID string, generation int64) (ExecutionState, error)
}
