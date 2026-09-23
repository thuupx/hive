package v1

import (
	"encoding/json"
	"strings"
	"time"
)

// Transport API method.
//
// A transport normalizes platform input into a transport-neutral envelope and
// hands it to Hive. The transport owns syntax; Hive owns semantics.
const (
	// MethodTransportInbound delivers a normalized inbound platform delivery.
	MethodTransportInbound = "transport.inbound"

	// CapabilityTransportInbound lets a transport plugin deliver inbound input.
	CapabilityTransportInbound = "transport.inbound"
)

// EnvelopeKind is the category of inbound platform input.
type EnvelopeKind string

const (
	// EnvelopeMessage is ordinary user text, which becomes a prompt.
	EnvelopeMessage EnvelopeKind = "message"

	// EnvelopeCommand is a platform command, which becomes a Hive operation.
	EnvelopeCommand EnvelopeKind = "command"

	// EnvelopeInteraction is a button, menu, or modal submission.
	EnvelopeInteraction EnvelopeKind = "interaction"
)

// Envelope is the transport-neutral form of inbound platform input.
//
// These types live in the protocol package rather than under internal/ because a
// transport plugin builds them: a plugin depends on the public protocol surface,
// not on the core.
type Envelope struct {
	Transport      string
	ConversationID string
	Principal      string

	// SourceID is the platform's stable identifier for the inbound delivery when
	// one exists, such as a Slack event id. It is what makes transport delivery
	// itself idempotent: a redelivery reuses SourceID, while a user deliberately
	// repeating an action produces a new one.
	SourceID string

	Kind EnvelopeKind

	Message     *IncomingMessage
	Command     *IncomingCommand
	Interaction *IncomingInteraction

	// ChannelContext is what else is being said in the conversation.
	ChannelContext []ChannelMessage
}

// IncomingMessage is ordinary user text, optionally with files.
type IncomingMessage struct {
	Text string

	// Attachments are files the user sent. The transport fetches them, because
	// reading the platform is its job; the core only carries the bytes.
	Attachments []Attachment
}

// Attachment is a file the user sent with a message.
type Attachment struct {
	// Name is the original file name.
	Name string `json:"name,omitempty"`

	// MimeType is the media type, for example image/png.
	MimeType string `json:"mimeType,omitempty"`

	// URL is where the transport fetched it from. It is the transport's own
	// bookkeeping and is not sent to the core.
	URL string `json:"-"`

	// Data is the file itself, base64 encoded on the wire.
	Data []byte `json:"data,omitempty"`
}

// IsImage reports whether the attachment is an image.
func (a Attachment) IsImage() bool {
	return strings.HasPrefix(a.MimeType, "image/")
}

// IncomingCommand is a platform command.
//
// Name is the transport's own command name, such as "new_chat". Method is the
// Hive operation the transport mapped it to. The transport resolves the alias, so
// `/new_chat` is a presentation-level name and `session.create` is the operation.
//
// An empty Method means the transport recognized the syntax but exposes no such
// operation, which is reported as a command error rather than forwarding the text
// as a prompt.
type IncomingCommand struct {
	Name   string
	Method string
	Args   []string

	// ConfigID names the agent selector the command targets, when it targets one.
	//
	// The transport knows that "/model" means the selector called model; Hive does
	// not have to. That is what lets an agent offer a new selector without a Hive
	// change.
	ConfigID string
}

// IncomingInteraction is a platform control.
type IncomingInteraction struct {
	Action string
	Value  string
}

// TransportInboundParams is a normalized inbound delivery.
type TransportInboundParams struct {
	Transport      string `json:"transport"`
	ConversationID string `json:"conversationId"`
	Principal      string `json:"principal"`
	SourceID       string `json:"sourceId,omitempty"`

	// Kind is "message", "command", or "interaction".
	Kind string `json:"kind"`

	// Message fields.
	Text        string       `json:"text,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`

	// Command fields.
	Command  string   `json:"command,omitempty"`
	Method   string   `json:"method,omitempty"`
	Args     []string `json:"args,omitempty"`
	ConfigID string   `json:"configId,omitempty"`

	// Interaction fields.
	Action string `json:"action,omitempty"`
	Value  string `json:"value,omitempty"`

	// ChannelContext is what else is being said in the conversation.
	//
	// A channel is not a private 1:1 pipe: a message means what it means in the
	// context of the room. The transport reads that context, because reading the
	// platform is its job, and the core decides what to do with it.
	ChannelContext []ChannelMessage `json:"channelContext,omitempty"`
}

// ChannelMessage is one thing said in a conversation.
type ChannelMessage struct {
	Author string    `json:"author,omitempty"`
	Text   string    `json:"text"`
	At     time.Time `json:"at,omitempty"`
	// Self marks a message this bot sent, so a prompt does not read back its own
	// output as if a person had said it.
	Self bool `json:"self,omitempty"`
}

// TransportOutcome is what an inbound delivery produced.
//
// An operation failure travels in Error rather than as a protocol error, because
// the transport has to render it to the user. A protocol error would mean the
// delivery itself was malformed.
type TransportOutcome struct {
	Method    string          `json:"method,omitempty"`
	CommandID string          `json:"commandId,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	RunID     string          `json:"runId,omitempty"`
	Created   bool            `json:"created,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *Error          `json:"error,omitempty"`
}
