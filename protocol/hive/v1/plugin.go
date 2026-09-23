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
}

// DeliveredEvent is an event pushed to a subscribed plugin.
type DeliveredEvent struct {
	SubscriptionID string `json:"subscriptionId"`
	Event          Event  `json:"event"`
}

// ExecutionStartParams asks an agent plugin to start an execution.
type ExecutionStartParams struct {
	AgentRunID    string `json:"agentRunId"`
	SessionID     string `json:"sessionId"`
	Generation    int64  `json:"generation"`
	WorkspacePath string `json:"workspacePath"`
}

// ExecutionPromptParams asks an agent plugin to prompt an execution.
type ExecutionPromptParams struct {
	AgentRunID string `json:"agentRunId"`
	Generation int64  `json:"generation"`
	Text       string `json:"text"`
}

// ExecutionCancelParams asks an agent plugin to cancel an execution.
type ExecutionCancelParams struct {
	AgentRunID string `json:"agentRunId"`
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
	AgentRunID     string `json:"agentRunId"`
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
