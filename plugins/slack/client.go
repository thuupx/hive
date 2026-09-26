package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"strings"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// Client is the Slack API surface the transport needs.
//
// Keeping it an interface is what makes the transport testable: the parsing,
// routing, and rendering above it are exercised against a fake, and only the live
// Socket Mode connection needs real credentials.
type Client interface {
	// Events streams inbound deliveries until ctx is cancelled.
	Events(ctx context.Context) (<-chan Inbound, error)

	// PostMessage posts a message and returns its timestamp.
	PostMessage(ctx context.Context, req PostMessageRequest) (string, error)

	// UpdateMessage replaces the content of a message already posted.
	//
	// It is what keeps a tool call to one message instead of one per update.
	UpdateMessage(ctx context.Context, req UpdateMessageRequest) error

	// AddReaction adds a reaction to a message. It is best-effort: a failure must
	// not fail the underlying Hive operation.
	AddReaction(ctx context.Context, req ReactionRequest) error

	// ChannelHistory reads the recent messages of a conversation, newest first.
	ChannelHistory(ctx context.Context, channel string, limit int) ([]HistoryMessage, error)

	// DownloadFile fetches a file. Slack requires the bot token to read it, so it
	// is not a plain HTTP fetch.
	DownloadFile(ctx context.Context, url string, maxBytes int64) ([]byte, error)

	// Close releases the connection.
	Close() error
}

// Inbound is one inbound delivery, decoded by the Slack library.
//
// Exactly one field is set: an Events API delivery carries Events, and a button
// press carries Interaction. The library decides how a delivery is framed; the
// transport decides what it means.
type Inbound struct {
	// Events is an Events API delivery, such as a message.
	Events *slackevents.EventsAPIEvent

	// Interaction is a block action: a button someone pressed.
	Interaction *slackgo.InteractionCallback

	// Payload is the envelope as Slack sent it.
	//
	// It is here for the one field the library's event types omit: a message
	// carries its attachments as "files", and only the app-mention event declares
	// them. Dropping a user's attachment because of a gap in a type would be a
	// silent loss, so that one field is read from the payload instead.
	Payload json.RawMessage
}

// Type is what Slack called this delivery, for logging.
func (i Inbound) Type() string {
	switch {
	case i.Interaction != nil:
		return string(slackgo.InteractionTypeBlockActions)
	case i.Events != nil:
		return i.Events.Type
	default:
		return "unknown"
	}
}

// PostMessageRequest is a message to post.
type PostMessageRequest struct {
	Channel  string
	ThreadTS string
	Message  Message
}

// UpdateMessageRequest replaces a posted message.
type UpdateMessageRequest struct {
	Channel   string
	Timestamp string
	Message   Message
}

// HistoryMessage is one message read from a conversation.
type HistoryMessage struct {
	Author    string
	Text      string
	Timestamp string
	Self      bool
}

// ReactionRequest is a reaction to add.
type ReactionRequest struct {
	Channel   string
	Timestamp string
	Name      string
}

// Config configures the live client.
type Config struct {
	// AppToken authenticates the Socket Mode connection.
	AppToken string

	// BotToken authenticates Web API calls.
	BotToken string

	// BaseURL overrides the Slack API origin. It exists for tests.
	BaseURL string

	// HTTPClient overrides the HTTP client.
	HTTPClient *http.Client

	// Log reports what arrives over the socket. A delivery that is never seen
	// cannot be explained by anything else, and "the button did nothing" is
	// exactly that case.
	Log *slog.Logger
}

// SocketClient is the live Slack client.
//
// The Socket Mode connection belongs to the library. It reconnects when Slack
// asks it to and when a connection fails, and it treats a missing WebSocket ping
// as a dead connection — none of which a hand-written read loop notices. A
// transport that has silently stopped receiving is the failure that is hardest to
// see, and the one this library exists to prevent.
type SocketClient struct {
	api    *slackgo.Client
	socket *socketmode.Client
	log    *slog.Logger
}

