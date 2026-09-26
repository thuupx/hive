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
	deleted  []string
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

func (c *fakeClient) DeleteMessage(_ context.Context, _ string, timestamp string) error {
	c.deleted = append(c.deleted, timestamp)
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

// The indicator is removed when the turn ends, and the answer is its own message.
//
// Found in a live conversation: the answer was edited into the indicator's
// message, so it landed above the turn's own tool list — out of order — and an
// answer longer than 4,000 characters could not be posted at all, because
// chat.update refuses a text that long while chat.postMessage accepts it.
func TestTheIndicatorIsRemovedRatherThanEditedIntoTheAnswer(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{TypingIndicator: true})

	p.startTyping(context.Background(), "C1", "0.9")

	if len(client.posted) != 1 {
		t.Fatalf("posted %d message(s), want one indicator", len(client.posted))
	}
	if client.posted[0].ThreadTS != "0.9" {
		t.Errorf("the indicator went to thread %q, want the turn's", client.posted[0].ThreadTS)
	}

	p.stopTyping(context.Background(), "C1")

	if len(client.deleted) != 1 {
		t.Fatalf("deleted %d message(s), want the indicator", len(client.deleted))
	}
	if client.deleted[0] != "1.0" {
		t.Errorf("deleted %q, want the indicator's message", client.deleted[0])
	}
	if len(client.updated) != 0 {
		t.Errorf("updated %d message(s), want none: an answer is posted, never edited into the indicator",
			len(client.updated))
	}

	// A turn that ends twice removes one indicator, not two.
	p.stopTyping(context.Background(), "C1")
	if len(client.deleted) != 1 {
		t.Fatalf("deleted %d message(s), want one", len(client.deleted))
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

	p.stopTyping(context.Background(), "C1")
	if len(client.deleted) != 0 {
		t.Fatalf("deleted %d message(s), want none", len(client.deleted))
	}
}

// An indicator that cannot be shown does not fail the turn.
func TestAnIndicatorThatCannotBeShownIsNotFatal(t *testing.T) {
	client := &fakeClient{failPost: true}
	p := New(nil, client, Options{TypingIndicator: true})

	p.startTyping(context.Background(), "C1", "")

	// Nothing to remove, and that is not a failure.
	p.stopTyping(context.Background(), "C1")
	if len(client.deleted) != 0 {
		t.Fatalf("deleted %d message(s), want none", len(client.deleted))
	}
}

// Only an answer ends the turn. A tool card is a step, and the turn is still
// working while it happens.
func TestOnlyAnAnswerEndsTheTurn(t *testing.T) {
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
	p.stopTyping(context.Background(), "C1")
	if len(client.deleted) != 0 {
		t.Fatalf("deleted %d message(s), want none: the indicator was already forgotten", len(client.deleted))
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

	// Nothing is posted for a turn that is still running.
	if len(client.posted) != 1 {
		t.Fatalf("posted %d message(s) in total, want one", len(client.posted))
	}
}

// A delivery that fails takes the indicator down.
//
// Found in a live conversation: a restart killed a delivery, and the clock it had
// posted kept ticking for the rest of its thirty minutes, saying the turn was
// still working when it was not.
func TestAFailedDeliveryTakesTheIndicatorDown(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{TypingIndicator: true})

	p.startTyping(context.Background(), "C1", "")

	// What the transport does when the delivery fails: the indicator goes, and
	// what happened is posted as its own message.
	p.stopTyping(context.Background(), "C1")

	if len(client.deleted) != 1 {
		t.Fatalf("deleted %d message(s), want the indicator", len(client.deleted))
	}
	// And there is nothing left to tick.
	p.stopTyping(context.Background(), "C1")
	if len(client.deleted) != 1 {
		t.Fatalf("deleted %d message(s), want one", len(client.deleted))
	}
}
