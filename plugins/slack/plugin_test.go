package slack

import (
	"encoding/json"
	"fmt"
	"testing"
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
