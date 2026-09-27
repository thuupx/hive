package zalo

import (
	"encoding/json"
	"testing"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// testUpdate is a text message from a person, as Zalo sends one.
func testUpdate(text string) Update {
	return Update{
		EventName: EventText,
		Message: &InboundMessage{
			From:      User{ID: "u1", DisplayName: "Ted"},
			Chat:      Chat{ID: "c1", ChatType: ChatPrivate},
			Text:      text,
			MessageID: "m1",
		},
	}
}

// A command is recognized in the first token, with or without a slash, and
// ordinary text is not a command.
func TestParseCommand(t *testing.T) {
	for _, tc := range []struct {
		text   string
		name   string
		method string
		ok     bool
	}{
		{"/status", "status", v1.MethodSessionStatus, true},
		{"status", "status", v1.MethodSessionStatus, true},
		{"new_chat hermes", "new_chat", v1.MethodSessionCreate, true},
		{"check src/main.go", "", "", false},
		{"hey status", "", "", false},
		{"/teleport", "teleport", "", true},
	} {
		t.Run(tc.text, func(t *testing.T) {
			name, method, _, ok := parseCommand(tc.text)
			if ok != tc.ok || name != tc.name || method != tc.method {
				t.Fatalf("parseCommand(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.text, name, method, ok, tc.name, tc.method, tc.ok)
			}
		})
	}
}

// A private message is always addressed; a group message is not, unless it
// addresses the bot or opens with a command.
func TestGroupAddressing(t *testing.T) {
	group := testUpdate("hello there")
	group.Message.Chat = Chat{ID: "g1", ChatType: ChatGroup}

	p := Parser{BotName: "Hive", RequireMention: true}
	if _, ok := p.ParseUpdate(group); ok {
		t.Fatal("a group message that does not address the bot should be ignored")
	}

	mentioned := testUpdate("@Hive what is this?")
	mentioned.Message.Chat = Chat{ID: "g1", ChatType: ChatGroup}
	env, ok := p.ParseUpdate(mentioned)
	if !ok {
		t.Fatal("a mention should address the bot")
	}
	if env.Message.Text != "what is this?" {
		t.Fatalf("text = %q, want the mention removed", env.Message.Text)
	}

	command := testUpdate("/status")
	command.Message.Chat = Chat{ID: "g1", ChatType: ChatGroup}
	if _, ok := p.ParseUpdate(command); !ok {
		t.Fatal("a command should address the bot in a group")
	}
}

// A private chat needs no mention.
func TestPrivateChatNeedsNoMention(t *testing.T) {
	p := Parser{RequireMention: true}
	env, ok := p.ParseUpdate(testUpdate("hello"))
	if !ok {
		t.Fatal("a private message should be for Hive")
	}
	if env.Kind != v1.EnvelopeMessage || env.Message.Text != "hello" {
		t.Fatalf("envelope = %+v, want a plain message", env)
	}
}

// A bot's own message is not user intent.
func TestABotMessageIsIgnored(t *testing.T) {
	update := testUpdate("hello")
	update.Message.From.IsBot = true
	if _, ok := (Parser{}).ParseUpdate(update); ok {
		t.Fatal("a bot message should be ignored")
	}
}

// An image without a caption still carries the picture, so it is not dropped.
func TestAnImageWithoutACaptionStillPrompts(t *testing.T) {
	update := Update{
		EventName: EventImage,
		Message: &InboundMessage{
			From:      User{ID: "u1"},
			Chat:      Chat{ID: "c1", ChatType: ChatPrivate},
			Photo:     "https://cdn.example/a.png",
			MessageID: "m2",
		},
	}
	env, ok := (Parser{}).ParseUpdate(update)
	if !ok {
		t.Fatal("an image should parse")
	}
	if env.Message.Text == "" {
		t.Error("the prompt must carry text, or the core refuses it")
	}
	if len(env.Message.Attachments) != 1 {
		t.Fatalf("attachments = %d, want the picture", len(env.Message.Attachments))
	}
	if got := env.Message.Attachments[0].MimeType; got != "image/png" {
		t.Errorf("mime = %q, want image/png from the URL", got)
	}
}

// A sticker or a voice note is not something the agent can act on.
func TestUnsupportedEventsAreIgnored(t *testing.T) {
	for _, name := range []string{EventSticker, EventVoice, EventUnsupported} {
		update := Update{EventName: name, Message: testUpdate("x").Message}
		if _, ok := (Parser{}).ParseUpdate(update); ok {
			t.Errorf("%s should be ignored", name)
		}
	}
}

// The source id is stable, so a redelivery resolves to the same operation.
func TestSourceIDIsStable(t *testing.T) {
	update := testUpdate("hello")
	env, _ := (Parser{}).ParseUpdate(update)
	if env.SourceID != "event:c1:m1" {
		t.Fatalf("source id = %q", env.SourceID)
	}
	if env.Principal != "zalo:u1" {
		t.Fatalf("principal = %q", env.Principal)
	}
}

// A permission is answered in words, because Zalo has no buttons.
func TestPermissionDecision(t *testing.T) {
	pending := &permissionRequest{
		AgentRequestID: "req1",
		Options: []permissionOption{
			{OptionID: "allow_once", Name: "Allow once", Kind: "allow_once"},
			{OptionID: "allow_always", Name: "Allow always", Kind: "allow_always"},
			{OptionID: "reject", Name: "Deny", Kind: "reject_once"},
		},
	}

	for _, tc := range []struct {
		text     string
		optionID string
		approved bool
		ok       bool
	}{
		{"allow", "allow_once", true, true},
		{"deny", "reject", false, true},
		{"2", "allow_always", true, true},
		{"3", "reject", false, true},
		{"hello there", "", false, false},
		{"4", "", false, false},
	} {
		t.Run(tc.text, func(t *testing.T) {
			optionID, approved, ok := permissionDecision(tc.text, pending)
			if ok != tc.ok || optionID != tc.optionID || approved != tc.approved {
				t.Fatalf("permissionDecision(%q) = (%q, %v, %v), want (%q, %v, %v)",
					tc.text, optionID, approved, ok, tc.optionID, tc.approved, tc.ok)
			}
		})
	}
}

// A decision is relayed as an interaction, which is the envelope the router
// performs a permission response with.
func TestPermissionEnvelopeIsAnInteraction(t *testing.T) {
	d := delivery{
		envelope: v1.Envelope{
			Transport:      Name,
			ConversationID: "c1",
			Principal:      "zalo:u1",
			SourceID:       "event:c1:m1",
			Kind:           v1.EnvelopeMessage,
			Message:        &v1.IncomingMessage{Text: "allow"},
		},
		conversationID: "c1",
		chatID:         "c1",
	}
	pending := &permissionRequest{AgentRequestID: "req1"}

	env := permissionEnvelope(d, pending, "allow_once", true)
	if env.Kind != v1.EnvelopeInteraction {
		t.Fatalf("kind = %q, want an interaction", env.Kind)
	}
	if env.Interaction == nil || env.Interaction.Method != v1.MethodPermissionRespond {
		t.Fatalf("interaction = %+v, want a permission response", env.Interaction)
	}

	var value struct {
		AgentRequestID string `json:"agentRequestId"`
		Approved       bool   `json:"approved"`
		OptionID       string `json:"optionId"`
	}
	if err := json.Unmarshal([]byte(env.Interaction.Value), &value); err != nil {
		t.Fatalf("decode value: %v", err)
	}
	if value.AgentRequestID != "req1" || !value.Approved || value.OptionID != "allow_once" {
		t.Fatalf("value = %+v, want the decision", value)
	}
}
