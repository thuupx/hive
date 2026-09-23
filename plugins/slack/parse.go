// Package slack implements the Slack transport.
//
// The transport owns Slack syntax: mentions, slash commands, aliases, buttons,
// and Block Kit. Hive owns operation semantics. Nothing Slack-specific reaches
// the core, and this package must not import any internal Hive package except the
// transport boundary it normalizes into.
package slack

import (
	"fmt"
	"sort"
	"strings"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// Name is the transport name. It is also the prefix of the principals this
// transport may assert.
const Name = "slack"

// CommandMap maps a transport command name to a Hive method.
//
// The map is transport-owned: it decides which operations this transport exposes
// and what to call them. It lives with the transport rather than in the protocol
// because Hive core must not contain a Slack command registry.
type CommandMap map[string]string

// Commands is the transport command catalog.
//
// The catalog is transport-owned: it decides which operations this transport
// exposes and what to call them. `/new_chat` is a presentation-level alias and
// never a Hive API method.
var Commands = CommandMap{
	"new_chat": v1.MethodSessionCreate,
	"new":      v1.MethodSessionCreate,
	"agents":   v1.MethodAgentList,
	"nodes":    v1.MethodNodeList,
	"status":   v1.MethodSessionStatus,
	"cancel":   v1.MethodSessionCancel,
	"handoff":  v1.MethodSessionHandoff,
	"model":    v1.MethodSessionConfig,
	"models":   v1.MethodSessionConfig,
	"mode":     v1.MethodSessionConfig,
	"sessions": v1.MethodSessionList,

	// help has no Hive method: the catalog is the transport's own, so the
	// transport answers it without asking the core.
	"help": "",
}

// ConfigIDs maps a transport command to the agent selector it targets.
//
// The transport owns this mapping: it decides that "/model" is about the selector
// called model. Hive never learns the name.
var ConfigIDs = map[string]string{
	"model":  v1.ConfigCategoryModel,
	"models": v1.ConfigCategoryModel,
	"mode":   v1.ConfigCategoryMode,
}

// Catalog is the discoverable command list this transport exposes.
//
// It is generated from the catalog rather than duplicated, so a command cannot
// exist in one place and be missing from the other.
type CatalogEntry struct {
	Command     string
	Method      string
	Description string
}

// Descriptions are the human-readable labels for the catalog.
var Descriptions = map[string]string{
	"new_chat": "Create a new Hive session",
	"new":      "Create a new Hive session",
	"agents":   "List available agents",
	"nodes":    "List connected nodes",
	"status":   "Show the current session status",
	"cancel":   "Cancel the current run",
	"handoff":  "Hand off the session to another agent",
	"model":    "Show or switch the agent model",
	"models":   "Show the agent models",
	"mode":     "Show or switch the agent session mode",
	"sessions": "List your sessions",
	"help":     "Show this list",
}

// HelpCommand is the transport command that shows the catalog.
const HelpCommand = "help"

// Catalog returns the commands this transport exposes, in a stable order.
func Catalog() []CatalogEntry {
	names := make([]string, 0, len(Commands))
	for name := range Commands {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]CatalogEntry, 0, len(names))
	for _, name := range names {
		out = append(out, CatalogEntry{
			Command:     name,
			Method:      Commands[name],
			Description: Descriptions[name],
		})
	}
	return out
}

// Parser turns Slack input into transport envelopes.
type Parser struct {
	// BotUserID is the bot's own Slack user id, used to resolve mentions.
	BotUserID string

	// RequireMention makes the transport ignore messages that do not address the
	// bot. A channel where Hive is the only participant can turn it off.
	RequireMention bool
}

