package v1

import (
	"encoding/json"
	"time"
)

// PluginType categorizes a plugin. The type decides which capabilities the
// core is willing to grant.
type PluginType string

const (
	PluginTypeTransport PluginType = "transport"
	PluginTypeAgent     PluginType = "agent"
	PluginTypeNetwork   PluginType = "network"
	PluginTypeUI        PluginType = "ui"
)

// Valid reports whether t is a known plugin type.
func (t PluginType) Valid() bool {
	switch t {
	case PluginTypeTransport, PluginTypeAgent, PluginTypeNetwork, PluginTypeUI:
		return true
	default:
		return false
	}
}

// Plugin API methods.
const (
	// MethodPluginHello is a plugin's first message on a new connection.
	MethodPluginHello = "plugin.hello"

	// MethodPluginReady is the core's acceptance of a plugin connection.
	MethodPluginReady = "plugin.ready"

	// MethodPluginHealth is a liveness probe.
	MethodPluginHealth = "plugin.health"

	// MethodPluginDrain asks a plugin to stop accepting new work.
	MethodPluginDrain = "plugin.drain"

	// MethodPluginShutdown asks a plugin to exit.
	MethodPluginShutdown = "plugin.shutdown"

	MethodCapabilityRegister = "capability.register"
	MethodCapabilityList     = "capability.list"

	MethodEventSubscribe = "event.subscribe"
	MethodEventAck       = "event.ack"
	MethodEventPublish   = "event.publish"

	MethodSessionCreate  = "session.create"
	MethodSessionPrompt  = "session.prompt"
	MethodSessionCancel  = "session.cancel"
	MethodSessionHandoff = "session.handoff"
	MethodSessionList    = "session.list"
	MethodSessionStatus  = "session.status"

	MethodAgentList = "agent.list"

	MethodPermissionList    = "permission.list"
	MethodPermissionRespond = "permission.respond"
	MethodPermissionRequest = "permission.request"

	MethodExecutionStart  = "execution.start"
	MethodExecutionPrompt = "execution.prompt"
	MethodExecutionCancel = "execution.cancel"
	MethodExecutionReport = "execution.report"

	// MethodExecutionConfig reads or changes a session-level agent selector.
	MethodExecutionConfig = "execution.config"
)

// NotificationEvent delivers an event to a subscribed plugin.
const NotificationEvent = "event.deliver"

// Capabilities a plugin may be granted.
//
// A capability is a coarse permission. A plugin may invoke only methods whose
// required capability it was granted, and a grant never exceeds what its type
// is allowed.
const (
	CapabilitySessionRead     = "session.read"
	CapabilitySessionWrite    = "session.write"
	CapabilityAgentRead       = "agent.read"
	CapabilityEventRead       = "event.read"
	CapabilityEventWrite      = "event.write"
	CapabilityPermissionRead  = "permission.read"
	CapabilityPermissionWrite = "permission.write"
	CapabilityExecutionWrite  = "execution.write"
)

// HelloRequest is a plugin's opening message.
//
// The plugin declares its stable identity and the capabilities it wants. The
// core decides what it is granted; a plugin cannot grant itself authority by
// asking for it.
type HelloRequest struct {
	PluginID        string          `json:"pluginId"`
	Type            PluginType      `json:"type"`
	Version         string          `json:"version"`
	ProtocolVersion ProtocolVersion `json:"protocolVersion"`
	Capabilities    []string        `json:"capabilities"`
}

// HelloResponse is the core's acceptance of a plugin connection.
type HelloResponse struct {
	InstanceID string `json:"instanceId"`

	// ConnectionGeneration identifies this connection for this plugin
	// identity. A superseded generation may not invoke capabilities.
	ConnectionGeneration int64 `json:"connectionGeneration"`

	// Capabilities is the granted subset of what the plugin requested.
	Capabilities []string `json:"capabilities"`
}

