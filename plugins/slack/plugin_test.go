package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// A long-lived transport must not remember every turn it has shown.
func TestToolRunsAreBounded(t *testing.T) {
	p := New(nil, nil, Options{})

	for i := 0; i < maxToolRuns+10; i++ {
		p.mu.Lock()
		p.rememberToolRunLocked(fmt.Sprintf("run_%d", i))
		p.mu.Unlock()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.toolRuns) != maxToolRuns {
		t.Fatalf("tool runs = %d, want %d", len(p.toolRuns), maxToolRuns)
	}
	if _, ok := p.toolRuns["run_0"]; ok {
		t.Error("the oldest turn should have been forgotten")
	}
}

// A turn's tool calls are one message, not one per call.
func TestATurnsToolCallsAreOneMessage(t *testing.T) {
	message := toolRunMessage([]v1.ToolCall{
		{ToolCallID: "a", Title: "Read file", Status: v1.ToolCompleted},
		{ToolCallID: "b", Title: "go test ./...", Status: v1.ToolInProgress},
	})

	for _, want := range []string{
		"2 tool calls",
		"Read file",
		"go test ./...",
		":white_check_mark:",
		":hourglass_flowing_sand:",
	} {
		if !strings.Contains(message.Text, want) {
			t.Errorf("the list does not mention %q: %q", want, message.Text)
		}
	}
}

// An update replaces a call in place, so the list does not reorder under the
// reader while a turn is running.
func TestAToolCallUpdateKeepsItsPosition(t *testing.T) {
	run := &toolRun{index: map[string]int{}}
	run.upsert(v1.ToolCall{ToolCallID: "a", Title: "first", Status: v1.ToolPending})
	run.upsert(v1.ToolCall{ToolCallID: "b", Title: "second", Status: v1.ToolPending})
	run.upsert(v1.ToolCall{ToolCallID: "a", Title: "first", Status: v1.ToolCompleted})

	if len(run.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(run.calls))
	}
	if run.calls[0].ToolCallID != "a" || run.calls[0].Status != v1.ToolCompleted {
		t.Fatalf("calls[0] = %+v, want the first call updated in place", run.calls[0])
	}
}

// A card loses its buttons once the decision was made.
//
// Found in a live conversation: the card kept offering Allow after the answer
// was given, and a second press was refused with "not pending", which reads as
// the first press having failed.
func TestAPermissionCardIsSettledAfterADecision(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{})

	p.settleInteraction(context.Background(), delivery{
		conversationID: "C1:1.0",
		messageTS:      "1.5",
		envelope: v1.Envelope{
			Kind: v1.EnvelopeInteraction,
			Interaction: &v1.IncomingInteraction{
				Action: ActionPermissionRespond,
				Method: v1.MethodPermissionRespond,
				Value:  `{"agentRequestId":"e1bf","optionId":"allow-always","approved":true,"label":"Allow always"}`,
			},
		},
	})

	if len(client.updated) != 1 {
		t.Fatalf("updated %d message(s), want the card", len(client.updated))
	}
	updated := client.updated[0]
	if updated.Timestamp != "1.5" {
		t.Errorf("timestamp = %q, want the card's", updated.Timestamp)
	}
	for _, block := range updated.Message.Blocks {
		if _, ok := block.(*slackgo.ActionBlock); ok {
			t.Error("the card still has an actions block, so its buttons are still there")
		}
	}
	if !strings.Contains(updated.Message.Text, "Allow always") {
		t.Errorf("text = %q, want the choice that was made", updated.Message.Text)
	}
}

// A card the platform did not name cannot be settled, and that is not a failure.
func TestASettleWithoutAMessageDoesNothing(t *testing.T) {
	client := &fakeClient{}
	p := New(nil, client, Options{})

	p.settleInteraction(context.Background(), delivery{conversationID: "C1:1.0"})

	if len(client.updated) != 0 {
		t.Fatalf("updated %d message(s), want none", len(client.updated))
	}
}

// A command answered while a turn runs does not take the turn's indicator.
//
// Found in a live conversation: a /status sent mid-turn replaced the "working"
// message, so the agent looked like it had stopped. It had not — it was waiting
// on a permission request whose card Slack had refused.
func TestACommandDoesNotEndTheTurn(t *testing.T) {
	message := delivery{envelope: v1.Envelope{Kind: v1.EnvelopeMessage}}
	command := delivery{envelope: v1.Envelope{Kind: v1.EnvelopeCommand}}

	for _, tc := range []struct {
		name    string
		d       delivery
		outcome v1.TransportOutcome
		want    bool
	}{
		{"a message is the turn", message,
			v1.TransportOutcome{Method: v1.MethodSessionPrompt}, true},
		{"a failed message has no turn to show", message,
			v1.TransportOutcome{Method: v1.MethodSessionPrompt, Error: v1.Internal("no")}, true},
		{"a status read is its own message", command,
			v1.TransportOutcome{Method: v1.MethodSessionStatus}, false},
		{"a settings change is its own message", command,
			v1.TransportOutcome{Method: v1.MethodSessionConfig}, false},
		{"a cancel ends the turn it was shown for", command,
			v1.TransportOutcome{Method: v1.MethodSessionCancel}, true},
		{"a failed cancel leaves the turn running", command,
			v1.TransportOutcome{Method: v1.MethodSessionCancel, Error: v1.Conflict("nothing to cancel")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := endsTurn(tc.d, tc.outcome); got != tc.want {
				t.Errorf("endsTurn = %v, want %v", got, tc.want)
			}
		})
	}
}

// A delivery carries the thread its turn belongs to.
//
// This is the wiring, not the rule: the rule was tested and correct while the
// delivery never called it, so every channel reply stayed flat.
func TestChannelDeliveryCarriesItsThread(t *testing.T) {
	p := New(nil, nil, Options{})

	inChannel := testMessage("<@U0BOT> hello")
	inChannel.TimeStamp = "1.0"

	d, ok := p.parse(eventsInbound(inChannel))
	if !ok {
		t.Fatal("the message should be for Hive")
	}
	if d.thread != "1.0" {
		t.Fatalf("thread = %q, want the message timestamp", d.thread)
	}

	// A direct message stays flat.
	direct := testMessage("hello")
	direct.Channel = "D123"
	direct.TimeStamp = "1.0"
	dm, ok := p.parse(eventsInbound(direct))
	if !ok {
		t.Fatal("the direct message should be for Hive")
	}
	if dm.thread != "" {
		t.Fatalf("a direct message thread = %q, want empty", dm.thread)
	}
}

// eventsInbound is an Events API delivery carrying one message.
func eventsInbound(event slackevents.MessageEvent) Inbound {
	return Inbound{Events: &slackevents.EventsAPIEvent{
		Type:       slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: event.Type, Data: &event},
	}}
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

	inThread := testMessage("hello")
	inThread.Channel = "C1"
	inThread.User = "U1"
	inThread.TimeStamp = "1.2"
	inThread.ThreadTimeStamp = "1.0"

	d, ok := p.parse(eventsInbound(inThread))
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
