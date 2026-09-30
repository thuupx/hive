package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// fakeClient records what a transport asked the platform to do.
type fakeClient struct {
	sent      []SendMessageRequest
	photos    []SendPhotoRequest
	actions   []string
	answered  []string
	markups   []int64
	reactions []string
	failSend  bool
	failPhoto bool
}

func (c *fakeClient) Events(context.Context) (<-chan Update, error) { return nil, nil }

func (c *fakeClient) Me(context.Context) (Bot, error) {
	return Bot{ID: 99, Username: "hivebot", IsBot: true}, nil
}

func (c *fakeClient) SendMessage(_ context.Context, req SendMessageRequest) (int64, error) {
	if c.failSend {
		return 0, errors.New("no")
	}
	c.sent = append(c.sent, req)
	return int64(len(c.sent)), nil
}

func (c *fakeClient) SendPhoto(_ context.Context, req SendPhotoRequest) (int64, error) {
	if c.failSend || c.failPhoto {
		return 0, errors.New("no")
	}
	c.photos = append(c.photos, req)
	return 100 + int64(len(c.photos)), nil
}

func (c *fakeClient) SendChatAction(_ context.Context, chatID string, threadID int64, action string) error {
	c.actions = append(c.actions, fmt.Sprintf("%s:%d:%s", chatID, threadID, action))
	return nil
}

func (c *fakeClient) AnswerCallbackQuery(_ context.Context, id, _ string) error {
	c.answered = append(c.answered, id)
	return nil
}

func (c *fakeClient) EditMessageReplyMarkup(_ context.Context, _ string, messageID int64, _ [][]InlineButton) error {
	c.markups = append(c.markups, messageID)
	return nil
}

func (c *fakeClient) SetMessageReaction(_ context.Context, chatID string, messageID int64, emoji string) error {
	c.reactions = append(c.reactions, fmt.Sprintf("%s:%d:%s", chatID, messageID, emoji))
	return nil
}

func (c *fakeClient) DownloadFile(context.Context, string, int64) ([]byte, string, error) {
	return []byte("data"), "file", nil
}

func (c *fakeClient) Close() error { return nil }

// A turn shows the working signal while it runs — the chat action in the
// header and a reaction on the user's message — and takes both down when it
// ends.
func TestTypingShowsAndStops(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{TypingIndicator: true})

	p.startTyping(context.Background(), "c1", 42)
	if len(client.actions) != 1 || client.actions[0] != "c1:0:typing" {
		t.Fatalf("actions = %v, want one typing action", client.actions)
	}
	if len(client.reactions) != 1 || client.reactions[0] != "c1:42:✍️" {
		t.Fatalf("reactions = %v, want the message marked as working", client.reactions)
	}
	if p.typing["c1"] == nil {
		t.Fatal("the conversation should be tracked as working")
	}

	p.advanceTyping(context.Background())
	if len(client.actions) != 2 {
		t.Fatalf("actions = %v, want the signal refreshed", client.actions)
	}

	p.stopTyping(context.Background(), "c1")
	if p.typing["c1"] != nil {
		t.Fatal("the signal should be forgotten when the turn ends")
	}
	if len(client.reactions) != 2 || client.reactions[1] != "c1:42:" {
		t.Fatalf("reactions = %v, want the reaction taken off", client.reactions)
	}
}

// A turn that runs long enough to time out loses its reaction rather than
// claiming work that is not happening.
func TestATimedOutTypingReactionIsRemoved(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{TypingIndicator: true})

	p.startTyping(context.Background(), "c1", 42)
	p.typing["c1"].started = time.Now().Add(-typingTimeout - time.Minute)

	p.advanceTyping(context.Background())
	if p.typing["c1"] != nil {
		t.Fatal("an expired signal should be forgotten")
	}
	if len(client.reactions) != 2 || client.reactions[1] != "c1:42:" {
		t.Fatalf("reactions = %v, want the stale reaction removed", client.reactions)
	}
}

// The signal is off when it is turned off.
func TestTypingCanBeTurnedOff(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{TypingIndicator: false})

	p.startTyping(context.Background(), "c1", 42)
	if len(client.actions) != 0 || len(client.reactions) != 0 {
		t.Fatalf("actions = %v reactions = %v, want none", client.actions, client.reactions)
	}
}

// A permission request is remembered so a button or a worded reply can answer
// it.
func TestPermissionIsRememberedAndCleared(t *testing.T) {
	p := New(nil, nil, Options{})

	p.rememberPending("c1", permissionRequest{AgentRequestID: "req1"}, 55)
	if got := p.pendingFor("c1"); got == nil || got.request.AgentRequestID != "req1" || got.messageID != 55 {
		t.Fatalf("pending = %+v, want the request and its message", got)
	}

	p.clearPending("c1")
	if p.pendingFor("c1") != nil {
		t.Fatal("the request should be forgotten once answered")
	}
}

