package zalo

import (
	"context"
	"errors"
	"fmt"
	"testing"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// fakeClient records what a transport asked the platform to do.
type fakeClient struct {
	sent     []SendMessageRequest
	actions  []string
	failSend bool
}

func (c *fakeClient) Events(context.Context) (<-chan Update, error) { return nil, nil }

func (c *fakeClient) SendMessage(_ context.Context, req SendMessageRequest) (string, error) {
	if c.failSend {
		return "", errors.New("no")
	}
	c.sent = append(c.sent, req)
	return "m1", nil
}

func (c *fakeClient) SendChatAction(_ context.Context, chatID, action string) error {
	c.actions = append(c.actions, chatID+":"+action)
	return nil
}

func (c *fakeClient) DownloadFile(context.Context, string, int64) ([]byte, error) {
	return []byte("data"), nil
}

func (c *fakeClient) Close() error { return nil }

// A turn shows the working signal while it runs, and stops refreshing when it
// ends.
func TestTypingShowsAndStops(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{TypingIndicator: true})

	p.startTyping(context.Background(), "c1")
	if len(client.actions) != 1 || client.actions[0] != "c1:typing" {
		t.Fatalf("actions = %v, want one typing action", client.actions)
	}
	if p.typing["c1"] == nil {
		t.Fatal("the conversation should be tracked as working")
	}

	p.advanceTyping(context.Background())
	if len(client.actions) != 2 {
		t.Fatalf("actions = %v, want the signal refreshed", client.actions)
	}

	p.stopTyping("c1")
	if p.typing["c1"] != nil {
		t.Fatal("the signal should be forgotten when the turn ends")
	}
}

// The signal is off when it is turned off.
func TestTypingCanBeTurnedOff(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{TypingIndicator: false})

	p.startTyping(context.Background(), "c1")
	if len(client.actions) != 0 {
		t.Fatalf("actions = %v, want none", client.actions)
	}
}

// A permission request is remembered so a reply in words can answer it.
func TestPermissionIsRememberedAndCleared(t *testing.T) {
	p := New(nil, nil, Options{})

	p.rememberPending("c1", permissionRequest{AgentRequestID: "req1"})
	if got := p.pendingFor("c1"); got == nil || got.AgentRequestID != "req1" {
		t.Fatalf("pending = %+v, want the request", got)
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
		p.rememberPending(fmt.Sprintf("c%d", i), permissionRequest{AgentRequestID: "req"})
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

	if p.alreadyHandled("event:c1:m1") {
		t.Fatal("the first delivery is not a repeat")
	}
	if !p.alreadyHandled("event:c1:m1") {
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

// A long answer is posted as more than one message, because Zalo refuses a text
// over its limit.
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
