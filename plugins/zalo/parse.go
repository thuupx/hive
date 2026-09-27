// Package zalo implements the Zalo Bot transport.
//
// The transport owns Zalo syntax: commands, aliases, permission replies, and
// text formatting. Hive owns operation semantics. Nothing Zalo-specific reaches
// the core, and this package must not import any internal Hive package except the
// transport boundary it normalizes into.
package zalo

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// Name is the transport name. It is also the prefix of the principals this
// transport may assert.
const Name = "zalo"

// CommandMap maps a transport command name to a Hive method.
//
// The map is transport-owned: it decides which operations this transport exposes
// and what to call them. It lives with the transport rather than in the protocol
// because Hive core must not contain a Zalo command registry.
type CommandMap map[string]string

// Commands is the transport command catalog.
//
// Zalo does not intercept a leading slash, so both `/new_chat` and a bare
// `new_chat` reach the bot. The slash is presentation; the transport accepts
// either.
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
	"logs":     v1.MethodSessionEvents,

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

// CatalogEntry is one discoverable command.
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
	"logs":     "Show recent activity for this conversation",
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

// Parser turns Zalo input into transport envelopes.
type Parser struct {
	// RequireMention makes the transport ignore a group message that does not
	// address the bot. A private chat is 1:1, so it is always addressed and this
	// does not apply to it.
	RequireMention bool

	// BotName is the bot's display name, used to recognize addressing by name.
	BotName string
}

// ParseUpdate classifies one inbound delivery.
//
// The parser resolves platform addressing first, then parses command syntax. A
// command is only recognized in the first token, so ordinary text that merely
// contains a slash-like token stays a prompt.
func (p Parser) ParseUpdate(update Update) (v1.Envelope, bool) {
	message := update.Message
	if message == nil {
		return v1.Envelope{}, false
	}
	// A bot's own message is not user intent, and a message with no sender
	// cannot be authorized.
	if message.From.IsBot || message.From.ID == "" {
		return v1.Envelope{}, false
	}

	text, attachments, ok := messageContent(update)
	if !ok {
		// A sticker, a voice note, or an unsupported message is not something
		// the agent can act on, and guessing at it would be worse than saying
		// nothing.
		return v1.Envelope{}, false
	}

	env := v1.Envelope{
		Transport:      Name,
		ConversationID: message.Chat.ID,
		Principal:      Name + ":" + message.From.ID,
		SourceID:       sourceID(message.Chat.ID, message.MessageID),
	}

	if p.RequireMention && message.Chat.IsGroup() && !p.addressed(text) {
		return v1.Envelope{}, false
	}

	// An "@name" prefix is how a group addresses the bot. It is presentation, so
	// it is removed before the command grammar and the prompt are read.
	text, _ = p.stripMention(text)

	if name, method, args, ok := parseCommand(text); ok {
		env.Kind = v1.EnvelopeCommand
		env.Command = &v1.IncomingCommand{
			Name:     name,
			Method:   method,
			Args:     args,
			ConfigID: ConfigIDs[name],
		}
		return env, true
	}

	env.Kind = v1.EnvelopeMessage
	env.Message = &v1.IncomingMessage{
		Text:        strings.TrimSpace(text),
		Attachments: attachments,
	}
	return env, true
}

// messageContent reads the text and attachments of a message event.
//
// Only text and image messages carry anything an agent can use. An image without
// a caption still carries the picture, so the text is a placeholder rather than
// empty: the core refuses a prompt with no text, and dropping the picture because
// nobody wrote a caption would be a silent loss.
func messageContent(update Update) (string, []v1.Attachment, bool) {
	message := update.Message

	switch update.EventName {
	case EventText:
		return message.Text, nil, true

	case EventImage:
		text := strings.TrimSpace(message.Caption)
		if message.Photo == "" {
			return text, nil, text != ""
		}
		if text == "" {
			text = "[image]"
		}
		return text, []v1.Attachment{{
			Name:     "photo" + extensionOf(message.Photo),
			MimeType: mimeOf(message.Photo),
			URL:      message.Photo,
		}}, true

	default:
		return "", nil, false
	}
}

