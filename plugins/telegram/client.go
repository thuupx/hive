package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the Telegram Bot API origin.
const DefaultBaseURL = "https://api.telegram.org"

// DefaultPollTimeout is how long a getUpdates call waits for a message.
//
// Long polling is what makes the transport need no public endpoint, which is
// the property that makes it workable on a laptop behind NAT.
const DefaultPollTimeout = 30 * time.Second

// Client is the Telegram Bot API surface the transport needs.
//
// Keeping it an interface is what makes the transport testable: parsing,
// routing, and rendering are exercised against a fake, and only the live
// connection needs a real token.
type Client interface {
	// Events streams inbound deliveries until ctx is cancelled.
	Events(ctx context.Context) (<-chan Update, error)

	// Me returns the bot's own identity.
	Me(ctx context.Context) (Bot, error)

	// SendMessage posts a text message and returns its id.
	SendMessage(ctx context.Context, req SendMessageRequest) (int64, error)

	// SendPhoto posts an image message and returns its id.
	SendPhoto(ctx context.Context, req SendPhotoRequest) (int64, error)

	// SendChatAction shows a transient status in a conversation, such as
	// "typing". It is best-effort: a failure must not fail the turn.
	SendChatAction(ctx context.Context, chatID string, action string) error

	// AnswerCallbackQuery acknowledges a button press, ending the spinner.
	AnswerCallbackQuery(ctx context.Context, id, text string) error

	// EditMessageReplyMarkup replaces a message's buttons, normally to take
	// them away once the question they asked is answered.
	EditMessageReplyMarkup(ctx context.Context, chatID string, messageID int64, keyboard [][]InlineButton) error

	// SetMessageReaction marks a user's message with an emoji.
	SetMessageReaction(ctx context.Context, chatID string, messageID int64, emoji string) error

	// DownloadFile fetches an attachment by its file id, bounded in size.
	DownloadFile(ctx context.Context, fileID string, maxBytes int64) ([]byte, string, error)

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
	ChatID   string
	Caption  string
	ThreadID int64

	// URL is a photo the platform fetches itself.
	URL string

	// Path is a local file to upload. Telegram accepts a multipart upload,
	// which Zalo's API does not, so a file on this machine can become a photo.
	Path string
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

// HTTPClient is the live Telegram client.
//
// Inbound messages arrive by long polling: a webhook needs a public HTTPS
// endpoint, which a personal installation behind NAT does not have. Polling is
// what the platform documents for exactly this.
type HTTPClient struct {
	token       string
	baseURL     string
	http        *http.Client
	pollTimeout time.Duration
	log         *slog.Logger
}

// NewHTTPClient returns a live Telegram client.
func NewHTTPClient(config Config) (*HTTPClient, error) {
	if strings.TrimSpace(config.Token) == "" {
		return nil, errors.New("telegram: a bot token is required")
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
		// No client timeout: a long poll is meant to block, so the request's
		// own context bounds it rather than the client.
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
		return fmt.Sprintf("telegram: api error %d", e.Code)
	}
	return fmt.Sprintf("telegram: %s (code %d)", e.Description, e.Code)
}

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
			return nil, fmt.Errorf("telegram: encode %s: %w", method, err)
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
		return nil, fmt.Errorf("telegram: %s: %w", method, err)
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram: %s: %w", method, err)
	}
	defer resp.Body.Close()

	// The body is bounded: a transport must not read an unbounded amount of a
	// remote response into memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("telegram: %s: read response: %w", method, err)
	}

	var decoded response
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("telegram: %s: decode response: %w", method, err)
	}
	if !decoded.OK {
		return nil, &apiError{Code: decoded.ErrorCode, Description: decoded.Description}
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
		return Bot{}, fmt.Errorf("telegram: decode getMe: %w", err)
	}
	return bot, nil
}

// Events opens the long-poll loop and streams inbound deliveries.
//
// The loop is what makes a silent transport visible: it logs the connection
// once and reports every failure, so "nothing happens" is answerable from the
// log rather than a guess.
func (c *HTTPClient) Events(ctx context.Context) (<-chan Update, error) {
	out := make(chan Update, 64)
	go func() {
		defer close(out)
		c.poll(ctx, out)
	}()
	return out, nil
}

