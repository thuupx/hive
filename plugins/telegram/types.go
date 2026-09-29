package telegram

// The Telegram Bot API's inbound shapes.
//
// They are the platform's, not Hive's: the transport decodes what Telegram
// sends and decides what it means. Nothing here reaches the core except
// through the transport-neutral envelope.

// User is who sent a message or pressed a button.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// Chat is the conversation a message belongs to.
//
// Telegram has private chats, groups, supergroups, and channels. A supergroup
// can be a forum, where messages carry a thread id naming their topic.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// Chat types.
const (
	ChatPrivate    = "private"
	ChatGroup      = "group"
	ChatSupergroup = "supergroup"
	ChatChannel    = "channel"
)

// IsGroup reports whether the conversation is a group.
func (c Chat) IsGroup() bool { return c.Type == ChatGroup || c.Type == ChatSupergroup }

// Entity is a structured mention Telegram parsed out of a message.
//
// Telegram parses commands, mentions, and links itself, so the transport reads
// them rather than sniffing text.
type Entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

// Entity types the transport reads.
const (
	EntityBotCommand = "bot_command"
	EntityMention    = "mention"
)

// PhotoSize is one rendition of a picture. A photo arrives as an array, small
// to large.
type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

// Document is a file a user sent.
type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

// Voice is a voice note.
type Voice struct {
	FileID   string `json:"file_id"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

// InboundMessage is one inbound message.
type InboundMessage struct {
	MessageID int64 `json:"message_id"`
	From      User  `json:"from"`
	Chat      Chat  `json:"chat"`

	Text     string   `json:"text"`
	Caption  string   `json:"caption"`
	Entities []Entity `json:"entities"`

	// CaptionEntities is the parsed structure of a media caption.
	CaptionEntities []Entity `json:"caption_entities"`

	// Photo is the picture's renditions, small to large.
	Photo []PhotoSize `json:"photo"`

	Document *Document `json:"document"`
	Voice    *Voice    `json:"voice"`

	// ReplyTo is the message this one answers, when the user replied.
	ReplyTo *InboundMessage `json:"reply_to_message"`

	// ThreadID is the forum topic the message belongs to. Zero means the chat
	// is not a forum, or the message is in the General topic.
	ThreadID int64 `json:"message_thread_id"`

	Date int64 `json:"date"`
}

// CallbackQuery is a button press on a message the bot sent.
type CallbackQuery struct {
	ID      string          `json:"id"`
	From    User            `json:"from"`
	Message *InboundMessage `json:"message"`
	Data    string          `json:"data"`
}

// Update is one inbound delivery, from getUpdates or a webhook.
type Update struct {
	UpdateID      int64           `json:"update_id"`
	Message       *InboundMessage `json:"message"`
	CallbackQuery *CallbackQuery  `json:"callback_query"`
}

// Bot is the bot's own identity, as getMe reports it.
type Bot struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
	CanJoin   bool   `json:"can_join_groups"`
}

// File is what getFile returns: where the platform stores an attachment.
type File struct {
	FileID   string `json:"file_id"`
	FilePath string `json:"file_path"`
	FileSize int64  `json:"file_size"`
}

// Message is what the transport renders and the client sends.
type Message struct {
	Text      string
	ParseMode string

	// Images are the pictures the message carries. Telegram draws no image
	// inside a text message, so a markdown image an agent writes is sent as a
	// photo of its own.
	Images []OutboundImage

	// Keyboard is the inline button rows under the message, used by a
	// permission request. Empty means no buttons.
	Keyboard [][]InlineButton

	// ThreadID posts the message into a forum topic. Zero is the chat itself.
	ThreadID int64
}

// InlineButton is one button under a message.
type InlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// OutboundImage is a picture a message refers to.
type OutboundImage struct {
	// Alt is the caption the markdown image declared.
	Alt string

	// Target is where the image is: an http(s) URL the platform fetches, or a
	// local path the transport uploads.
	Target string
}

// Limits, which a message must respect or Telegram rejects it.
const (
	// MaxTextChars is the most Telegram accepts in one sendMessage text.
	MaxTextChars = 4096

	// ChunkChars is how much of that a posted chunk may use. The remainder is
	// headroom for the markup an HTML message carries, which counts toward the
	// limit.
	ChunkChars = 3900

	// CaptionChars is the most Telegram accepts as a photo caption.
	CaptionChars = 1024
)

// DefaultMaxAttachmentBytes bounds how large an attachment may be read.
//
// getFile serves files up to 20 MB, so the default sits under that.
const DefaultMaxAttachmentBytes = 8 << 20
