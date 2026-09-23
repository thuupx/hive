package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Slack Web API endpoints.
const (
	connectionsOpenURL = "https://slack.com/api/apps.connections.open"
	postMessageURL     = "https://slack.com/api/chat.postMessage"
	updateMessageURL   = "https://slack.com/api/chat.update"
	historyURL         = "https://slack.com/api/conversations.history"
	addReactionURL     = "https://slack.com/api/reactions.add"
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

// Inbound is one inbound delivery, still in Slack form.
type Inbound struct {
	// Type is the Slack event type, such as "message" or "block_actions".
	Type    string
	Payload json.RawMessage
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
type SocketClient struct {
	config Config
	http   *http.Client
	log    *slog.Logger

	mu     sync.Mutex
	conn   *websocket.Conn
	closed bool
}

// NewSocketClient returns a live Slack client.
func NewSocketClient(config Config) (*SocketClient, error) {
	switch {
	case config.AppToken == "":
		return nil, errors.New("slack: an app token is required for Socket Mode")
	case config.BotToken == "":
		return nil, errors.New("slack: a bot token is required")
	}
	if config.BaseURL == "" {
		config.BaseURL = "https://slack.com/api"
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if config.Log == nil {
		config.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &SocketClient{config: config, http: config.HTTPClient, log: config.Log}, nil
}

// Events opens a Socket Mode connection and streams inbound deliveries.
//
// Socket Mode means no public endpoint and no inbound tunnel is needed, which is
// what makes it the right default for a personal installation.
func (c *SocketClient) Events(ctx context.Context) (<-chan Inbound, error) {
	url, err := c.openConnection(ctx)
	if err != nil {
		return nil, err
	}

	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("slack: dial socket mode: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	out := make(chan Inbound, 64)
	go c.readLoop(ctx, conn, out)
	return out, nil
}

func (c *SocketClient) openConnection(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(connectionsOpenURL), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.config.AppToken)

	var response struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := c.do(req, &response); err != nil {
		return "", err
	}
	if !response.OK {
		return "", fmt.Errorf("slack: apps.connections.open failed: %s", response.Error)
	}
	return response.URL, nil
}

func (c *SocketClient) readLoop(ctx context.Context, conn *websocket.Conn, out chan<- Inbound) {
	defer close(out)
	defer conn.Close(websocket.StatusNormalClosure, "")

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}

		var envelope Envelope
		if err := json.Unmarshal(data, &envelope); err != nil {
			continue
		}

		// Acknowledge every envelope. Slack redelivers an unacknowledged
		// envelope, and Hive deduplicates by the delivery's source id, so an
		// acknowledgement lost in flight is safe.
		if envelope.EnvelopeID != "" {
			ack, _ := json.Marshal(map[string]string{"envelope_id": envelope.EnvelopeID})
			if err := conn.Write(ctx, websocket.MessageText, ack); err != nil {
				return
			}
		}

		// Every envelope is reported, because an envelope Hive does not handle is
		// the answer to "why did nothing happen" and would otherwise leave no trace.
		c.log.Debug("socket envelope", "type", envelope.Type)

		switch envelope.Type {
		case "events_api", "interactive":
			var wrapper struct {
				Event   json.RawMessage `json:"event"`
				Actions json.RawMessage `json:"actions"`
			}
			_ = json.Unmarshal(envelope.Payload, &wrapper)

			payload := wrapper.Event
			kind := "events_api"
			if len(wrapper.Actions) > 0 {
				payload = envelope.Payload
				kind = "block_actions"
			}

			select {
			case out <- Inbound{Type: kind, Payload: payload}:
			case <-ctx.Done():
				return
			}

		case "disconnect":
			// Slack asks the client to reconnect. Ending the stream lets the
			// caller reconnect with a fresh URL.
			return
		}
	}
}

// PostMessage posts a message through the Web API.
func (c *SocketClient) PostMessage(ctx context.Context, req PostMessageRequest) (string, error) {
	body := map[string]any{
		"channel": req.Channel,
		"text":    req.Message.Text,
	}
	if len(req.Message.Blocks) > 0 {
		blocks, err := json.Marshal(req.Message.Blocks)
		if err != nil {
			return "", err
		}
		body["blocks"] = json.RawMessage(blocks)
	}
	if req.ThreadTS != "" {
		body["thread_ts"] = req.ThreadTS
	}

	var response struct {
		OK        bool   `json:"ok"`
		Timestamp string `json:"ts"`
		Error     string `json:"error"`
	}
	if err := c.post(ctx, postMessageURL, body, &response); err != nil {
		return "", err
	}
	if !response.OK {
		return "", fmt.Errorf("slack: chat.postMessage failed: %s", response.Error)
	}
	return response.Timestamp, nil
}

// ChannelHistory reads a conversation through the Web API.
func (c *SocketClient) ChannelHistory(ctx context.Context, channel string, limit int) ([]HistoryMessage, error) {
	if limit <= 0 {
		limit = 20
	}

	url := fmt.Sprintf("%s?channel=%s&limit=%d", c.endpoint(historyURL), url.QueryEscape(channel), limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.config.BotToken)

	var response struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Messages []struct {
			User  string `json:"user"`
			BotID string `json:"bot_id"`
			Text  string `json:"text"`
			TS    string `json:"ts"`
		} `json:"messages"`
	}
	if err := c.do(req, &response); err != nil {
		return nil, err
	}
	if !response.OK {
		return nil, fmt.Errorf("slack: conversations.history failed: %s", response.Error)
	}

	out := make([]HistoryMessage, 0, len(response.Messages))
	for _, message := range response.Messages {
		out = append(out, HistoryMessage{
			Author:    message.User,
			Text:      message.Text,
			Timestamp: message.TS,
			Self:      message.BotID != "",
		})
	}
	return out, nil
}

// DownloadFile fetches a file through the Web API.
//
// The size is bounded: a transport must not read an unbounded amount of a user's
// data into memory because they attached something large.
func (c *SocketClient) DownloadFile(ctx context.Context, url string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxAttachmentBytes
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.config.BotToken)

	response, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack: download: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("slack: download returned %d", response.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("slack: download: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("slack: file is larger than %d bytes", maxBytes)
	}
	return data, nil
}

// DefaultMaxAttachmentBytes bounds how large an attachment may be.
const DefaultMaxAttachmentBytes = 8 << 20

// UpdateMessage edits a message through the Web API.
func (c *SocketClient) UpdateMessage(ctx context.Context, req UpdateMessageRequest) error {
	body := map[string]any{
		"channel": req.Channel,
		"ts":      req.Timestamp,
		"text":    req.Message.Text,
	}
	if len(req.Message.Blocks) > 0 {
		blocks, err := json.Marshal(req.Message.Blocks)
		if err != nil {
			return err
		}
		body["blocks"] = json.RawMessage(blocks)
	}

	var response struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := c.post(ctx, updateMessageURL, body, &response); err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("slack: chat.update failed: %s", response.Error)
	}
	return nil
}