// poll reads updates until ctx is cancelled.
//
// The offset is the acknowledgement: Telegram redelivers any update at or
// after it, so advancing past an update_id is what stops a repeat. The cursor
// lives in memory; a restart replays at most the unconfirmed tail, and the
// transport's dedupe window absorbs it.
func (c *HTTPClient) poll(ctx context.Context, out chan<- Update) {
	c.connect(ctx)

	var offset int64
	for {
		if ctx.Err() != nil {
			return
		}

		updates, err := c.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.Warn("telegram getUpdates failed; it is retried", "error", err)
			if !sleep(ctx, time.Second) {
				return
			}
			continue
		}

		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
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
// It is the line that says Telegram is reachable and the token is valid.
// Without it, a token that is wrong looks exactly like a bot nobody has
// messaged.
func (c *HTTPClient) connect(ctx context.Context) {
	bot, err := c.Me(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.log.Warn("telegram could not identify the bot; check TELEGRAM_BOT_TOKEN", "error", err)
		}
		return
	}
	c.log.Info("telegram bot connected",
		"id", bot.ID,
		"name", bot.FirstName,
		"username", bot.Username,
		"groups", bot.CanJoin,
	)
}

// getUpdates long-polls for inbound deliveries.
func (c *HTTPClient) getUpdates(ctx context.Context, offset int64) ([]Update, error) {
	// The request outlives the poll window: the server answers at the deadline,
	// and a request that timed out first would turn every idle poll into an
	// error.
	ctx, cancel := context.WithTimeout(ctx, c.pollTimeout+10*time.Second)
	defer cancel()

	params := map[string]any{
		"timeout":         int(c.pollTimeout.Seconds()),
		"allowed_updates": []string{"message", "callback_query"},
	}
	if offset > 0 {
		params["offset"] = offset
	}

	raw, err := c.call(ctx, "getUpdates", params)
	if err != nil {
		return nil, err
	}

	var updates []Update
	if err := json.Unmarshal(raw, &updates); err != nil {
		return nil, fmt.Errorf("telegram: decode getUpdates: %w", err)
	}
	return updates, nil
}

// SendMessage posts a text message.
//
// The message is sent as HTML when it carries markup; Telegram's reply is the
// message id, which a permission answer later needs to take the buttons away.
func (c *HTTPClient) SendMessage(ctx context.Context, req SendMessageRequest) (int64, error) {
	params := map[string]any{
		"chat_id": req.ChatID,
		"text":    req.Message.Text,
	}
	if req.Message.ParseMode != "" {
		params["parse_mode"] = req.Message.ParseMode
	}
	if req.Message.ThreadID > 0 {
		params["message_thread_id"] = req.Message.ThreadID
	}
	if len(req.Message.Keyboard) > 0 {
		params["reply_markup"] = map[string]any{"inline_keyboard": req.Message.Keyboard}
	}

	raw, err := c.call(ctx, "sendMessage", params)
	if err != nil {
		// Markup that does not parse must not lose the message: the text is
		// the same, only the presentation degrades.
		var apiErr *apiError
		if req.Message.ParseMode != "" && errors.As(err, &apiErr) {
			c.log.Debug("markup was refused; sending the text plain", "error", err)
			params["parse_mode"] = ""
			delete(params, "parse_mode")
			raw, err = c.call(ctx, "sendMessage", params)
		}
		if err != nil {
			return 0, err
		}
	}

	var result struct {
		MessageID int64 `json:"message_id"`
	}
	_ = json.Unmarshal(raw, &result)
	return result.MessageID, nil
}

// SendPhoto posts an image message.
//
// An http(s) target is sent as a reference the platform fetches. A local path
// is uploaded: Telegram accepts a multipart body, so a file on this machine
// becomes a photo rather than a text reference.
func (c *HTTPClient) SendPhoto(ctx context.Context, req SendPhotoRequest) (int64, error) {
	if req.Path != "" {
		return c.sendPhotoUpload(ctx, req)
	}

	params := map[string]any{
		"chat_id": req.ChatID,
		"photo":   req.URL,
	}
	if req.Caption != "" {
		params["caption"] = req.Caption
	}
	if req.ThreadID > 0 {
		params["message_thread_id"] = req.ThreadID
	}

	raw, err := c.call(ctx, "sendPhoto", params)
	if err != nil {
		return 0, err
	}

	var result struct {
		MessageID int64 `json:"message_id"`
	}
	_ = json.Unmarshal(raw, &result)
	return result.MessageID, nil
}

