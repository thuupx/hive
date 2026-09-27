package zalo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The API returns a single event, but reading a batch too means a message is
// never dropped because the shape was not what was expected.
func TestDecodeUpdatesShapes(t *testing.T) {
	single := json.RawMessage(`{"event_name":"message.text.received","message":{"text":"hi"}}`)
	if got := decodeUpdates(single); len(got) != 1 || got[0].Message.Text != "hi" {
		t.Fatalf("single = %+v", got)
	}

	list := json.RawMessage(`[{"event_name":"message.text.received","message":{"text":"a"}},{"event_name":"message.text.received","message":{"text":"b"}}]`)
	if got := decodeUpdates(list); len(got) != 2 {
		t.Fatalf("list = %+v", got)
	}

	batch := json.RawMessage(`{"updates":[{"event_name":"message.text.received","message":{"text":"c"}}]}`)
	if got := decodeUpdates(batch); len(got) != 1 || got[0].Message.Text != "c" {
		t.Fatalf("batch = %+v", got)
	}

	if got := decodeUpdates(nil); got != nil {
		t.Fatalf("empty = %+v, want none", got)
	}
}

// A token is required before any call is made.
func TestNewHTTPClientRequiresToken(t *testing.T) {
	if _, err := NewHTTPClient(Config{}); err == nil {
		t.Fatal("a client without a token should not be created")
	}
}

// A timeout from getUpdates is not an error: it is how an idle poll ends.
func TestGetUpdatesTimeoutIsNotAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": false, "description": "Request timeout", "error_code": 408})
	}))
	defer server.Close()

	client, err := NewHTTPClient(Config{Token: "t", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	if _, err := client.getUpdates(context.Background()); !errors.Is(err, errPollTimeout) {
		t.Fatalf("err = %v, want errPollTimeout", err)
	}
}

// SendMessage posts the text and returns the message id.
func TestSendMessage(t *testing.T) {
	var gotPath string
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, map[string]any{"ok": true, "result": map[string]any{"message_id": "m9"}})
	}))
	defer server.Close()

	client, err := NewHTTPClient(Config{Token: "tok", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	id, err := client.SendMessage(context.Background(), SendMessageRequest{
		ChatID:  "c1",
		Message: Message{Text: "hello", ParseMode: "markdown"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if id != "m9" {
		t.Fatalf("message id = %q", id)
	}
	if gotPath != "/bottok/sendMessage" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["chat_id"] != "c1" || gotBody["text"] != "hello" || gotBody["parse_mode"] != "markdown" {
		t.Fatalf("body = %+v", gotBody)
	}
}

// A refusal is reported with the platform's own words.
func TestAPIErrorIsReported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": false, "description": "Bad Request", "error_code": 400})
	}))
	defer server.Close()

	client, err := NewHTTPClient(Config{Token: "t", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	_, err = client.SendMessage(context.Background(), SendMessageRequest{ChatID: "c1", Message: Message{Text: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "Bad Request") {
		t.Fatalf("err = %v, want the platform's description", err)
	}
}

// Me reads the bot's identity.
func TestMe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "result": map[string]any{
			"id": "1", "account_name": "bot.x", "display_name": "Bot Hive Agent",
		}})
	}))
	defer server.Close()

	client, err := NewHTTPClient(Config{Token: "t", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	bot, err := client.Me(context.Background())
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if bot.DisplayName != "Bot Hive Agent" {
		t.Fatalf("bot = %+v", bot)
	}
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
