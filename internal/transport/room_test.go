package transport

import (
	"strings"
	"testing"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// A room is context, and the agent must be able to tell it from the request.
func TestRenderRoomLabelsItselfAsContext(t *testing.T) {
	room := []v1.ChannelMessage{
		{Author: "U1", Text: "the deploy failed on staging"},
		{Author: "U0BOT", Text: "I can look at the logs", Self: true},
		{Author: "U2", Text: "it is the migration step"},
	}

	rendered := renderRoom(room)

	if !strings.Contains(rendered, "This is context, not the request") {
		t.Errorf("the room should say what it is:\n%s", rendered)
	}
	if !strings.Contains(rendered, "The request follows") {
		t.Errorf("the room should say what comes next:\n%s", rendered)
	}
	for _, want := range []string{"the deploy failed on staging", "it is the migration step"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the room does not mention %q:\n%s", want, rendered)
		}
	}

	// The bot's own message is labelled as its own, so it does not read its output
	// back as if a person had said it.
	if !strings.Contains(rendered, "this agent: I can look at the logs") {
		t.Errorf("the bot's own message should be labelled:\n%s", rendered)
	}
}

func TestRenderRoomIsEmptyWithoutContext(t *testing.T) {
	if got := renderRoom(nil); got != "" {
		t.Fatalf("renderRoom(nil) = %q, want empty", got)
	}
}

// A long message is bounded, because the context costs tokens on every turn.
func TestRenderRoomBoundsLongMessages(t *testing.T) {
	room := []v1.ChannelMessage{{Author: "U1", Text: strings.Repeat("x", 2000)}}

	rendered := renderRoom(room)
	if len(rendered) > 800 {
		t.Fatalf("rendered %d characters, want the message bounded", len(rendered))
	}
	if !strings.Contains(rendered, "…") {
		t.Error("a truncated message should say so")
	}
}

// A message with no readable text is skipped rather than rendered as an empty line.
func TestRenderRoomSkipsEmptyMessages(t *testing.T) {
	room := []v1.ChannelMessage{
		{Author: "U1", Text: "   "},
		{Author: "U2", Text: "real content"},
	}

	rendered := renderRoom(room)
	if strings.Contains(rendered, "- U1") {
		t.Errorf("an empty message should be skipped:\n%s", rendered)
	}
	if !strings.Contains(rendered, "real content") {
		t.Errorf("the room lost its content:\n%s", rendered)
	}
}

// A channel with no context renders nothing, so no preamble is sent.
func TestRenderRoomWithoutMessages(t *testing.T) {
	if got := renderRoom([]v1.ChannelMessage{}); got != "" {
		t.Fatalf("renderRoom(empty) = %q, want empty", got)
	}
}
