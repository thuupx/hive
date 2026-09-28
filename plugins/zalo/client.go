package zalo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the Zalo Bot API origin.
const DefaultBaseURL = "https://bot-api.zaloplatforms.com"

// DefaultPollTimeout is how long a getUpdates call waits for a message.
//
// Zalo's own default is 30 seconds. Long polling is what makes the transport
// need no public endpoint, which is the property that makes it workable on a
// laptop behind NAT.
const DefaultPollTimeout = 30 * time.Second

// Client is the Zalo Bot API surface the transport needs.
//
// Keeping it an interface is what makes the transport testable: parsing,
// routing, and rendering are exercised against a fake, and only the live
// connection needs a real token.
type Client interface {
	// Events streams inbound deliveries until ctx is cancelled.
	Events(ctx context.Context) (<-chan Update, error)

	// SendMessage posts a text message and returns its id.
	SendMessage(ctx context.Context, req SendMessageRequest) (string, error)

	// SendPhoto posts an image message and returns its id.
	SendPhoto(ctx context.Context, req SendPhotoRequest) (string, error)

	// SendChatAction shows a transient status in a conversation, such as
	// "typing". It is best-effort: a failure must not fail the turn.
	SendChatAction(ctx context.Context, chatID, action string) error

	// DownloadFile fetches an attachment. Zalo serves media from a CDN, so it
	// is a plain HTTP fetch, bounded in size.
	DownloadFile(ctx context.Context, url string, maxBytes int64) ([]byte, error)

	// Close releases the connection.
	Close() error
}

// SendMessageRequest is a message to post.
type SendMessageRequest struct {
	ChatID  string
	Message Message
}

// SendPhotoRequest is a picture to post.
type SendPhotoRequest struct {
	ChatID  string
	Caption string

	// URL is a photo the platform fetches itself. The Zalo Bot API accepts
	// only an http(s) reference: it has no upload endpoint, so a file on this
	// machine cannot become a photo.
	URL string
}

// Chat actions.
const (
	ActionTyping = "typing"
)

// Config configures the live client.
type Config struct {
	// Token authenticates every call. It is the bot token, "<bot id>:<secret>".
	Token string

	// BaseURL overrides the API origin. It exists for tests.
	BaseURL string

	// HTTPClient overrides the HTTP client.
	HTTPClient *http.Client

	// PollTimeout is how long getUpdates waits for a message.
	PollTimeout time.Duration

	// Log reports what the connection does.
	Log *slog.Logger
}

// HTTPClient is the live Zalo client.
//
// Inbound messages arrive by long polling: Zalo has no socket mode, and a
// webhook needs a public HTTPS endpoint, which a personal installation behind
// NAT does not have. Polling is what the platform documents for exactly this.
type HTTPClient struct {
	token       string
	baseURL     string
	http        *http.Client
	pollTimeout time.Duration
	log         *slog.Logger
}

// NewHTTPClient returns a live Zalo client.
func NewHTTPClient(config Config) (*HTTPClient, error) {
	if strings.TrimSpace(config.Token) == "" {
		return nil, errors.New("zalo: a bot token is required")
	}

	baseURL := config.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	pollTimeout := config.PollTimeout
	if pollTimeout <= 0 {
		pollTimeout = DefaultPollTimeout
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		// No client timeout: a long poll is meant to block, so the request's own
		// context bounds it rather than the client.
		httpClient = &http.Client{}
	}
	log := config.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &HTTPClient{
		token:       strings.TrimSpace(config.Token),
		baseURL:     strings.TrimRight(baseURL, "/"),
		http:        httpClient,
		pollTimeout: pollTimeout,
		log:         log,
	}, nil
}

// apiError is a refusal from the API.
type apiError struct {
	Code        int
	Description string
}

func (e *apiError) Error() string {
	if e.Description == "" {
		return fmt.Sprintf("zalo: api error %d", e.Code)
	}
	return fmt.Sprintf("zalo: %s (code %d)", e.Description, e.Code)
}