// SubscribeRequest asks the core to deliver events.
type SubscribeRequest struct {
	// SessionID scopes the subscription. Empty means every session the plugin
	// is authorized for.
	SessionID string `json:"sessionId,omitempty"`

	// FromSequence resumes from a durable cursor. The core replays from the
	// durable store when the cursor is still valid, and reports a gap when it
	// has been pruned.
	FromSequence int64 `json:"fromSequence,omitempty"`
}

// SubscribeResponse answers a subscribe request.
type SubscribeResponse struct {
	SubscriptionID string `json:"subscriptionId"`

	// FromSequence is the cursor the core will deliver after. It differs from
	// the requested value when the core had to advance past a pruned region.
	FromSequence int64 `json:"fromSequence"`
}

// AckRequest acknowledges delivered events.
type AckRequest struct {
	SubscriptionID string `json:"subscriptionId"`
	Sequence       int64  `json:"sequence"`

	// ConversationID names the transport conversation the event was delivered to.
	// A transport sets it so the durable binding cursor advances with the
	// acknowledgement.
	ConversationID string `json:"conversationId,omitempty"`
}

// DeliveredEvent is an event pushed to a subscribed plugin.
type DeliveredEvent struct {
	SubscriptionID string `json:"subscriptionId"`
	Event          Event  `json:"event"`
}

// ExecutionStartParams asks an agent plugin to start an execution.
type ExecutionStartParams struct {
	AgentRunID string `json:"agentRunId"`
	SessionID  string `json:"sessionId"`

	// AgentID names the agent that must run this execution. A node runs several
	// agents, so the execution surface has to say which one.
	AgentID string `json:"agentId"`

	Generation    int64  `json:"generation"`
	WorkspacePath string `json:"workspacePath"`

	// Context is a preamble the agent should receive before its first prompt,
	// when a handoff carried context into this run. It is rendered by the core so
	// the agent adapter stays protocol-agnostic.
	Context string `json:"context,omitempty"`

	// Config are the session options the user chose, keyed by the agent's own
	// config id. Hive does not know what they mean: the agent declares its
	// selectors and the user picks a value.
	Config map[string]string `json:"config,omitempty"`

	// Resume is the runtime session this run already had, when it had one.
	//
	// An agent session lives in the agent process, so it does not survive a
	// restart. An agent that can restore one gets its own conversation back
	// instead of starting over, which is what makes a restart survivable.
	Resume string `json:"resume,omitempty"`
}

// ExecutionStartResult is what the node reports about a start.
type ExecutionStartResult struct {
	// RuntimeSessionID belongs to the agent runtime.
	RuntimeSessionID string `json:"runtimeSessionId"`

	// Resumed reports that an existing agent session was restored rather than a
	// new one created.
	Resumed bool `json:"resumed,omitempty"`

	// ConfigOptions are the selectors the agent declared.
	ConfigOptions []SessionConfigOption `json:"configOptions,omitempty"`
}

// SessionConfigOption is one selector an agent offers for a session.
//
// It is the agent's own declaration, passed through so a client can render it. A
// model is just a selector whose category is "model".
type SessionConfigOption struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
	Type        string `json:"type,omitempty"`

	// CurrentValue is the value in force. It is a string for a select and a bool
	// for a toggle, so it stays untyped on the wire.
	CurrentValue json.RawMessage `json:"currentValue,omitempty"`

	Options []SessionConfigOptionValue `json:"options,omitempty"`
}

// SessionConfigOptionValue is one choice in a selector.
type SessionConfigOptionValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Config option categories. They are UX hints: a client renders a model selector
// for the model category without knowing anything about models.
const (
	ConfigCategoryMode    = "mode"
	ConfigCategoryModel   = "model"
	ConfigCategoryThought = "thought_level"
)

// ExecutionPromptParams asks an agent plugin to prompt an execution.
type ExecutionPromptParams struct {
	AgentRunID string `json:"agentRunId"`
	AgentID    string `json:"agentId"`
	Generation int64  `json:"generation"`
	Text       string `json:"text"`

	// Context is a preamble for this prompt only, such as the surrounding
	// conversation in a channel.
	Context string `json:"context,omitempty"`

	// Images are pictures sent with the prompt. An agent that does not accept
	// images is told they were attached instead of being sent them.
	Images []PromptImage `json:"images,omitempty"`
}

