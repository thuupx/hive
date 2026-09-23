package slack

import (
	"testing"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

func parser() Parser {
	return Parser{BotUserID: "U0BOT", RequireMention: true}
}

func messageEvent(text string) MessageEvent {
	return MessageEvent{
		Type:      "message",
		Channel:   "C123",
		User:      "U123",
		Text:      text,
		Timestamp: "1700000000.000100",
	}
}

func TestParseCommandWithMention(t *testing.T) {
	env := parser().ParseMessage(messageEvent("<@U0BOT> /new_chat"))

	if env.Kind != v1.EnvelopeCommand {
		t.Fatalf("kind = %q, want command", env.Kind)
	}
	if env.Command.Name != "new_chat" {
		t.Errorf("name = %q", env.Command.Name)
	}
	if env.Command.Method != v1.MethodSessionCreate {
		t.Errorf("method = %q, want %q", env.Command.Method, v1.MethodSessionCreate)
	}
	if env.Principal != "slack:U123" {
		t.Errorf("principal = %q", env.Principal)
	}
	if env.ConversationID != "C123" {
		t.Errorf("conversation = %q", env.ConversationID)
	}
	if env.SourceID == "" {
		t.Error("a source id is required to make delivery idempotent")
	}
}

// The platform may allow a bare command name as well as a slash form.
func TestParseBareCommandName(t *testing.T) {
	env := parser().ParseMessage(messageEvent("<@U0BOT> new_chat"))

	if env.Kind != v1.EnvelopeCommand || env.Command.Method != v1.MethodSessionCreate {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestParseCommandWithArguments(t *testing.T) {
	env := parser().ParseMessage(messageEvent("<@U0BOT> /new_chat claude"))

	if env.Command.Name != "new_chat" {
		t.Fatalf("name = %q", env.Command.Name)
	}
	if len(env.Command.Args) != 1 || env.Command.Args[0] != "claude" {
		t.Fatalf("args = %v", env.Command.Args)
	}
}

// A recognized command the transport does not expose is a command error, not a
// prompt: the text must not be forwarded to the agent.
func TestParseUnknownCommand(t *testing.T) {
	env := parser().ParseMessage(messageEvent("<@U0BOT> /teleport"))

	if env.Kind != v1.EnvelopeCommand {
		t.Fatalf("kind = %q, want command", env.Kind)
	}
	if env.Command.Method != "" {
		t.Fatalf("method = %q, want empty so the router reports a command error", env.Command.Method)
	}
}

func TestParseOrdinaryTextIsAMessage(t *testing.T) {
	env := parser().ParseMessage(messageEvent("<@U0BOT> fix the login bug"))

	if env.Kind != v1.EnvelopeMessage {
		t.Fatalf("kind = %q, want message", env.Kind)
	}
	if env.Message.Text != "fix the login bug" {
		t.Fatalf("text = %q", env.Message.Text)
	}
}

// Text that merely contains a slash-like token must not become a command.
func TestParseSlashInsideTextIsNotACommand(t *testing.T) {
	env := parser().ParseMessage(messageEvent("<@U0BOT> check src/main.go and the /api route"))

	if env.Kind != v1.EnvelopeMessage {
		t.Fatalf("kind = %q, want message", env.Kind)
	}
}

// Without a mention in a channel that requires one, the transport ignores the
// message rather than guessing that Hive was addressed.
func TestParseIgnoresUnaddressedMessages(t *testing.T) {
	env := parser().ParseMessage(messageEvent("/agents"))

	if env.Kind != "" {
		t.Fatalf("kind = %q, want the message to be ignored", env.Kind)
	}
}

func TestParseWithoutMentionRequirement(t *testing.T) {
	p := Parser{BotUserID: "U0BOT", RequireMention: false}

	env := p.ParseMessage(messageEvent("/agents"))
	if env.Kind != v1.EnvelopeCommand || env.Command.Method != v1.MethodAgentList {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestParseInteraction(t *testing.T) {
	payload := ActionPayload{
		Type:     "block_actions",
		ActionTS: "1700000000.000200",
	}
	payload.User.ID = "U123"
	payload.Channel.ID = "C123"
	payload.Actions = []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	}{{ActionID: ActionPermissionAllow, Value: `{"agentRequestId":"7","approved":true}`}}

	env := parser().ParseInteraction(payload)

	if env.Kind != v1.EnvelopeInteraction {
		t.Fatalf("kind = %q, want interaction", env.Kind)
	}
	if env.Interaction.Action != ActionPermissionAllow {
		t.Errorf("action = %q", env.Interaction.Action)
	}
	if env.Principal != "slack:U123" {
		t.Errorf("principal = %q", env.Principal)
	}
	if env.SourceID == "" {
		t.Error("a source id is required to make a repeated click idempotent")
	}
}

// The catalog is generated from the command map, so a command cannot exist in one
// place and be missing from the other.
func TestCatalogMatchesCommands(t *testing.T) {
	catalog := Catalog()
	if len(catalog) != len(Commands) {
		t.Fatalf("catalog = %d entries, want %d", len(catalog), len(Commands))
	}
	for _, entry := range catalog {
		if Commands[entry.Command] != entry.Method {
			t.Errorf("catalog entry %q does not match the command map", entry.Command)
		}
		if entry.Description == "" {
			t.Errorf("catalog entry %q has no description", entry.Command)
		}
	}
}