// NewSocketClient returns a live Slack client.
func NewSocketClient(config Config) (*SocketClient, error) {
	switch {
	case config.AppToken == "":
		return nil, errors.New("slack: an app token is required for Socket Mode")
	case config.BotToken == "":
		return nil, errors.New("slack: a bot token is required")
	}
	if config.Log == nil {
		config.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	options := []slackgo.Option{
		// The bot token is the credential for the Web API; the app token is what
		// opens the socket, which is the one call that needs it.
		slackgo.OptionAppLevelToken(config.AppToken),
		// The library's own log goes to the transport's, at debug: it is where a
		// reconnect or a refused acknowledgement is explained.
		slackgo.OptionLog(log.New(libraryWriter{log: config.Log}, "", 0)),
	}
	if config.HTTPClient != nil {
		options = append(options, slackgo.OptionHTTPClient(config.HTTPClient))
	}
	if config.BaseURL != "" {
		options = append(options, slackgo.OptionAPIURL(config.BaseURL))
	}

	api := slackgo.New(config.BotToken, options...)
	return &SocketClient{api: api, socket: socketmode.New(api), log: config.Log}, nil
}

// libraryWriter sends the library's log to the transport's.
type libraryWriter struct{ log *slog.Logger }

func (w libraryWriter) Write(p []byte) (int, error) {
	w.log.Debug("slack library", "message", strings.TrimSpace(string(p)))
	return len(p), nil
}

// Events opens a Socket Mode connection and streams inbound deliveries.
//
// Socket Mode means no public endpoint and no inbound tunnel is needed, which is
// what makes it the right default for a personal installation.
func (c *SocketClient) Events(ctx context.Context) (<-chan Inbound, error) {
	out := make(chan Inbound, 64)
	done := make(chan struct{})

	// RunContext blocks until the context ends, reconnecting in between. Its
	// return is the end of the socket, and the stream is closed with it: a
	// transport that kept reading a channel nobody will write to again would look
	// alive while receiving nothing, which is the failure this library was adopted
	// to end.
	go func() {
		defer close(done)
		if err := c.socket.RunContext(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("slack socket mode stopped", "error", err)
		}
	}()

	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case event, ok := <-c.socket.Events:
				if !ok {
					return
				}
				c.dispatch(ctx, event, out)
			}
		}
	}()
	return out, nil
}

// dispatch acknowledges one delivery and hands it on.
func (c *SocketClient) dispatch(ctx context.Context, event socketmode.Event, out chan<- Inbound) {
	// Every envelope is acknowledged before it is handled. Slack redelivers an
	// unacknowledged envelope, and Hive deduplicates by the delivery's source id,
	// so an acknowledgement lost in flight is safe — and acknowledging late is
	// what makes Slack resend.
	if event.Request != nil {
		if err := c.socket.Ack(*event.Request); err != nil {
			c.log.Debug("could not acknowledge a delivery", "error", err)
		}
	}

	var inbound Inbound
	if event.Request != nil {
		inbound.Payload = event.Request.Payload
	}
	switch event.Type {
	case socketmode.EventTypeEventsAPI:
		parsed, ok := event.Data.(slackevents.EventsAPIEvent)
		if !ok {
			c.log.Debug("slack sent an unreadable event")
			return
		}
		inbound.Events = &parsed

	case socketmode.EventTypeInteractive:
		callback, ok := event.Data.(slackgo.InteractionCallback)
		if !ok {
			c.log.Debug("slack sent an unreadable interaction")
			return
		}
		inbound.Interaction = &callback

	case socketmode.EventTypeConnected:
		// The socket is up. This is the line that says Slack is reachable, and its
		// absence is what a silent transport looks like from the outside.
		c.log.Info("slack socket mode connected")
		return

	case socketmode.EventTypeConnectionError:
		c.log.Warn("slack socket mode connection failed; it is retried")
		return

	default:
		// A delivery Hive does not handle is the answer to "why did nothing
		// happen", and would otherwise leave no trace.
		c.log.Debug("slack sent an unhandled delivery", "type", string(event.Type))
		return
	}

	select {
	case out <- inbound:
	case <-ctx.Done():
	}
}

