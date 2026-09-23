package v1

import "encoding/json"

// Meta carries Hive-specific context that is not part of JSON-RPC. It is
// an additive extension to the envelope.
//
// Authentication is transport-specific. Actor is a claimed identity used
// for diagnostics and routing context and must never be treated as proof
// of identity; the transport establishes the authenticated principal.
type Meta struct {
	ProtocolVersion *ProtocolVersion `json:"protocolVersion,omitempty"`
	Actor           string           `json:"actor,omitempty"`
	Capabilities    []string         `json:"capabilities,omitempty"`
	SessionID       string           `json:"sessionId,omitempty"`
	RunID           string           `json:"runId,omitempty"`
	CommandID       string           `json:"commandId,omitempty"`
}

// Message is a single JSON-RPC 2.0 message.
//
// A message is a request when Method is set and ID is valid, a
// notification when Method is set and ID is unset, and a response when
// Method is empty and ID is valid.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *ID             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
	Meta    *Meta           `json:"meta,omitempty"`
}

// Kind classifies a message.
type Kind uint8

const (
	KindInvalid Kind = iota
	KindRequest
	KindNotification
	KindResponse
)

func (k Kind) String() string {
	switch k {
	case KindRequest:
		return "request"
	case KindNotification:
		return "notification"
	case KindResponse:
		return "response"
	default:
		return "invalid"
	}
}

// Kind returns the message kind.
func (m *Message) Kind() Kind {
	if m == nil {
		return KindInvalid
	}
	hasID := m.ID != nil && m.ID.Valid()
	switch {
	case m.Method != "" && hasID:
		return KindRequest
	case m.Method != "":
		return KindNotification
	case hasID && (m.Error != nil || m.Result != nil):
		return KindResponse
	default:
		return KindInvalid
	}
}

// RequestID returns the message identifier, or the zero ID when unset.
func (m *Message) RequestID() ID {
	if m == nil || m.ID == nil {
		return ID{}
	}
	return *m.ID
}

// NewRequest builds a request. A nil params value omits the params member.
func NewRequest(id ID, method string, params any) (*Message, error) {
	raw, err := marshalOptional(params)
	if err != nil {
		return nil, err
	}
	return &Message{JSONRPC: JSONRPCVersion, ID: &id, Method: method, Params: raw}, nil
}

// NewNotification builds a notification. A nil params value omits the
// params member.
func NewNotification(method string, params any) (*Message, error) {
	raw, err := marshalOptional(params)
	if err != nil {
		return nil, err
	}
	return &Message{JSONRPC: JSONRPCVersion, Method: method, Params: raw}, nil
}

// NewResult builds a successful response. A nil result becomes an explicit
// JSON null so the response always carries a result member.
func NewResult(id ID, result any) (*Message, error) {
	raw, err := marshalRequired(result)
	if err != nil {
		return nil, err
	}
	return &Message{JSONRPC: JSONRPCVersion, ID: &id, Result: raw}, nil
}

// NewErrorResponse builds an error response.
func NewErrorResponse(id ID, e *Error) *Message {
	return &Message{JSONRPC: JSONRPCVersion, ID: &id, Error: e}
}

// WithMeta attaches Hive context to the message and returns it.
func (m *Message) WithMeta(meta *Meta) *Message {
	m.Meta = meta
	return m
}

// Validate reports whether the message is a well-formed JSON-RPC 2.0
// message that Hive accepts.
func (m *Message) Validate() error {
	if m == nil {
		return InvalidRequest("empty message")
	}
	if m.JSONRPC != JSONRPCVersion {
		return InvalidRequest("unsupported jsonrpc version %q, want %q", m.JSONRPC, JSONRPCVersion)
	}
	switch m.Kind() {
	case KindRequest, KindNotification:
		if m.Result != nil || m.Error != nil {
			return InvalidRequest("request and notification must not carry result or error")
		}
	case KindResponse:
		if m.Result == nil && m.Error == nil {
			return InvalidRequest("response must carry result or error")
		}
		if m.Result != nil && m.Error != nil {
			return InvalidRequest("response must not carry both result and error")
		}
	default:
		return InvalidRequest("message is neither request, notification, nor response")
	}
	return nil
}

// ParseMessage decodes and validates a single JSON-RPC message.
func ParseMessage(data []byte) (*Message, error) {
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, NewErrorf(CodeParseError, "%s", err.Error())
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func marshalOptional(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, Internal("encode params: %s", err.Error())
	}
	return b, nil
}

func marshalRequired(v any) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage("null"), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, Internal("encode result: %s", err.Error())
	}
	return b, nil
}
