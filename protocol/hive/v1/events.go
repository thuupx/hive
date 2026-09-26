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

	// EventTool is a normalized agent tool call or one of its updates.
	//
	// Hive normalizes tool activity and nothing else from an agent stream, because
	// tool activity is what makes an agent observable: a user needs to see what it
	// is doing, and a conversation that shows only the final answer is a black box.
	EventTool = "tool"

	// EventUsage is what a turn cost, published when the turn ends.
	//
	// It is normalized because a user asking what a conversation costs should not
	// have to read an agent's protocol, and the numbers are the agent's own.
	EventUsage = "usage"
)

// Usage is what one turn cost and how full the context is.
//
// Every field is optional: an agent reports what it reports, and a zero means it
// did not report that number rather than that the number was zero.
type Usage struct {
	// InputTokens and OutputTokens are this turn's prompt and completion counts.
	InputTokens  int `json:"inputTokens,omitempty"`
	OutputTokens int `json:"outputTokens,omitempty"`

	// ThoughtTokens is the part of the output spent reasoning, when the agent
	// separates it.
	ThoughtTokens int `json:"thoughtTokens,omitempty"`

	// CachedReadTokens is the part of the input served from a prompt cache, which
	// is what makes a long conversation affordable.
	CachedReadTokens int `json:"cachedReadTokens,omitempty"`

	// TotalTokens is the turn's total as the agent counted it. It is used when
	// the agent reports one, because a provider's total is not always the sum of
	// its parts.
	TotalTokens int `json:"totalTokens,omitempty"`

	// ContextUsed and ContextSize are how much of the context window the
	// conversation now occupies.
	ContextUsed int `json:"contextUsed,omitempty"`
	ContextSize int `json:"contextSize,omitempty"`
}

// Reported reports whether the agent gave any number at all.
func (u Usage) Reported() bool {
	return u.InputTokens != 0 || u.OutputTokens != 0 || u.TotalTokens != 0 ||
		u.ContextUsed != 0 || u.ContextSize != 0
}

// Turn is the turn's total: the agent's own, or the sum of its parts.
func (u Usage) Turn() int {
	if u.TotalTokens != 0 {
		return u.TotalTokens
	}
	return u.InputTokens + u.OutputTokens
}

// ToolCall is the normalized form of an agent tool call.
//
// It carries only what a client needs to display the activity. The agent's own
// content stays in the raw stream, so no vendor schema reaches the core.
type ToolCall struct {
	// ToolCallID is the agent's own identifier, stable across its updates. It is
	// what lets a client keep one message per tool call instead of one per update.
	ToolCallID string `json:"toolCallId"`

	// Title is what the agent calls the operation.
	Title string `json:"title,omitempty"`

	// Kind is the agent's own category: execute, edit, read, search, and so on.
	Kind string `json:"kind,omitempty"`

	// Status is the lifecycle: pending, in_progress, completed, failed.
	Status string `json:"status,omitempty"`

	// Summary is one short human-readable line.
	Summary string `json:"summary,omitempty"`

	// Started reports that this is the first update for the tool call, which is
	// when a client creates the message it will keep updating.
	Started bool `json:"started,omitempty"`
}

// Tool status values.
const (
	ToolPending    = "pending"
	ToolInProgress = "in_progress"
	ToolCompleted  = "completed"
	ToolFailed     = "failed"
)