// PostMessage posts a message through the Web API.
func (c *SocketClient) PostMessage(ctx context.Context, req PostMessageRequest) (string, error) {
	options := messageOptions(req.Message)
	if req.ThreadTS != "" {
		options = append(options, slackgo.MsgOptionTS(req.ThreadTS))
	}

	_, timestamp, err := c.api.PostMessageContext(ctx, req.Channel, options...)
	if err != nil {
		return "", fmt.Errorf("slack: chat.postMessage: %w", err)
	}
	return timestamp, nil
}

// UpdateMessage edits a message through the Web API.
func (c *SocketClient) UpdateMessage(ctx context.Context, req UpdateMessageRequest) error {
	if _, _, _, err := c.api.UpdateMessageContext(ctx, req.Channel, req.Timestamp, messageOptions(req.Message)...); err != nil {
		return fmt.Errorf("slack: chat.update: %w", err)
	}
	return nil
}

// messageOptions renders a message as what the API takes.
func messageOptions(message Message) []slackgo.MsgOption {
	options := []slackgo.MsgOption{slackgo.MsgOptionText(message.Text, false)}
	if len(message.Blocks) > 0 {
		options = append(options, slackgo.MsgOptionBlocks(message.Blocks...))
	}
	return options
}

// AddReaction adds a reaction through the Web API.
func (c *SocketClient) AddReaction(ctx context.Context, req ReactionRequest) error {
	item := slackgo.ItemRef{Channel: req.Channel, Timestamp: req.Timestamp}
	if err := c.api.AddReactionContext(ctx, req.Name, item); err != nil {
		return fmt.Errorf("slack: reactions.add: %w", err)
	}
	return nil
}

// ChannelHistory reads a conversation through the Web API.
func (c *SocketClient) ChannelHistory(ctx context.Context, channel string, limit int) ([]HistoryMessage, error) {
	if limit <= 0 {
		limit = 20
	}

	response, err := c.api.GetConversationHistoryContext(ctx, &slackgo.GetConversationHistoryParameters{
		ChannelID: channel,
		Limit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("slack: conversations.history: %w", err)
	}

	out := make([]HistoryMessage, 0, len(response.Messages))
	for _, message := range response.Messages {
		out = append(out, HistoryMessage{
			Author:    message.User,
			Text:      message.Text,
			Timestamp: message.Timestamp,
			Self:      message.BotID != "",
		})
	}
	return out, nil
}

// DefaultMaxAttachmentBytes bounds how large an attachment may be.
const DefaultMaxAttachmentBytes = 8 << 20

// DownloadFile fetches a file through the Web API.
//
// The size is bounded: a transport must not read an unbounded amount of a user's
// data into memory because they attached something large.
func (c *SocketClient) DownloadFile(ctx context.Context, url string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxAttachmentBytes
	}

	var buffer bytes.Buffer
	if err := c.api.GetFileContext(ctx, url, &boundedWriter{writer: &buffer, remaining: maxBytes, limit: maxBytes}); err != nil {
		return nil, fmt.Errorf("slack: download: %w", err)
	}
	return buffer.Bytes(), nil
}

// boundedWriter refuses a download that exceeds its bound.
//
// It errors rather than truncating: a truncated attachment would reach the agent
// as a corrupt file, which is worse than a refusal it can report.
type boundedWriter struct {
	writer    io.Writer
	remaining int64
	limit     int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("slack: file is larger than %d bytes", w.limit)
	}
	w.remaining -= int64(len(p))
	return w.writer.Write(p)
}

// Close releases the connection.
//
// The socket's lifetime is the context Events was given: the library's RunContext
// returns when it ends, so there is nothing else to close here.
func (c *SocketClient) Close() error { return nil }
