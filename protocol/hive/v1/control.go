package v1

import (
	"encoding/json"
	"time"
)

// Control API parameter and result types.
//
// The Control API shares method names with the plugin-invokable methods: the
// same operation reached over a different transport is the same operation, so
// `session.create` means the same thing whether a client or a transport plugin
// asks for it.
//
// Every mutating call carries a command id. It is the idempotency key and is
// stable across retries of the same logical operation, so a retry resolves to the
// existing operation rather than creating a second one.

// Control-only method names.
//
// The rest of the Control API shares its method names with the
// plugin-invokable set, because the same operation reached over a different
// transport is the same operation.
const (
	// MethodNodeList lists the nodes the coordinator has heard from.
	MethodNodeList = "node.list"

	// MethodCommandGet reads a command status resource.
	MethodCommandGet = "command.get"

	// MethodSessionEvents replays a session event stream from a cursor.
	MethodSessionEvents = "session.events"

	// MethodWorkspaceCreate registers a workspace.
	MethodWorkspaceCreate = "workspace.create"

	// MethodWorkspaceList lists workspaces and shared-location warnings.
	MethodWorkspaceList = "workspace.list"

	// MethodSessionConfig reads or changes a session's agent selectors, such as
	// the model or the session mode.
	MethodSessionConfig = "session.config"
)

// SessionConfigParams reads or changes a session's agent selectors.
//
// An empty ConfigID reads them. A change is durable on the session, so a new
// AgentRun continues with the same selection.
type SessionConfigParams struct {
	CommandID string `json:"commandId,omitempty"`
	SessionID string `json:"sessionId"`
	RunID     string `json:"runId,omitempty"`
	ConfigID  string `json:"configId,omitempty"`
	Value     string `json:"value,omitempty"`
	SourceID  string `json:"sourceId,omitempty"`
}

// SessionConfigResult is a session's agent selectors after the change.
type SessionConfigResult struct {
	SessionID string                `json:"sessionId"`
	AgentID   string                `json:"agentId"`
	Options   []SessionConfigOption `json:"options"`
}

// SessionCreateParams creates a session and its first AgentRun.
type SessionCreateParams struct {
	CommandID string `json:"commandId"`

	// AgentID selects the agent. Empty uses the configured default agent.
	AgentID string `json:"agentId,omitempty"`

	// Workspace names the workspace the run works in.
	Workspace string `json:"workspace,omitempty"`

	// Metadata is opaque client metadata stored with the session.
	Metadata json.RawMessage `json:"metadata,omitempty"`

	// SourceID correlates the command with the originating transport delivery
	// when the transport has a stable identifier.
	SourceID string `json:"sourceId,omitempty"`
}

// SessionCreateResult is the outcome of session.create.
type SessionCreateResult struct {
	CommandID string `json:"commandId"`
	SessionID string `json:"sessionId"`
	RunID     string `json:"runId"`
	AgentID   string `json:"agentId"`
	NodeID    string `json:"nodeId"`
	RunState  string `json:"runState"`
}

// SessionPromptParams prompts an AgentRun.
type SessionPromptParams struct {
	CommandID string `json:"commandId"`
	SessionID string `json:"sessionId"`

	// RunID targets a specific AgentRun. Empty applies the session routing
	// rule, which is deterministic and never depends on timestamps.
	RunID string `json:"runId,omitempty"`

	Text     string `json:"text"`
	SourceID string `json:"sourceId,omitempty"`
}

// SessionPromptResult is the outcome of session.prompt.
type SessionPromptResult struct {
	CommandID string `json:"commandId"`
	RunID     string `json:"runId"`

	// CreatedRun reports that routing had to create a new AgentRun.
	CreatedRun bool `json:"createdRun"`
}

// SessionHandoffParams transfers a session to another agent.
//
// A handoff creates a new AgentRun rather than migrating an existing agent
// process, and the source and target runtime session ids stay distinct fields.
type SessionHandoffParams struct {
	CommandID string `json:"commandId"`
	SessionID string `json:"sessionId"`

	// RunID names the source AgentRun. Empty uses the session's default run.
	RunID string `json:"runId,omitempty"`

	// AgentID selects the target agent.
	AgentID string `json:"agentId"`

	// Summary is an optional source-agent summary. A handoff must remain valid
	// when the source cannot generate one.
	Summary string `json:"summary,omitempty"`

	Workspace string `json:"workspace,omitempty"`
	SourceID  string `json:"sourceId,omitempty"`
}

// SessionHandoffResult is the outcome of session.handoff.
type SessionHandoffResult struct {
	CommandID   string `json:"commandId"`
	HandoffID   string `json:"handoffId"`
	SessionID   string `json:"sessionId"`
	SourceRunID string `json:"sourceRunId,omitempty"`
	TargetRunID string `json:"targetRunId"`
	AgentID     string `json:"agentId"`
	NodeID      string `json:"nodeId"`
	State       string `json:"state"`
}

// SessionCancelParams cancels the current turn of an AgentRun.
type SessionCancelParams struct {
	CommandID string `json:"commandId"`
	SessionID string `json:"sessionId"`
	RunID     string `json:"runId,omitempty"`
	SourceID  string `json:"sourceId,omitempty"`
}

// SessionStatusParams asks for one session.
type SessionStatusParams struct {
	SessionID string `json:"sessionId"`
}

