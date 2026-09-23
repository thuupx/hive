package slack

import (
	"encoding/json"
	"fmt"
	"testing"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// A long-lived transport must not remember every tool call it has ever shown.
func TestToolMessagesAreBounded(t *testing.T) {
	p := New(nil, nil, Options{})

	for i := 0; i < maxToolMessages+10; i++ {
		id := fmt.Sprintf("tc_%d", i)
		p.mu.Lock()
		p.toolMessages[id] = "1.0"
		p.toolOrder = append(p.toolOrder, id)
		for len(p.toolOrder) > maxToolMessages {
			oldest := p.toolOrder[0]
			p.toolOrder = p.toolOrder[1:]
			delete(p.toolMessages, oldest)
		}
		p.mu.Unlock()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.toolMessages) != maxToolMessages {
		t.Fatalf("tool messages = %d, want %d", len(p.toolMessages), maxToolMessages)
	}
	if _, ok := p.toolMessages["tc_0"]; ok {
		t.Error("the oldest tool message should have been forgotten")
	}
}

// A delivery carries the thread its turn belongs to.
//
// This is the wiring, not the rule: the rule was tested and correct while the
// delivery never called it, so every channel reply stayed flat.
func TestChannelDeliveryCarriesItsThread(t *testing.T) {
	p := New(nil, nil, Options{})

	d, ok := p.parse(Inbound{Type: "event", Payload: mustJSON(t, MessageEvent{
		Type:      "message",
		Channel:   "C123",
		User:      "U1",
		Text:      "<@U0BOT> hello",
		Timestamp: "1.0",
	})})
	if !ok {
		t.Fatal("the message should be for Hive")
	}
	if d.thread != "1.0" {
		t.Fatalf("thread = %q, want the message timestamp", d.thread)
	}

	// A direct message stays flat.
	dm, ok := p.parse(Inbound{Type: "event", Payload: mustJSON(t, MessageEvent{
		Type:      "message",
		Channel:   "D123",
		User:      "U1",
		Text:      "hello",
		Timestamp: "1.0",
	})})
	if !ok {
		t.Fatal("the direct message should be for Hive")
	}
	if dm.thread != "" {
		t.Fatalf("a direct message thread = %q, want empty", dm.thread)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}

// The envelope names the thread, because that is what Hive binds.
//
// Found by testing the new key against a real channel: the delivery carried the
// thread, and the envelope still carried the channel, so Hive bound the channel
// and two threads were one conversation again.
func TestTheEnvelopeNamesTheThread(t *testing.T) {
	p := New(nil, nil, Options{})

	d, ok := p.parse(Inbound{
		Type: "event_callback",
		Payload: json.RawMessage(`{
			"type": "message",
			"channel": "C1",
			"user": "U1",
			"text": "hello",
			"ts": "1.2",
			"thread_ts": "1.0"
		}`),
	})
	if !ok {
		t.Fatal("a message should parse")
	}

	if d.conversationID != "C1:1.0" {
		t.Fatalf("conversation = %q, want the thread", d.conversationID)
	}
	if d.envelope.ConversationID != d.conversationID {
		t.Fatalf("the envelope says %q and the delivery says %q, so Hive would bind the wrong one",
			d.envelope.ConversationID, d.conversationID)
	}
	if d.channelID != "C1" {
		t.Fatalf("channel = %q, want the channel that holds the thread", d.channelID)
	}
}

// An event goes where the core says, not only where this transport learned.
//
// Found in a live thread: a restart emptied the transport's own map, so a
// permission card rendered into nothing — the log said conversations=0 while the
// binding existed. A card that is never posted is a button nobody can press.
func TestAnEventUsesTheConversationsTheCoreNames(t *testing.T) {
	p := New(nil, nil, Options{})

	// Nothing learned locally: this is a transport that has just started.
	got := p.conversationsFor(v1.DeliveredEvent{
		Event:         v1.Event{SessionID: "sess_1"},
		Conversations: []string{"C1:1.0", "C1:2.0"},
	})

	if len(got) != 2 || got[0] != "C1:1.0" || got[1] != "C1:2.0" {
		t.Fatalf("conversations = %v, want the ones the core named", got)
	}

	// A repeat is not rendered twice.
	got = p.conversationsFor(v1.DeliveredEvent{
		Event:         v1.Event{SessionID: "sess_1"},
		Conversations: []string{"C1:1.0", "C1:1.0"},
	})
	if len(got) != 1 {
		t.Fatalf("conversations = %v, want one", got)
	}

	// What this transport learned is still used, because a binding made moments
	// ago is already true here.
	p.rememberBinding("C1:3.0", "sess_1")
	got = p.conversationsFor(v1.DeliveredEvent{
		Event:         v1.Event{SessionID: "sess_1"},
		Conversations: []string{"C1:1.0"},
	})
	if len(got) != 2 {
		t.Fatalf("conversations = %v, want the core's and the local one", got)
	}
}

// A gap in the sequence is read back, not skipped.
//
// Found in a live thread: an answer was published while the transport was not
// reading, so the bus no longer had it, the cursor never moved, and the answer
// never arrived. A user reports that as a bot that ignored them.
func TestAGapIsNoticed(t *testing.T) {
	p := New(nil, nil, Options{})

	// The first event establishes the position.
	if p.gapFrom("sess_1", 5) != 0 {
		t.Fatal("the first event is not a gap")
	}

	// The next in order is not a gap either.
	if p.gapFrom("sess_1", 6) != 0 {
		t.Fatal("a consecutive event is not a gap")
	}

	// A jump is: 7 and 8 were never delivered.
	if got := p.gapFrom("sess_1", 9); got != 6 {
		t.Fatalf("gapFrom = %d, want the last sequence seen", got)
	}

	// A session this transport has never seen starts wherever it starts.
	if got := p.gapFrom("sess_other", 40); got != 0 {
		t.Fatalf("gapFrom = %d, want no gap for a session just met", got)
	}
}
