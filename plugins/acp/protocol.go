package acp

import "encoding/json"

// ProtocolVersion is the ACP protocol version this adapter implements.
//
// An ACP protocol version is a single integer that is only incremented for
// breaking changes. Non-breaking features arrive as capabilities, so the
// adapter negotiates the version once and then relies on capability flags.
const ProtocolVersion = 1

// ACP method names.
const (
	methodInitialize        = "initialize"
	methodAuthenticate      = "authenticate"
	methodSetConfigOption   = "session/set_config_option"
	methodSessionNew        = "session/new"
	methodSessionLoad       = "session/load"
	methodSessionPrompt     = "session/prompt"
	methodSessionCancel     = "session/cancel"
	methodSessionUpdate     = "session/update"
	methodRequestPermission = "session/request_permission"
)

// Permission option kinds defined by ACP.
const (
	OptionAllow         = "allow"
	OptionDeny          = "deny"
	OptionApplyPatch    = "apply_patch"
	OptionModifyRequest = "modify_request"
)

// Permission outcome variants defined by ACP.
const (
	outcomeSelected  = "selected"
	outcomeCancelled = "cancelled"
)

// Implementation identifies a client or agent to its peer.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// Capabilities is what the agent advertised during initialization.
type Capabilities struct {
	// ProtocolVersion is the negotiated version. It always equals
	// ProtocolVersion, because an unsupported version fails the handshake.
	ProtocolVersion int

	// LoadSession reports that the agent supports session/load, which is what
	// same-agent restore depends on.
	LoadSession bool

	// Prompt capabilities describe richer content the agent accepts.
	PromptImages   bool
	PromptAudio    bool
	PromptEmbedded bool

	AgentName    string
	AgentVersion string

	// AuthMethods are the ways this agent accepts authentication. A non-empty
	// list means the agent refuses to create a session until the client
	// authenticates.
	AuthMethods []AuthMethod

	// ConfigOptions are the selectors the agent offers for a session. Hive renders
	// them without knowing what they mean.
	ConfigOptions []ConfigOption
}

type initializeRequest struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities clientCapabilities `json:"clientCapabilities"`
	ClientInfo         *Implementation    `json:"clientInfo,omitempty"`
}

// clientCapabilities advertises what Hive can do.
//
// Hive advertises no filesystem, terminal, or elicitation support: the agent
// owns tool execution, and claiming a capability Hive does not implement would
// make the agent depend on it.
type clientCapabilities struct {
	FS       fsCapabilities `json:"fs"`
	Terminal bool           `json:"terminal"`
}

type fsCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type initializeResponse struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AgentInfo         *Implementation   `json:"agentInfo,omitempty"`
	AuthMethods       []AuthMethod      `json:"authMethods,omitempty"`
}

// ConfigOption is one session-level setting the agent offers.
//
// The agent declares these, and a client renders a selector for each. Hive does
// not know what a model is: it knows the agent offers a selector with the model
// category, which is the whole point of the protocol.
type ConfigOption struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Description  string              `json:"description,omitempty"`
	Category     string              `json:"category,omitempty"`
	Type         string              `json:"type,omitempty"`
	CurrentValue any                 `json:"currentValue,omitempty"`
	Options      []ConfigOptionValue `json:"options,omitempty"`
}

// ConfigOptionValue is one choice in a selector.
type ConfigOptionValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Config option categories. They are UX hints, not semantics: a client renders a
// model selector for CategoryModel without knowing anything about models.
const (
	CategoryMode        = "mode"
	CategoryModel       = "model"
	CategoryModelConfig = "model_config"
	CategoryThought     = "thought_level"
)

type setConfigOptionRequest struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     any    `json:"value"`
}

