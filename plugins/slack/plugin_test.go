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

// A channel is shared, so a turn's output belongs in a thread under the message
// that asked for it. A direct message has nobody to spare the noise from.
func TestReplyThread(t *testing.T) {
	cases := map[string]struct {
		conversation string
		messageTS    string
		existing     string
		want         string
	}{
		"a channel message starts a thread":           {"C1", "1.0", "", "1.0"},
		"a group message starts a thread":             {"G1", "1.0", "", "1.0"},
		"a direct message stays flat":                 {"D1", "1.0", "", ""},
		"a message already in a thread replies in it": {"C1", "1.0", "0.9", "0.9"},
		"a threaded direct message replies in it":     {"D1", "1.0", "0.9", "0.9"},
		"a message with no timestamp stays flat":      {"C1", "", "", ""},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := replyThread(tc.conversation, tc.messageTS, tc.existing, false)
			if got != tc.want {
				t.Fatalf("thread = %q, want %q", got, tc.want)
			}
		})
	}
}

// A run's thread is remembered so its output lands under the right message.
func TestRememberThread(t *testing.T) {
	p := New(nil, nil, Options{})

	p.rememberThread("run_1", "1.0")
	if got := p.threadFor("run_1", "C1"); got != "1.0" {
		t.Fatalf("thread = %q, want 1.0", got)
	}
	// An unknown run is not threaded, and neither is an empty id.
	if got := p.threadFor("run_unknown", "C1"); got != "" {
		t.Fatalf("thread = %q, want empty", got)
	}
	if got := p.threadFor("", "C1"); got != "" {
		t.Fatalf("thread = %q, want empty", got)
	}

	// A run in a direct message is remembered as flat.
	p.rememberThread("run_2", "")
	if got := p.threadFor("run_2", "C1"); got != "" {
		t.Fatalf("thread = %q, want empty", got)
	}
}

// The map of run threads is bounded, like every other per-activity map.
func TestRunThreadsAreBounded(t *testing.T) {
	p := New(nil, nil, Options{})

	for i := 0; i < maxRunThreads+10; i++ {
		p.rememberThread(fmt.Sprintf("run_%d", i), "1.0")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.runThreads) != maxRunThreads {
		t.Fatalf("run threads = %d, want %d", len(p.runThreads), maxRunThreads)
	}
	if _, ok := p.runThreads["run_0"]; ok {
		t.Error("the oldest run thread should have been forgotten")
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

// A tool call that arrives before the delivery returns still belongs to the turn.
//
// Found in a live conversation: one tool card landed in the channel while the
// rest of the turn landed in the thread, because the agent emitted its first call
// before the delivery that caused it had returned and the run was not known yet.
func TestAnEventBeforeTheRunIsKnownStillThreads(t *testing.T) {
	p := New(nil, nil, Options{})

	// The turn is under way: the conversation's thread is known, the run is not.
	p.beginTurn("C1", "1.0")

	if got := p.threadFor("", "C1"); got != "1.0" {
		t.Fatalf("an event with no run yet threaded to %q, want the turn thread", got)
	}

	// Once the run is known it is the precise answer, even if it differs.
	p.rememberThread("run_1", "2.0")
	if got := p.threadFor("run_1", "C1"); got != "2.0" {
		t.Fatalf("a known run threaded to %q, want its own thread", got)
	}

	// The turn ends, and the conversation is no longer mid-turn.
	p.endTurn("C1")
	if got := p.threadFor("", "C1"); got != "" {
		t.Fatalf("after the turn, an unknown run threaded to %q, want empty", got)
	}
}

// A conversation with no turn under way does not thread.
func TestNoTurnMeansNoThread(t *testing.T) {
	p := New(nil, nil, Options{})

	if got := p.threadFor("run_unknown", "C1"); got != "" {
		t.Fatalf("thread = %q, want empty", got)
	}
}
