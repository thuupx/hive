package zalo

// The Zalo Bot API's inbound shapes.
//
// They are the platform's, not Hive's: the transport decodes what Zalo sends and
// decides what it means. Nothing here reaches the core except through the
// transport-neutral envelope.

// User is who sent a message.
type User struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	IsBot       bool   `json:"is_bot"`
}

// Chat is the conversation a message belongs to.
//
// Zalo has two kinds: a private 1:1 chat and a group. There are no threads, so a
// chat is the whole conversation.
type Chat struct {
	ID string `json:"id"`

	// ChatType is "PRIVATE" or "GROUP".
	ChatType string `json:"chat_type"`
}

// Chat types.
const (
	ChatPrivate = "PRIVATE"
	ChatGroup   = "GROUP"
)

// IsGroup reports whether the conversation is a group.
func (c Chat) IsGroup() bool { return c.ChatType == ChatGroup }

// InboundMessage is one inbound message.
//
// The fields are per event kind: a text message carries Text, an image message
// carries Photo and Caption, a sticker carries Sticker and URL, and a voice
// message carries VoiceURL. A field the event does not carry is empty.
type InboundMessage struct {
	From User `json:"from"`
	Chat Chat `json:"chat"`

	Text string `json:"text"`

	// Photo is the image URL of an image message.
	Photo string `json:"photo"`

	// Caption is the text sent with an image message.
	Caption string `json:"caption"`

	// Sticker is the sticker code, and URL is where it is.
	Sticker string `json:"sticker"`
	URL     string `json:"url"`

	// VoiceURL is where a voice message's audio is.
	VoiceURL string `json:"voice_url"`

	MessageID string `json:"message_id"`
	Date      int64  `json:"date"`
}

// Update is one inbound delivery, from getUpdates or a webhook.
//
// EventName is the platform's name for what happened, and Message is set for the
// message events.
type Update struct {
	EventName string          `json:"event_name"`
	Message   *InboundMessage `json:"message"`
}

// Event names the transport handles.
const (
	EventText        = "message.text.received"
	EventImage       = "message.image.received"
	EventSticker     = "message.sticker.received"
	EventVoice       = "message.voice.received"
	EventUnsupported = "message.unsupported.received"
)

// Bot is the bot's own identity, as getMe reports it.
type Bot struct {
	ID          string `json:"id"`
	AccountName string `json:"account_name"`
	AccountType string `json:"account_type"`
	DisplayName string `json:"display_name"`
	CanJoin     bool   `json:"can_join_groups"`
}

// Message is what the transport renders and the client sends.
//
// Zalo renders one text field. ParseMode is "markdown" for formatted answers and
// empty for plain text, which is what a message full of code fences wants: the
// markup would be read as formatting and the code would arrive mangled.
type Message struct {
	Text      string
	ParseMode string
}

// Limits, which a message must respect or Zalo rejects it.
const (
	// MaxTextChars is the most Zalo accepts in one sendMessage text.
	MaxTextChars = 2000

	// ChunkChars is how much of that a posted chunk may use. The remainder is
	// headroom for the markup a parse_mode message strips, which still counts
	// toward the platform's limit.
	ChunkChars = 1900
)

// DefaultMaxAttachmentBytes bounds how large an attachment may be read.
const DefaultMaxAttachmentBytes = 8 << 20