// AddReaction adds a reaction through the Web API.
func (c *SocketClient) AddReaction(ctx context.Context, req ReactionRequest) error {
	body := map[string]any{
		"channel":   req.Channel,
		"timestamp": req.Timestamp,
		"name":      req.Name,
	}

	var response struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := c.post(ctx, addReactionURL, body, &response); err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("slack: reactions.add failed: %s", response.Error)
	}
	return nil
}

// Close releases the connection.
func (c *SocketClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true
	if c.conn == nil {
		return nil
	}
	return c.conn.Close(websocket.StatusNormalClosure, "")
}

func (c *SocketClient) post(ctx context.Context, url string, body any, target any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(url), bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.config.BotToken)

	return c.do(req, target)
}

func (c *SocketClient) do(req *http.Request, target any) error {
	response, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("slack: %s: %w", req.URL.Path, err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("slack: read %s: %w", req.URL.Path, err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("slack: %s returned %d", req.URL.Path, response.StatusCode)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("slack: decode %s: %w", req.URL.Path, err)
	}
	return nil
}

func (c *SocketClient) endpoint(fallback string) string {
	if c.config.BaseURL == "" || c.config.BaseURL == "https://slack.com/api" {
		return fallback
	}
	switch fallback {
	case connectionsOpenURL:
		return c.config.BaseURL + "/apps.connections.open"
	case postMessageURL:
		return c.config.BaseURL + "/chat.postMessage"
	case updateMessageURL:
		return c.config.BaseURL + "/chat.update"
	case historyURL:
		return c.config.BaseURL + "/conversations.history"
	case addReactionURL:
		return c.config.BaseURL + "/reactions.add"
	default:
		return fallback
	}
}