// sendPhotoUpload posts a picture from a file on this machine.
func (c *HTTPClient) sendPhotoUpload(ctx context.Context, req SendPhotoRequest) (int64, error) {
	file, err := os.Open(req.Path)
	if err != nil {
		return 0, fmt.Errorf("telegram: open photo: %w", err)
	}
	defer file.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chat_id", req.ChatID); err != nil {
		return 0, err
	}
	if req.Caption != "" {
		if err := writer.WriteField("caption", req.Caption); err != nil {
			return 0, err
		}
	}
	if req.ThreadID > 0 {
		if err := writer.WriteField("message_thread_id", strconv.FormatInt(req.ThreadID, 10)); err != nil {
			return 0, err
		}
	}
	part, err := writer.CreateFormFile("photo", filepath.Base(req.Path))
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(part, file); err != nil {
		return 0, fmt.Errorf("telegram: read photo: %w", err)
	}
	if err := writer.Close(); err != nil {
		return 0, err
	}

	raw, err := c.post(ctx, "sendPhoto", writer.FormDataContentType(), &body)
	if err != nil {
		return 0, err
	}

	var result struct {
		MessageID int64 `json:"message_id"`
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

// AnswerCallbackQuery acknowledges a button press. Without it the button's
// spinner turns until Telegram gives up.
func (c *HTTPClient) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	params := map[string]any{"callback_query_id": id}
	if text != "" {
		params["text"] = text
	}
	_, err := c.call(ctx, "answerCallbackQuery", params)
	return err
}

// EditMessageReplyMarkup replaces a message's buttons.
func (c *HTTPClient) EditMessageReplyMarkup(ctx context.Context, chatID string, messageID int64, keyboard [][]InlineButton) error {
	_, err := c.call(ctx, "editMessageReplyMarkup", map[string]any{
		"chat_id":      chatID,
		"message_id":   messageID,
		"reply_markup": map[string]any{"inline_keyboard": keyboard},
	})
	return err
}

// SetMessageReaction marks a user's message with an emoji.
func (c *HTTPClient) SetMessageReaction(ctx context.Context, chatID string, messageID int64, emoji string) error {
	_, err := c.call(ctx, "setMessageReaction", map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"reaction":   []map[string]any{{"type": "emoji", "emoji": emoji}},
	})
	return err
}

// DownloadFile fetches an attachment by file id.
//
// Telegram answers getFile with a path the file endpoint then serves, so a
// download is two calls. The size is bounded: a transport must not read an
// unbounded amount of a user's data into memory because they attached
// something large. It returns the bytes and the file's own name when known.
func (c *HTTPClient) DownloadFile(ctx context.Context, fileID string, maxBytes int64) ([]byte, string, error) {
	raw, err := c.call(ctx, "getFile", map[string]any{"file_id": fileID})
	if err != nil {
		return nil, "", err
	}
	var file File
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, "", fmt.Errorf("telegram: decode getFile: %w", err)
	}
	if file.FilePath == "" {
		return nil, "", errors.New("telegram: getFile returned no path")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxAttachmentBytes
	}
	if file.FileSize > maxBytes {
		return nil, "", fmt.Errorf("telegram: file is larger than %d bytes", maxBytes)
	}

	url := fmt.Sprintf("%s/file/bot%s/%s", c.baseURL, c.token, file.FilePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("telegram: download: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("telegram: download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("telegram: download: %s", resp.Status)
	}

	var buffer bytes.Buffer
	if _, err := io.Copy(&buffer, io.LimitReader(resp.Body, maxBytes+1)); err != nil {
		return nil, "", fmt.Errorf("telegram: download: %w", err)
	}
	if int64(buffer.Len()) > maxBytes {
		return nil, "", fmt.Errorf("telegram: file is larger than %d bytes", maxBytes)
	}
	return buffer.Bytes(), filepath.Base(file.FilePath), nil
}

// Close releases the connection.
//
// The poll loop's lifetime is the context Events was given, so there is
// nothing else to close here.
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