// addressed reports whether a group message addresses the bot.
//
// Zalo has no mention markup in an inbound message, so addressing is a command
// in the first token, or the bot's name prefixed with "@".
func (p Parser) addressed(text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	if _, ok := Commands[strings.TrimPrefix(fields[0], "/")]; ok {
		return true
	}
	if !strings.HasPrefix(fields[0], "@") {
		return false
	}
	if p.BotName == "" {
		return true
	}
	mention := strings.ToLower(strings.TrimPrefix(fields[0], "@"))
	name := strings.ToLower(strings.TrimSpace(p.BotName))
	return mention == name || strings.HasPrefix(name, mention)
}

// stripMention removes an "@name" prefix and reports whether one was there.
func (p Parser) stripMention(text string) (string, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "@") {
		return text, false
	}

	mention := strings.ToLower(strings.TrimPrefix(fields[0], "@"))
	name := strings.ToLower(strings.TrimSpace(p.BotName))
	if name != "" && mention != name && !strings.HasPrefix(name, mention) {
		return text, false
	}
	return strings.Join(fields[1:], " "), true
}

// parseCommand recognizes a command in the first token and resolves it.
//
// Both `/new_chat` and a bare `new_chat` are accepted. Anything else is ordinary
// text. A recognized but unknown command returns ok=true with an empty method, so
// the router reports a command error instead of forwarding the text as a prompt.
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
		return name, "", fields[1:], true
	}
	return name, resolved, fields[1:], true
}

func isKnownCommand(name string) bool {
	_, ok := Commands[name]
	return ok
}

// sourceID is the platform's stable identifier for a delivery.
func sourceID(chatID, messageID string) string {
	if chatID == "" || messageID == "" {
		return ""
	}
	return fmt.Sprintf("event:%s:%s", chatID, messageID)
}

// mimeOf guesses an image's media type from its URL.
//
// Zalo does not send a content type with a message, and an image the core cannot
// type is an image the agent is not told about.
func mimeOf(url string) string {
	switch strings.ToLower(extensionOf(url)) {
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	default:
		return "image/jpeg"
	}
}

// extensionOf is the URL's file extension, including the dot, if it has one.
func extensionOf(url string) string {
	if i := strings.IndexAny(url, "?#"); i >= 0 {
		url = url[:i]
	}
	if i := strings.LastIndex(url, "."); i >= 0 && !strings.Contains(url[i:], "/") {
		return url[i:]
	}
	return ""
}

// permissionDecision reads a text reply as a decision on a pending request.
//
// Zalo has no buttons, so a permission is answered in words. The grammar is
// deliberately narrow — one word, or the number of an option — so a sentence that
// happens to contain "allow" is still a prompt rather than an accidental
// authorization.
func permissionDecision(text string, pending *permissionRequest) (optionID string, approved bool, ok bool) {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	if len(fields) != 1 {
		return "", false, false
	}

	switch fields[0] {
	case "allow", "approve", "yes":
		return firstOption(pending, allows), true, true
	case "deny", "reject", "no":
		return firstOption(pending, rejects), false, true
	}

	// A numbered reply selects one of the agent's own options.
	n, err := strconv.Atoi(fields[0])
	if err != nil || n < 1 || n > len(pending.Options) {
		return "", false, false
	}
	option := pending.Options[n-1]
	return option.OptionID, allows(option.Kind), true
}

// firstOption is the first option matching a predicate, or empty.
func firstOption(pending *permissionRequest, match func(string) bool) string {
	for _, option := range pending.Options {
		if match(option.Kind) {
			return option.OptionID
		}
	}
	return ""
}

// allows and rejects read an option's kind.
//
// The vocabulary belongs to the agent, so these read the prefix rather than an
// exact list: an agent that offers "allow_once" and "allow_always" offers two
// approvals, and one that offers "reject_once" offers a refusal.
func allows(kind string) bool { return strings.HasPrefix(kind, "allow") }

func rejects(kind string) bool {
	return strings.HasPrefix(kind, "deny") || strings.HasPrefix(kind, "reject")
}