// errPollTimeout is getUpdates reporting that nothing arrived in time.
//
// It is not a failure: a long poll that returns empty is the normal way to learn
// there is nothing to do.
var errPollTimeout = errors.New("zalo: getUpdates timed out")

// response is the API's envelope.
type response struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
}

// call invokes one API method with a JSON body.
func (c *HTTPClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	body := []byte("{}")
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("zalo: encode %s: %w", method, err)
		}
		body = encoded
	}
	return c.post(ctx, method, "application/json", bytes.NewReader(body))
}

// post issues one API call and decodes the response envelope.
func (c *HTTPClient) post(ctx context.Context, method, contentType string, body io.Reader) (json.RawMessage, error) {
	url := fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("zalo: %s: %w", method, err)
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zalo: %s: %w", method, err)
	}
	defer resp.Body.Close()

	// The body is bounded: a transport must not read an unbounded amount of a
	// remote response into memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("zalo: %s: read response: %w", method, err)
	}

	var decoded response
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("zalo: %s: decode response: %w", method, err)
	}
	if !decoded.OK {
		apiErr := &apiError{Code: decoded.ErrorCode, Description: decoded.Description}
		if decoded.ErrorCode == http.StatusRequestTimeout {
			return nil, errPollTimeout
		}
		return nil, apiErr
	}
	return decoded.Result, nil
}

// Me returns the bot's own identity.
func (c *HTTPClient) Me(ctx context.Context) (Bot, error) {
	raw, err := c.call(ctx, "getMe", nil)
	if err != nil {
		return Bot{}, err
	}
	var bot Bot
	if err := json.Unmarshal(raw, &bot); err != nil {
		return Bot{}, fmt.Errorf("zalo: decode getMe: %w", err)
	}
	return bot, nil
}

// Events opens the long-poll loop and streams inbound deliveries.
//
// The loop is what makes a silent transport visible: it logs the connection once
// and reports every failure, so "nothing happens" is answerable from the log
// rather than a guess.
func (c *HTTPClient) Events(ctx context.Context) (<-chan Update, error) {
	out := make(chan Update, 64)
	go func() {
		defer close(out)
		c.poll(ctx, out)
	}()
	return out, nil
}

// poll reads updates until ctx is cancelled.
func (c *HTTPClient) poll(ctx context.Context, out chan<- Update) {
	c.connect(ctx)

	for {
		if ctx.Err() != nil {
			return
		}

		updates, err := c.getUpdates(ctx)
		switch {
		case errors.Is(err, errPollTimeout):
			// Nothing arrived in the poll window. Ask again.
			continue
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			c.log.Warn("zalo getUpdates failed; it is retried", "error", err)
			if !sleep(ctx, time.Second) {
				return
			}
			continue
		}

		for _, update := range updates {
			select {
			case out <- update:
			case <-ctx.Done():
				return
			}
		}
	}
}

// connect logs the bot the token belongs to.
//
// It is the line that says Zalo is reachable and the token is valid. Without it,
// a token that is wrong looks exactly like a bot nobody has messaged.
func (c *HTTPClient) connect(ctx context.Context) {
	bot, err := c.Me(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.log.Warn("zalo could not identify the bot; check ZALO_BOT_TOKEN", "error", err)
		}
		return
	}
	c.log.Info("zalo bot connected",
		"id", bot.ID,
		"name", bot.DisplayName,
		"account", bot.AccountName,
		"groups", bot.CanJoin,
	)
}

// getUpdates long-polls for inbound deliveries.
func (c *HTTPClient) getUpdates(ctx context.Context) ([]Update, error) {
	// The request outlives the poll window: the server answers at the deadline,
	// and a request that timed out first would turn every idle poll into an
	// error.
	ctx, cancel := context.WithTimeout(ctx, c.pollTimeout+10*time.Second)
	defer cancel()

	raw, err := c.call(ctx, "getUpdates", map[string]any{
		"timeout": strconv.Itoa(int(c.pollTimeout.Seconds())),
	})
	if err != nil {
		return nil, err
	}
	return decodeUpdates(raw), nil
}

