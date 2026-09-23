package slack

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// fakeClient records what a transport asked the platform to do.
type fakeClient struct {
	posted   []PostMessageRequest
	updated  []UpdateMessageRequest
	reaction []ReactionRequest

	failPost bool
}

func (c *fakeClient) PostMessage(_ context.Context, req PostMessageRequest) (string, error) {
	if c.failPost {
		return "", errors.New("no")
	}
	c.posted = append(c.posted, req)
	return "1.0", nil
}

func (c *fakeClient) UpdateMessage(_ context.Context, req UpdateMessageRequest) error {
	c.updated = append(c.updated, req)
	return nil
}

func (c *fakeClient) AddReaction(_ context.Context, req ReactionRequest) error {
	c.reaction = append(c.reaction, req)
	return nil
}

func (c *fakeClient) ChannelHistory(context.Context, string, int) ([]HistoryMessage, error) {
	return nil, nil
}

func (c *fakeClient) DownloadFile(context.Context, string, int64) ([]byte, error) {
	return nil, nil
}

func (c *fakeClient) Events(ctx context.Context) (<-chan Inbound, error) { return nil, nil }

func (c *fakeClient) Close() error { return nil }

// A turn shows that it is working with one message, and the answer takes its
// place rather than arriving as a second message.
func TestTheAnswerReplacesTheTypingIndicator(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{TypingIndicator: true})

	p.startTyping(context.Background(), "C1", "0.9")

	if len(client.posted) != 1 {
		t.Fatalf("posted %d message(s), want one indicator", len(client.posted))
	}
	if client.posted[0].ThreadTS != "0.9" {
		t.Errorf("the indicator went to thread %q, want the turn's", client.posted[0].ThreadTS)
	}

	replaced := p.replaceTyping(context.Background(), "C1", textMessage("the answer"))

	if !replaced {
		t.Fatal("the answer should have replaced the indicator")
	}
	if len(client.updated) != 1 {
		t.Fatalf("updated %d message(s), want one", len(client.updated))
	}
	if client.updated[0].Timestamp != "1.0" {
		t.Errorf("updated %q, want the indicator's message", client.updated[0].Timestamp)
	}

	// A second answer has nothing left to replace, so it is posted as its own
	// message rather than vanishing.
	if p.replaceTyping(context.Background(), "C1", textMessage("again")) {
		t.Fatal("there is no indicator to replace a second time")
	}
}

// The indicator is off when it is turned off.
func TestTypingIndicatorCanBeTurnedOff(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{TypingIndicator: false})

	p.startTyping(context.Background(), "C1", "")

	if len(client.posted) != 0 {
		t.Fatalf("posted %d message(s), want none", len(client.posted))
	}
	if p.replaceTyping(context.Background(), "C1", textMessage("x")) {
		t.Fatal("there is no indicator to replace")
	}
}

// An indicator that cannot be shown does not fail the turn.
func TestAnIndicatorThatCannotBeShownIsNotFatal(t *testing.T) {
	client := &fakeClient{failPost: true}
	p := New(nil, client, Options{TypingIndicator: true})

	p.startTyping(context.Background(), "C1", "")

	if p.replaceTyping(context.Background(), "C1", textMessage("x")) {
		t.Fatal("a failed indicator leaves nothing to replace")
	}
}

// Only an answer replaces the indicator. A tool card is a step, and it belongs
// after the indicator rather than in place of it.
func TestOnlyAnAnswerReplacesTheIndicator(t *testing.T) {
	cases := map[string]struct {
		event v1.Event
		want  bool
	}{
		"an answer": {v1.Event{
			Type:    v1.EventMessage,
			Payload: json.RawMessage(`{"text":"here it is"}`),
		}, true},
		"an empty answer": {v1.Event{
			Type:    v1.EventMessage,
			Payload: json.RawMessage(`{"text":"   "}`),
		}, false},
		"a tool call": {v1.Event{
			Type:    v1.EventTool,
			Payload: json.RawMessage(`{"toolCallId":"tc","status":"in_progress"}`),
		}, false},
		"the agent stream": {v1.Event{
			Type:    v1.EventAgentRaw,
			Payload: json.RawMessage(`{"sessionUpdate":"x"}`),
		}, false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := isAnswer(tc.event); got != tc.want {
				t.Fatalf("isAnswer = %v, want %v", got, tc.want)
			}
		})
	}
}

// The indicator advances, so a turn that is working looks like it.
func TestTheIndicatorAdvances(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{TypingIndicator: true})
	p.startTyping(context.Background(), "C1", "")

	p.advanceTyping(context.Background())

	if len(client.updated) != 1 {
		t.Fatalf("updated %d message(s), want one", len(client.updated))
	}
	if client.updated[0].Message.Text == typingFrames[0] {
		t.Error("the indicator did not advance")
	}
}

// A turn that ended without an answer stops ticking: a clock that ticks forever
// is a lie.
func TestAnIndicatorStopsAfterTheTimeout(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{TypingIndicator: true})
	p.startTyping(context.Background(), "C1", "")

	p.mu.Lock()
	p.typing[p.typingKey("C1")].started = time.Now().Add(-typingTimeout - time.Minute)
	p.mu.Unlock()

	p.advanceTyping(context.Background())

	if len(client.updated) != 0 {
		t.Fatalf("updated %d message(s), want none", len(client.updated))
	}
	if p.replaceTyping(context.Background(), "C1", textMessage("x")) {
		t.Fatal("the indicator should have been forgotten")
	}
}

// A successful prompt posts nothing of its own.
//
// The reaction on the message is the acknowledgement and the answer is the
// response; a message that only says "working on it" is noise between the two.
func TestASuccessfulPromptPostsNoAcknowledgement(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{TypingIndicator: true})

	// The indicator is one message; the outcome adds nothing.
	p.startTyping(context.Background(), "C1", "")

	if len(client.posted) != 1 {
		t.Fatalf("posted %d message(s), want only the indicator", len(client.posted))
	}

	// The answer replaces it, so the turn ends with one message.
	p.replaceTyping(context.Background(), "C1", textMessage("the answer"))

	if len(client.posted) != 1 {
		t.Fatalf("posted %d message(s) in total, want one", len(client.posted))
	}
}