// AuthMethod is one way an agent can authenticate.
//
// An agent that advertises auth methods refuses to create a session until the
// client has authenticated. Hive does not own agent credentials, so which method
// to use is configuration.
type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type authenticateRequest struct {
	MethodID string `json:"methodId"`

	// Meta carries protocol extensions. An agent that authenticates with an API
	// key reads it from here.
	Meta json.RawMessage `json:"_meta,omitempty"`
}

type agentCapabilities struct {
	LoadSession        bool               `json:"loadSession"`
	PromptCapabilities promptCapabilities `json:"promptCapabilities"`
}

type promptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// NewSessionResult is the outcome of creating a session.
type NewSessionResult struct {
	SessionID     string
	ConfigOptions []ConfigOption
}

// NewSessionRequest asks the agent to create a session.
type NewSessionRequest struct {
	// Cwd is the workspace path the session runs in.
	Cwd string `json:"cwd"`

	// McpServers is required by the schema; Hive configures none in v1.
	McpServers []json.RawMessage `json:"mcpServers"`

	// AdditionalDirectories requests extra workspace roots when the agent
	// advertises support.
	AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
}

type newSessionResponse struct {
	SessionID string `json:"sessionId"`

	// ConfigOptions are the selectors this agent offers for the session, such as
	// the model or the session mode.
	ConfigOptions []ConfigOption `json:"configOptions,omitempty"`
}

// LoadSessionRequest asks the agent to restore a session it created earlier.
//
// The session id belongs to this runtime. Hive must never reinterpret another
// runtime's session id as this one's.
type LoadSessionRequest struct {
	SessionID  string            `json:"sessionId"`
	Cwd        string            `json:"cwd"`
	McpServers []json.RawMessage `json:"mcpServers"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type promptRequest struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

type promptResponse struct {
	StopReason string `json:"stopReason"`
}

type cancelNotification struct {
	SessionID string `json:"sessionId"`
}

// Update is a session notification, preserved as raw protocol data.
//
// Hive normalizes only the semantics it owns, so agent-specific streaming
// updates are carried through without a Hive schema change.
type Update struct {
	SessionID string
	// Method is the protocol-native notification method.
	Method string
	// Payload is the raw protocol data, unmodified.
	Payload json.RawMessage
}

// PermissionRequest is a session/request_permission call from the agent.
//
// Hive treats the request as opaque except for the metadata required to route
// and correlate the response.
type PermissionRequest struct {
	// RequestID is the raw JSON-RPC id of the agent's request. It is echoed
	// verbatim when responding.
	RequestID json.RawMessage

	SessionID string

	// ToolCall is the agent's description of the pending operation, preserved
	// unmodified.
	ToolCall json.RawMessage

	// Options are the choices the agent offered.
	Options []PermissionOption
}

// PermissionOption is one choice the agent offered.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// PermissionOutcome is the response to a permission request.
type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// OutcomeForDecision maps an approval decision onto an ACP outcome using the
// options the agent offered.
//
// Hive decides approved or denied; expressing that in ACP is the adapter's
// job, because only the adapter knows ACP's vocabulary. When optionID is set
// it is used directly, because the transport may have rendered the agent's own
// options to the user. Otherwise the first option matching the decision kind
// is selected.
//
// When nothing matches, the request is cancelled rather than answered with an
// option the agent did not offer: the agent then aborts the operation instead
// of proceeding with an approval the user did not give.
func OutcomeForDecision(options []PermissionOption, approved bool, optionID string) PermissionOutcome {
	if optionID != "" {
		for _, opt := range options {
			if opt.OptionID == optionID {
				return PermissionOutcome{Outcome: outcomeSelected, OptionID: opt.OptionID}
			}
		}
		return PermissionOutcome{Outcome: outcomeCancelled}
	}

	want := OptionDeny
	if approved {
		want = OptionAllow
	}
	for _, opt := range options {
		if opt.Kind == want {
			return PermissionOutcome{Outcome: outcomeSelected, OptionID: opt.OptionID}
		}
	}
	return PermissionOutcome{Outcome: outcomeCancelled}
}
