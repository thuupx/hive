package slack

import (
	"strings"
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

// Slack puts the mention where the user typed it. A message that addresses the bot
// anywhere is addressed to the bot.
func TestParseMentionAnywhereInTheMessage(t *testing.T) {
	cases := map[string]struct {
		kind   v1.EnvelopeKind
		text   string
		method string
	}{
		"mention first, command":   {v1.EnvelopeCommand, "", v1.MethodAgentList},
		"mention first, prompt":    {v1.EnvelopeMessage, "fix the login bug", ""},
		"mention after a greeting": {v1.EnvelopeMessage, "hey /agents", ""},
		"mention at the end":       {v1.EnvelopeMessage, "what is this", ""},
	}

	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			var text string
			switch name {
			case "mention first, command":
				text = "<@U0BOT> /agents"
			case "mention first, prompt":
				text = "<@U0BOT> fix the login bug"
			case "mention after a greeting":
				text = "hey <@U0BOT> /agents"
			case "mention at the end":
				text = "what is this <@U0BOT>"
			}

			env := parser().ParseMessage(messageEvent(text))
			if env.Kind != want.kind {
				t.Fatalf("kind = %q, want %q", env.Kind, want.kind)
			}
			switch want.kind {
			case v1.EnvelopeCommand:
				if env.Command.Method != want.method {
					t.Errorf("method = %q, want %q", env.Command.Method, want.method)
				}
			case v1.EnvelopeMessage:
				if env.Message.Text != want.text {
					t.Errorf("text = %q, want %q", env.Message.Text, want.text)
				}
			}
		})
	}
}

// A message with no mention at all is not addressed to the bot.
func TestParseIgnoresAMessageWithNoMention(t *testing.T) {
	env := parser().ParseMessage(messageEvent("hello everyone"))
	if env.Kind != "" {
		t.Fatalf("kind = %q, want the message to be ignored", env.Kind)
	}
}

// A repeated mention is removed from the prompt.
func TestParseRemovesEveryMention(t *testing.T) {
	env := parser().ParseMessage(messageEvent("<@U0BOT> thanks <@U0BOT>"))

	if env.Kind != v1.EnvelopeMessage {
		t.Fatalf("kind = %q", env.Kind)
	}
	if strings.Contains(env.Message.Text, "<@") {
		t.Fatalf("text = %q, want the mentions removed", env.Message.Text)
	}
	if env.Message.Text != "thanks" {
		t.Fatalf("text = %q, want %q", env.Message.Text, "thanks")
	}
}

// Slack linkifies @Hive into a mention entity. Both the command form and the bare
// form are accepted, because Slack reserves a leading slash for itself.
func TestParseAcceptsSlashAndBareForms(t *testing.T) {
	for _, text := range []string{"<@U0BOT> /agents", "<@U0BOT> agents"} {
		env := parser().ParseMessage(messageEvent(text))
		if env.Kind != v1.EnvelopeCommand || env.Command.Method != v1.MethodAgentList {
			t.Errorf("%q parsed as %+v, want the agents command", text, env)
		}
	}
}

// One platform message can arrive as more than one event, and the conversation
// must see one delivery.
func TestAlreadyHandledDeduplicatesADelivery(t *testing.T) {
	p := New(nil, nil, Options{})

	if p.alreadyHandled("event:C1:1") {
		t.Fatal("the first delivery is not a duplicate")
	}
	if !p.alreadyHandled("event:C1:1") {
		t.Fatal("the second delivery of the same event is a duplicate")
	}
	if p.alreadyHandled("event:C1:2") {
		t.Fatal("a different event is not a duplicate")
	}
	if p.alreadyHandled("") {
		t.Fatal("a delivery with no source id cannot be deduplicated")
	}
}

// A message with a file is still a message: it arrives with the file_share
// subtype, which is not a reason to drop it.
func TestParseImageMessage(t *testing.T) {
	event := messageEvent("<@U0BOT> what is in this picture?")
	event.SubType = "file_share"
	event.Files = []File{{
		ID:                 "F1",
		Name:               "screenshot.png",
		MimeType:           "image/png",
		Size:               1234,
		URLPrivateDownload: "https://files.slack.com/files-pri/T1-F1/download",
	}}

	env := parser().ParseMessage(event)

	if env.Kind != v1.EnvelopeMessage {
		t.Fatalf("kind = %q, want message", env.Kind)
	}
	if env.Message.Text != "what is in this picture?" {
		t.Errorf("text = %q", env.Message.Text)
	}
	if len(env.Message.Attachments) != 1 {
		t.Fatalf("attachments = %+v", env.Message.Attachments)
	}

	attachment := env.Message.Attachments[0]
	if attachment.Name != "screenshot.png" || attachment.MimeType != "image/png" {
		t.Errorf("attachment = %+v", attachment)
	}
	if !attachment.IsImage() {
		t.Error("a png should be an image")
	}
	// The bytes are fetched by the transport, so the envelope carries where from.
	if attachment.URL == "" {
		t.Error("the attachment should carry where to fetch it from")
	}
	if len(attachment.Data) != 0 {
		t.Error("the parser does not fetch, so no data yet")
	}
}

// A file with no download URL is skipped rather than failing the message.
func TestParseSkipsAFileWithNoURL(t *testing.T) {
	event := messageEvent("<@U0BOT> here")
	event.SubType = "file_share"
	event.Files = []File{{ID: "F1", Name: "broken.png", MimeType: "image/png"}}

	env := parser().ParseMessage(event)
	if len(env.Message.Attachments) != 0 {
		t.Fatalf("attachments = %+v", env.Message.Attachments)
	}
}