// ParseMessage classifies one inbound Slack message.
//
// The parser resolves platform addressing first, then parses command syntax. A
// command is only recognized in the first token, so ordinary text that merely
// contains a slash-like token stays a prompt.
func (p Parser) ParseMessage(ev MessageEvent) v1.Envelope {
	env := v1.Envelope{
		Transport:      Name,
		ConversationID: ev.Channel,
		Principal:      Name + ":" + ev.User,
		SourceID:       sourceID(ev.Channel, ev.Timestamp),
	}

	text, addressed := p.stripMention(ev.Text)
	if p.RequireMention && !addressed {
		// Not addressed to Hive: the transport ignores it rather than guessing.
		return env
	}

	if name, method, args, ok := parseCommand(text); ok {
		env.Kind = v1.EnvelopeCommand
		env.Command = &v1.IncomingCommand{
			Name:     name,
			Method:   method,
			Args:     args,
			ConfigID: ConfigIDs[name],
		}
		return env
	}

	env.Kind = v1.EnvelopeMessage
	env.Message = &v1.IncomingMessage{
		Text:        strings.TrimSpace(text),
		Attachments: attachmentsOf(ev.Files),
	}
	return env
}

// attachmentsOf carries the files a user sent.
//
// Only the transport knows how to fetch them, so the bytes are filled in later.
// A file this transport cannot read is skipped rather than failing the message.
func attachmentsOf(files []File) []v1.Attachment {
	out := make([]v1.Attachment, 0, len(files))
	for _, file := range files {
		url := file.DownloadURL()
		if url == "" {
			continue
		}
		out = append(out, v1.Attachment{
			Name:     file.Name,
			MimeType: file.MimeType,
			URL:      url,
		})
	}
	return out
}

// ParseInteraction turns a Slack block action into an interaction envelope.
//
// Buttons, menus, and modal submissions are interactions, not a second
// business-logic system: they carry the authenticated principal and a stable
// action identity, and Hive performs the domain transition.
func (p Parser) ParseInteraction(payload ActionPayload) v1.Envelope {
	actionID, value := payload.Action()

	return v1.Envelope{
		Transport:      Name,
		ConversationID: payload.Channel.ID,
		Principal:      Name + ":" + payload.User.ID,
		SourceID:       sourceID(payload.Channel.ID, payload.ActionTS),
		Kind:           v1.EnvelopeInteraction,
		Interaction: &v1.IncomingInteraction{
			Action: actionID,
			Value:  value,
		},
	}
}

// stripMention removes a bot mention and reports whether the message addressed it.
//
// Slack puts the mention where the user typed it, not necessarily first. Looking
// only at the start would ignore "hey @Hive, what is this?", which is a message
// that plainly addresses the bot.
func (p Parser) stripMention(text string) (string, bool) {
	if p.BotUserID == "" {
		return text, false
	}

	mention := "<@" + p.BotUserID + ">"
	if !strings.Contains(text, mention) {
		return text, false
	}

	// Remove every occurrence: a user may mention the bot more than once, and
	// none of them belong in the prompt.
	stripped := strings.ReplaceAll(text, mention, " ")
	return strings.Join(strings.Fields(stripped), " "), true
}

// parseCommand recognizes a command in the first token and resolves it.
//
// Both `/new_chat` and a bare `new_chat` are accepted when the platform allows
// them, because a transport may support either. Anything else is ordinary text.
//
// A recognized but unknown command returns ok=true with an empty method, so the
// router reports a command error instead of forwarding the text as a prompt.
func parseCommand(text string) (name, method string, args []string, ok bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", "", nil, false
	}

	first := fields[0]
	switch {
	case strings.HasPrefix(first, "/"):
		name = strings.TrimPrefix(first, "/")
	case isKnownCommand(first):
		name = first
	default:
		return "", "", nil, false
	}

	resolved, known := Commands[name]
	if !known {
		// The transport's grammar recognized a command form, but it is not one
		// this transport exposes.
		return name, "", fields[1:], true
	}
	return name, resolved, fields[1:], true
}

func isKnownCommand(name string) bool {
	_, ok := Commands[name]
	return ok
}

// sourceID is the platform's stable identifier for a delivery.
func sourceID(channel, timestamp string) string {
	if channel == "" || timestamp == "" {
		return ""
	}
	return fmt.Sprintf("event:%s:%s", channel, timestamp)
}