// A long-lived transport must not remember every permission it has seen.
func TestPendingIsBounded(t *testing.T) {
	p := New(nil, nil, Options{})

	for i := 0; i < maxPending+10; i++ {
		p.rememberPending(fmt.Sprintf("c%d", i), permissionRequest{AgentRequestID: "req"}, 0)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pending) != maxPending {
		t.Fatalf("pending = %d, want %d", len(p.pending), maxPending)
	}
}

// A gap in the sequence is read back, not skipped.
func TestAGapIsNoticed(t *testing.T) {
	p := New(nil, nil, Options{})

	if p.gapFrom("s1", 5) != 0 {
		t.Fatal("the first event is not a gap")
	}
	if p.gapFrom("s1", 6) != 0 {
		t.Fatal("a consecutive event is not a gap")
	}
	if got := p.gapFrom("s1", 9); got != 6 {
		t.Fatalf("gapFrom = %d, want the last sequence seen", got)
	}
}

// A delivery seen twice is handled once.
func TestARepeatDeliveryIsIgnored(t *testing.T) {
	p := New(nil, nil, Options{})

	if p.alreadyHandled("event:1:1") {
		t.Fatal("the first delivery is not a repeat")
	}
	if !p.alreadyHandled("event:1:1") {
		t.Fatal("the second delivery is a repeat")
	}
}

// An event goes where the core says, not only where this transport learned.
func TestAnEventUsesTheConversationsTheCoreNames(t *testing.T) {
	p := New(nil, nil, Options{})

	got := p.conversationsFor(v1.DeliveredEvent{
		Event:         v1.Event{SessionID: "s1"},
		Conversations: []string{"c1", "c2"},
	})
	if len(got) != 2 {
		t.Fatalf("conversations = %v, want the ones the core named", got)
	}

	p.rememberBinding("c3", "s1")
	got = p.conversationsFor(v1.DeliveredEvent{
		Event:         v1.Event{SessionID: "s1"},
		Conversations: []string{"c1"},
	})
	if len(got) != 2 {
		t.Fatalf("conversations = %v, want the core's and the local one", got)
	}
}

// A long answer is posted as more than one message, because Telegram refuses
// a text over its limit.
func TestPostSplitsLongMessages(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{})

	long := make([]byte, ChunkChars+200)
	for i := range long {
		long[i] = 'x'
	}
	p.post(context.Background(), "c1", plainMessage(string(long)))

	if len(client.sent) < 2 {
		t.Fatalf("sent %d message(s), want the answer split", len(client.sent))
	}
	for _, req := range client.sent {
		if len(req.Message.Text) > ChunkChars {
			t.Errorf("a chunk is %d chars, over the limit", len(req.Message.Text))
		}
	}
}

// A message's pictures are posted as photos after its text.
func TestPostSendsAnImageByURL(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{})

	p.post(context.Background(), "c1", Message{
		Text:   "here it is",
		Images: []OutboundImage{{Alt: "shot", Target: "https://cdn.example/a.png"}},
	})

	if len(client.sent) != 1 || client.sent[0].Message.Text != "here it is" {
		t.Fatalf("sent = %+v, want the text first", client.sent)
	}
	if len(client.photos) != 1 {
		t.Fatalf("photos = %+v, want the picture", client.photos)
	}
	photo := client.photos[0]
	if photo.URL != "https://cdn.example/a.png" || photo.Caption != "shot" || photo.Path != "" {
		t.Fatalf("photo = %+v", photo)
	}
}

// Telegram accepts an upload, so a local path becomes a photo rather than a
// text reference.
func TestALocalImageIsUploaded(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{})
	p.post(context.Background(), "c1", Message{
		Images: []OutboundImage{{Alt: "shot", Target: "/tmp/shot.png"}},
	})

	if len(client.photos) != 1 || client.photos[0].Path != "/tmp/shot.png" {
		t.Fatalf("photos = %+v, want the file uploaded", client.photos)
	}
}

// A picture that cannot be sent is not silently lost: the reference is posted
// as text instead.
func TestAFailedImagePostsItsReference(t *testing.T) {
	client := &fakeClient{failPhoto: true}
	p := New(nil, client, Options{})

	p.post(context.Background(), "c1", Message{
		Images: []OutboundImage{{Alt: "shot", Target: "https://cdn.example/a.png"}},
	})

	found := false
	for _, req := range client.sent {
		if strings.Contains(req.Message.Text, "https://cdn.example/a.png") {
			found = true
		}
	}
	if !found {
		t.Fatalf("sent = %+v, want the reference kept", client.sent)
	}
}

// A permission post remembers the request and the message its buttons are on.
func TestAPermissionPostRemembersItsMessage(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{})

	rendered := Rendered{
		Message:    permissionMessage(permissionRequest{AgentRequestID: "req1"}),
		Permission: &permissionRequest{AgentRequestID: "req1"},
	}
	p.postPermission(context.Background(), "c1", rendered)

	if len(client.sent) != 1 {
		t.Fatalf("sent = %+v", client.sent)
	}
	got := p.pendingFor("c1")
	if got == nil || got.request.AgentRequestID != "req1" || got.messageID != 1 {
		t.Fatalf("pending = %+v, want the request and its message id", got)
	}
}
