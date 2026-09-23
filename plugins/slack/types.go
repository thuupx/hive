package slack

import (
	"encoding/json"
	"strings"
)

// MessageEvent is a Slack message event.
type MessageEvent struct {
	Type      string `json:"type"`
	SubType   string `json:"subtype,omitempty"`
	Channel   string `json:"channel"`
	User      string `json:"user"`
	Text      string `json:"text"`
	Timestamp string `json:"ts"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	BotID     string `json:"bot_id,omitempty"`

	// Files are what the user attached. A message with files arrives with the
	// file_share subtype, which is otherwise not a message a user typed.
	Files []File `json:"files,omitempty"`
}

// File is a file attached to a message.
type File struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mimetype"`
	Size     int    `json:"size"`

	// URLPrivateDownload is where the bytes are. Slack requires the bot token to
	// read it, so it is not a public link.
	URLPrivateDownload string `json:"url_private_download"`
	URLPrivate         string `json:"url_private"`
}

// DownloadURL is where to fetch the file from.
func (f File) DownloadURL() string {
	if f.URLPrivateDownload != "" {
		return f.URLPrivateDownload
	}
	return f.URLPrivate
}

// IsImage reports whether the file is a picture.
func (f File) IsImage() bool {
	return strings.HasPrefix(f.MimeType, "image/")
}

// ActionPayload is a Slack block action.
type ActionPayload struct {
	Type string `json:"type"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	ActionTS string `json:"action_ts"`
	Message  struct {
		TS string `json:"ts"`

		// ThreadTS is the thread the message is in, which is the conversation the
		// interaction belongs to. Slack omits it for a message that is not in a
		// thread.
		ThreadTS string `json:"thread_ts"`
	} `json:"message"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

// Action returns the first action in the payload.
func (p ActionPayload) Action() (actionID, value string) {
	if len(p.Actions) == 0 {
		return "", ""
	}
	return p.Actions[0].ActionID, p.Actions[0].Value
}

// Block Kit types, kept minimal: the transport renders Hive events into them and
// nothing else in the system needs to know their shape.

// Message is a Slack message payload.
type Message struct {
	Text   string  `json:"text"`
	Blocks []Block `json:"blocks,omitempty"`
}

// Block is a Block Kit block.
type Block struct {
	Type     string      `json:"type"`
	Text     *TextObject `json:"text,omitempty"`
	Elements []Element   `json:"elements,omitempty"`
	BlockID  string      `json:"block_id,omitempty"`
}

// TextObject is a Block Kit text object.
type TextObject struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Element is a Block Kit interactive element.
type Element struct {
	Type     string      `json:"type"`
	Text     *TextObject `json:"text,omitempty"`
	ActionID string      `json:"action_id,omitempty"`
	Value    string      `json:"value,omitempty"`
	Style    string      `json:"style,omitempty"`
}

// Envelope is one inbound Socket Mode envelope.
type Envelope struct {
	EnvelopeID string          `json:"envelope_id"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
}