// decodeUpdates reads whatever shape getUpdates returned.
//
// The platform documents a single event object, mirroring a webhook delivery,
// but a batch is the other shape a long-poll API uses. Reading both means a
// message is never dropped because the shape was not what was expected.
func decodeUpdates(raw json.RawMessage) []Update {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	var list []Update
	if err := json.Unmarshal(raw, &list); err == nil {
		return present(list)
	}

	var batch struct {
		Updates []Update `json:"updates"`
	}
	if err := json.Unmarshal(raw, &batch); err == nil && len(batch.Updates) > 0 {
		return present(batch.Updates)
	}

	var one Update
	if err := json.Unmarshal(raw, &one); err == nil && one.EventName != "" {
		return []Update{one}
	}
	return nil
}

// present drops entries that carry no event.
func present(updates []Update) []Update {
	out := make([]Update, 0, len(updates))
	for _, update := range updates {
		if update.EventName == "" && update.Message == nil {
			continue
		}
		out = append(out, update)
	}
	return out
}

// SendMessage posts a text message.
func (c *HTTPClient) SendMessage(ctx context.Context, req SendMessageRequest) (string, error) {
	params := map[string]any{
		"chat_id": req.ChatID,
		"text":    req.Message.Text,
	}
	if req.Message.ParseMode != "" {
		params["parse_mode"] = req.Message.ParseMode
	}

	raw, err := c.call(ctx, "sendMessage", params)
	if err != nil {
		return "", err
	}

	var result struct {
		MessageID string `json:"message_id"`
	}
	_ = json.Unmarshal(raw, &result)
	return result.MessageID, nil
}

// SendPhoto posts an image message.
//
// The photo is sent as a reference and the platform fetches the URL itself:
// the Bot API accepts no other form, so there is no upload path.
func (c *HTTPClient) SendPhoto(ctx context.Context, req SendPhotoRequest) (string, error) {
	params := map[string]any{
		"chat_id": req.ChatID,
		"photo":   req.URL,
	}
	if req.Caption != "" {
		params["caption"] = req.Caption
	}

	raw, err := c.call(ctx, "sendPhoto", params)
	if err != nil {
		return "", err
	}

	var result struct {
		MessageID string `json:"message_id"`
	}
	_ = json.Unmarshal(raw, &result)
	return result.MessageID, nil
}

// SendChatAction shows a transient status in a conversation.
func (c *HTTPClient) SendChatAction(ctx context.Context, chatID, action string) error {
	if chatID == "" {
		return nil
	}
	_, err := c.call(ctx, "sendChatAction", map[string]any{
		"chat_id": chatID,
		"action":  action,
	})
	return err
}

// DownloadFile fetches an attachment over plain HTTP.
//
// Zalo serves message media from a CDN URL, so no credential is needed. The size
// is bounded: a transport must not read an unbounded amount of a user's data
// into memory because they attached something large.
func (c *HTTPClient) DownloadFile(ctx context.Context, url string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxAttachmentBytes
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("zalo: download: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zalo: download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zalo: download: %s", resp.Status)
	}

	var buffer bytes.Buffer
	if _, err := io.Copy(&buffer, io.LimitReader(resp.Body, maxBytes+1)); err != nil {
		return nil, fmt.Errorf("zalo: download: %w", err)
	}
	if int64(buffer.Len()) > maxBytes {
		return nil, fmt.Errorf("zalo: file is larger than %d bytes", maxBytes)
	}
	return buffer.Bytes(), nil
}

// Close releases the connection.
//
// The poll loop's lifetime is the context Events was given, so there is nothing
// else to close here.
func (c *HTTPClient) Close() error { return nil }

// sleep waits for d or until ctx ends, reporting false when it ended.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