// ExecutionCancelParams asks an agent plugin to cancel an execution.
type ExecutionCancelParams struct {
	AgentRunID string `json:"agentRunId"`
	AgentID    string `json:"agentId"`
	Generation int64  `json:"generation"`
}

// ExecutionReportParams reports execution state to the core.
//
// The state vocabulary matches the execution reconciliation states, so a
// recovered coordinator can establish what happened to a start it never saw
// acknowledged.
type ExecutionReportParams struct {
	AgentRunID       string `json:"agentRunId"`
	Generation       int64  `json:"generation"`
	State            string `json:"state"`
	RuntimeSessionID string `json:"runtimeSessionId,omitempty"`
	Reason           string `json:"reason,omitempty"`
}

// PublishEventParams publishes an agent-produced event.
//
// Protocol-specific data travels in Payload unmodified, so a new agent protocol
// feature does not require a new core event type.
type PublishEventParams struct {
	// EventID is the producer's stable event identity. The coordinator
	// persists an event at most once by id, so a redelivery is deduplicated
	// rather than duplicated.
	EventID string `json:"eventId"`

	AgentRunID string          `json:"agentRunId"`
	SessionID  string          `json:"sessionId"`
	Type       string          `json:"type"`
	Protocol   string          `json:"protocol,omitempty"`
	Method     string          `json:"method,omitempty"`
	Payload    json.RawMessage `json:"payload"`
}

// PermissionRequestParams asks the core to relay a permission request.
type PermissionRequestParams struct {
	AgentRunID     string          `json:"agentRunId"`
	SessionID      string          `json:"sessionId"`
	AgentRequestID string          `json:"agentRequestId"`
	Payload        json.RawMessage `json:"payload"`

	// ExpiresInSeconds bounds how long the request stays pending. Zero means
	// it does not time out on its own.
	ExpiresInSeconds int `json:"expiresInSeconds,omitempty"`
}

// PermissionRespondParams delivers a permission decision to a plugin.
type PermissionRespondParams struct {
	AgentRunID string `json:"agentRunId"`

	// SessionID scopes the lookup when the caller knows which conversation the
	// request belongs to.
	SessionID      string `json:"sessionId,omitempty"`
	AgentID        string `json:"agentId"`
	AgentRequestID string `json:"agentRequestId"`
	Approved       bool   `json:"approved"`

	// OptionID names the specific choice the user made, when the transport
	// rendered the agent's own options.
	OptionID string `json:"optionId,omitempty"`
}

// Event is the wire form of a durable event.
//
// It mirrors the internal event model but is a separate type on purpose: the
// wire contract is public and versioned, while the internal model may change.
type Event struct {
	ID         string          `json:"id"`
	Sequence   int64           `json:"sequence"`
	SessionID  string          `json:"sessionId"`
	RunID      string          `json:"runId,omitempty"`
	OriginNode string          `json:"originNode,omitempty"`
	Timestamp  time.Time       `json:"timestamp"`
	Type       string          `json:"type"`
	Version    int             `json:"version"`
	Protocol   string          `json:"protocol,omitempty"`
	Method     string          `json:"method,omitempty"`
	Payload    json.RawMessage `json:"payload"`
}

// ExecutionConfigParams reads or changes an agent's session selectors.
//
// An empty ConfigID reads the current options. A value is a string on the wire
// because a selector value is a string or a bool, and the agent decides which.
type ExecutionConfigParams struct {
	AgentRunID string `json:"agentRunId"`
	AgentID    string `json:"agentId"`
	ConfigID   string `json:"configId,omitempty"`
	Value      string `json:"value,omitempty"`
}

// ExecutionConfigResult is the agent's selectors after the change.
type ExecutionConfigResult struct {
	Options []SessionConfigOption `json:"options"`
}