// SessionStatusResult describes a session and its runs.
type SessionStatusResult struct {
	SessionID    string       `json:"sessionId"`
	State        string       `json:"state"`
	Workspace    string       `json:"workspace,omitempty"`
	DefaultRunID string       `json:"defaultRunId,omitempty"`
	Runs         []RunSummary `json:"runs"`
}

// RunSummary is a compact view of an AgentRun.
type RunSummary struct {
	RunID               string `json:"runId"`
	AgentID             string `json:"agentId"`
	NodeID              string `json:"nodeId,omitempty"`
	State               string `json:"state"`
	ExecutionGeneration int64  `json:"executionGeneration"`

	// RuntimeSessionID belongs to one runtime. Hive never reinterprets one
	// runtime's session id as another's.
	RuntimeSessionID string `json:"runtimeSessionId,omitempty"`
}

// SessionListParams lists sessions.
type SessionListParams struct {
	Limit int `json:"limit,omitempty"`
}

// SessionListResult lists sessions.
type SessionListResult struct {
	Sessions []SessionSummary `json:"sessions"`
}

// SessionSummary is a compact view of a session.
type SessionSummary struct {
	SessionID string    `json:"sessionId"`
	State     string    `json:"state"`
	Runs      int       `json:"runs"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// AgentListResult lists the configured agents.
type AgentListResult struct {
	Agents []AgentSummary `json:"agents"`
}

// AgentSummary is a compact view of a configured agent.
type AgentSummary struct {
	ID       string `json:"id"`
	Protocol string `json:"protocol"`
}

// NodeListResult lists the nodes the coordinator has heard from.
type NodeListResult struct {
	Nodes []NodeSummary `json:"nodes"`
}

// NodeSummary is a compact view of a node.
type NodeSummary struct {
	NodeID               string    `json:"nodeId"`
	Version              string    `json:"version,omitempty"`
	ConnectionGeneration int64     `json:"connectionGeneration"`
	Connected            bool      `json:"connected"`
	Executions           int       `json:"executions"`
	LastSeen             time.Time `json:"lastSeen"`
}

// WorkspaceCreateParams registers a workspace.
type WorkspaceCreateParams struct {
	CommandID string `json:"commandId"`
	Name      string `json:"name"`

	// Locations map a node to the path the workspace lives at there. The same
	// workspace may live at different paths on different machines.
	Locations map[string]string `json:"locations,omitempty"`
}

// WorkspaceSummary is a compact view of a workspace.
type WorkspaceSummary struct {
	WorkspaceID string            `json:"workspaceId"`
	Name        string            `json:"name"`
	Locations   map[string]string `json:"locations,omitempty"`
	ActiveRuns  int               `json:"activeRuns"`
}

// WorkspaceListResult lists workspaces.
type WorkspaceListResult struct {
	Workspaces []WorkspaceSummary `json:"workspaces"`

	// Warnings report locations several active runs reference. They are warnings,
	// never locks: concurrent runs on one location are a real workflow.
	Warnings []WorkspaceWarning `json:"warnings,omitempty"`
}

// WorkspaceWarning describes a workspace location several active runs share.
type WorkspaceWarning struct {
	WorkspaceID string   `json:"workspaceId"`
	NodeID      string   `json:"nodeId"`
	Path        string   `json:"path"`
	RunIDs      []string `json:"runIds"`
}

// PermissionListParams lists pending permission requests.
type PermissionListParams struct {
	// SessionID scopes the listing. Empty lists every session the caller may
	// see.
	SessionID string `json:"sessionId,omitempty"`

	Limit int `json:"limit,omitempty"`
}

// PermissionListResult lists pending permission requests.
type PermissionListResult struct {
	Permissions []PermissionSummary `json:"permissions"`
}

// PermissionSummary is a compact view of a pending permission request.
type PermissionSummary struct {
	PermissionID   string    `json:"permissionId"`
	SessionID      string    `json:"sessionId"`
	RunID          string    `json:"runId,omitempty"`
	AgentRequestID string    `json:"agentRequestId"`
	CreatedAt      time.Time `json:"createdAt"`
	ExpiresAt      time.Time `json:"expiresAt,omitempty"`
}

// CommandGetParams asks for a command record.
type CommandGetParams struct {
	CommandID string `json:"commandId"`
}

// CommandResult is the command status resource.
//
// An asynchronous operation is queried here rather than by holding a
// request/response open until it completes.
type CommandResult struct {
	CommandID string          `json:"commandId"`
	Method    string          `json:"method"`
	State     string          `json:"state"`
	Target    string          `json:"target,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     json.RawMessage `json:"error,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// EventReplayParams replays a session event stream.
type EventReplayParams struct {
	SessionID    string `json:"sessionId"`
	FromSequence int64  `json:"fromSequence"`
	Limit        int    `json:"limit,omitempty"`
}

// EventReplayResult is the replay outcome.
type EventReplayResult struct {
	Events []Event `json:"events"`

	// Gap is set when the requested cursor has been pruned. The client must
	// rehydrate from the snapshot boundary rather than skip history silently.
	Gap *CursorGap `json:"gap,omitempty"`
}

// CursorGap is the wire form of a pruned cursor.
type CursorGap struct {
	Requested        int64  `json:"requested"`
	NextSequence     int64  `json:"nextSequence"`
	SnapshotID       string `json:"snapshotId,omitempty"`
	SnapshotSequence int64  `json:"snapshotSequence,omitempty"`
}
