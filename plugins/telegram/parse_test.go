package telegram

import (
	"testing"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// A private text message is a prompt addressed to the agent.
func TestAPrivateMessageIsAPrompt(t *testing.T) {
	env, ok := Parser{}.ParseUpdate(Update{
		UpdateID: 1,
		Message: &InboundMessage{
			MessageID: 10,
			From:      User{ID: 42},
			Chat:      Chat{ID: 42, Type: ChatPrivate},
			Text:      "hello there",
		},
	})
	if !ok {
		t.Fatal("a private message should parse")
	}
	if env.Kind != v1.EnvelopeMessage || env.Message.Text != "hello there" {
		t.Fatalf("env = %+v", env)
	}
	if env.Principal != "telegram:42" || env.ConversationID != "42" {
		t.Fatalf("env = %+v, want principal and conversation", env)
	}
}

// A group message that does not address the bot is ignored when mentions are
// required.
func TestAGroupMessageNeedsAddressing(t *testing.T) {
	parser := Parser{RequireMention: true, BotUsername: "hivebot", BotID: 99}
	message := &InboundMessage{
		MessageID: 10,
		From:      User{ID: 42},
		Chat:      Chat{ID: -100, Type: ChatSupergroup},
		Text:      "hello there",
	}
	if _, ok := parser.ParseUpdate(Update{UpdateID: 1, Message: message}); ok {
		t.Fatal("an unaddressed group message should be ignored")
	}
}

// An @mention of the bot in a group is addressing, and the mention is not part
// of the prompt.
func TestAMentionAddressesTheBot(t *testing.T) {
	parser := Parser{RequireMention: true, BotUsername: "hivebot"}
	env, ok := parser.ParseUpdate(Update{
		UpdateID: 1,
		Message: &InboundMessage{
			MessageID: 10,
			From:      User{ID: 42},
			Chat:      Chat{ID: -100, Type: ChatGroup},
			Text:      "@hivebot hello there",
			Entities:  []Entity{{Type: EntityMention, Offset: 0, Length: 8}},
		},
	})
	if !ok {
		t.Fatal("a mention should be addressed")
	}
	if env.Message == nil || env.Message.Text != "hello there" {
		t.Fatalf("env = %+v, want the mention stripped", env)
	}
}

// A reply to one of the bot's own messages is addressed.
func TestAReplyToTheBotIsAddressed(t *testing.T) {
	parser := Parser{RequireMention: true, BotID: 99}
	env, ok := parser.ParseUpdate(Update{
		UpdateID: 1,
		Message: &InboundMessage{
			MessageID: 11,
			From:      User{ID: 42},
			Chat:      Chat{ID: -100, Type: ChatGroup},
			Text:      "and this?",
			ReplyTo:   &InboundMessage{From: User{ID: 99, IsBot: true}},
		},
	})
	if !ok {
		t.Fatal("a reply to the bot should be addressed")
	}
	if env.Message == nil || env.Message.Text != "and this?" {
		t.Fatalf("env = %+v", env)
	}
}

// A slash command is a command, and the "@bot" group suffix is stripped.
func TestACommandIsRecognized(t *testing.T) {
	env, ok := Parser{}.ParseUpdate(Update{
		UpdateID: 1,
		Message: &InboundMessage{
			MessageID: 10,
			From:      User{ID: 42},
			Chat:      Chat{ID: -100, Type: ChatGroup},
			Text:      "/cancel@hivebot now",
		},
	})
	if !ok {
		t.Fatal("a command should parse")
	}
	if env.Command == nil || env.Command.Name != "cancel" || env.Command.Method != v1.MethodSessionCancel {
		t.Fatalf("env = %+v, want the cancel command", env)
	}
	if len(env.Command.Args) != 1 || env.Command.Args[0] != "now" {
		t.Fatalf("args = %v", env.Command.Args)
	}
}

// An unknown slash word is still a command — the router reports it rather than
// the text becoming a prompt.
func TestAnUnknownCommandIsACommand(t *testing.T) {
	env, ok := Parser{}.ParseUpdate(Update{
		UpdateID: 1,
		Message: &InboundMessage{
			MessageID: 10,
			From:      User{ID: 42},
			Chat:      Chat{ID: 42, Type: ChatPrivate},
			Text:      "/bogus",
		},
	})
	if !ok || env.Command == nil || env.Command.Name != "bogus" {
		t.Fatalf("env = %+v, want the unknown command", env)
	}
}

// A bot's own message and a message with no sender are not user intent.
func TestBotAndAnonymousMessagesAreIgnored(t *testing.T) {
	if _, ok := (Parser{}).ParseUpdate(Update{UpdateID: 1, Message: &InboundMessage{
		From: User{ID: 1, IsBot: true}, Chat: Chat{ID: 1, Type: ChatPrivate}, Text: "hi",
	}}); ok {
		t.Fatal("a bot message should be ignored")
	}
	if _, ok := (Parser{}).ParseUpdate(Update{UpdateID: 1, Message: &InboundMessage{
		Chat: Chat{ID: 1, Type: ChatPrivate}, Text: "hi",
	}}); ok {
		t.Fatal("a senderless message should be ignored")
	}
}

// A photo carries an attachment whose URL is the file id, which the transport
// resolves with getFile.
func TestAPhotoIsAnAttachment(t *testing.T) {
	env, ok := Parser{}.ParseUpdate(Update{
		UpdateID: 1,
		Message: &InboundMessage{
			MessageID: 10,
			From:      User{ID: 42},
			Chat:      Chat{ID: 42, Type: ChatPrivate},
			Caption:   "look",
			Photo: []PhotoSize{
				{FileID: "small", Width: 90},
				{FileID: "large", Width: 800},
			},
		},
	})
	if !ok || env.Message == nil {
		t.Fatal("a photo message should parse")
	}
	if env.Message.Text != "look" || len(env.Message.Attachments) != 1 {
		t.Fatalf("env = %+v", env.Message)
	}
	if env.Message.Attachments[0].URL != "large" {
		t.Fatalf("attachment = %+v, want the largest rendition", env.Message.Attachments[0])
	}
}

// A document keeps its name and media type.
func TestADocumentIsAnAttachment(t *testing.T) {
	env, ok := Parser{}.ParseUpdate(Update{
		UpdateID: 1,
		Message: &InboundMessage{
			MessageID: 10,
			From:      User{ID: 42},
			Chat:      Chat{ID: 42, Type: ChatPrivate},
			Document:  &Document{FileID: "f1", FileName: "report.pdf", MimeType: "application/pdf"},
		},
	})
	if !ok || len(env.Message.Attachments) != 1 {
		t.Fatal("a document should parse")
	}
	att := env.Message.Attachments[0]
	if att.Name != "report.pdf" || att.MimeType != "application/pdf" || att.URL != "f1" {
		t.Fatalf("attachment = %+v", att)
	}
}

// A button press is an interaction carrying its choice.
func TestACallbackIsAnInteraction(t *testing.T) {
	env, ok := Parser{}.ParseUpdate(Update{
		UpdateID: 2,
		CallbackQuery: &CallbackQuery{
			ID:      "cb1",
			From:    User{ID: 42},
			Data:    "perm:0",
			Message: &InboundMessage{Chat: Chat{ID: -100, Type: ChatGroup}},
		},
	})
	if !ok {
		t.Fatal("a callback should parse")
	}
	if env.Kind != v1.EnvelopeInteraction || env.Interaction == nil || env.Interaction.Value != "perm:0" {
		t.Fatalf("env = %+v", env)
	}
	if env.ConversationID != "-100" || env.Principal != "telegram:42" {
		t.Fatalf("env = %+v", env)
	}
}

// A callback with no data or no message is not something to answer.
func TestABadCallbackIsIgnored(t *testing.T) {
	if _, ok := (Parser{}).ParseUpdate(Update{UpdateID: 1, CallbackQuery: &CallbackQuery{
		ID: "cb1", From: User{ID: 42}, Data: "", Message: &InboundMessage{Chat: Chat{ID: 1}},
	}}); ok {
		t.Fatal("a callback with no data should be ignored")
	}
}

// Worded and button answers resolve to an option.
func TestPermissionDecisions(t *testing.T) {
	pending := &permissionRequest{
		AgentRequestID: "req1",
		Options: []permissionOption{
			{OptionID: "a1", Kind: "allow_once"},
			{OptionID: "d1", Kind: "reject_once"},
		},
	}

	id, approved, ok := permissionDecision("allow", pending)
	if !ok || !approved || id != "a1" {
		t.Fatalf("allow = %v %v %v", id, approved, ok)
	}

	id, approved, ok = permissionDecision("2", pending)
	if !ok || approved || id != "d1" {
		t.Fatalf("number = %v %v %v", id, approved, ok)
	}

	if _, _, ok := permissionDecision("sure why not", pending); ok {
		t.Fatal("a sentence is not a decision")
	}

	id, approved, ok = callbackDecision("perm:1", pending)
	if !ok || approved || id != "d1" {
		t.Fatalf("callback = %v %v %v", id, approved, ok)
	}
	if _, _, ok := callbackDecision("perm:9", pending); ok {
		t.Fatal("an out-of-range button is not a decision")
	}
}
