package slack

import (
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
	if got := p.threadFor("run_1"); got != "1.0" {
		t.Fatalf("thread = %q, want 1.0", got)
	}
	// An unknown run is not threaded, and neither is an empty id.
	if got := p.threadFor("run_unknown"); got != "" {
		t.Fatalf("thread = %q, want empty", got)
	}
	if got := p.threadFor(""); got != "" {
		t.Fatalf("thread = %q, want empty", got)
	}

	// A run in a direct message is remembered as flat.
	p.rememberThread("run_2", "")
	if got := p.threadFor("run_2"); got != "" {
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
