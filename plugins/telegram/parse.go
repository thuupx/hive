// Package telegram implements the Telegram Bot transport.
//
// The transport owns Telegram syntax: commands, mentions, permission answers,
// and text formatting. Hive owns operation semantics. Nothing
// Telegram-specific reaches the core, and this package must not import any
// internal Hive package except the transport boundary it normalizes into.
package telegram

import (
	"fmt"
	"mime"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// Name is the transport name. It is also the prefix of the principals this
// transport may assert.
const Name = "telegram"

// CommandMap maps a transport command name to a Hive method.
//
// The map is transport-owned: it decides which operations this transport
// exposes and what to call them. It lives with the transport rather than in
// the protocol because Hive core must not contain a Telegram command registry.
type CommandMap map[string]string

// Commands is the transport command catalog.
//
// Telegram intercepts a leading slash and parses it as a bot_command entity,
// so commands arrive as real structured input rather than text a transport
// must guess at.
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
	// transport answers it without asking the core. start is the message
	// Telegram opens a bot conversation with, so it answers the same way.
	"help":  "",
	"start": "",
}

// ConfigIDs maps a transport command to the agent selector it targets.
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
	"start":    "Show this list",
}

// HelpCommand is the transport command that shows the catalog, and
// HelpCommands are the aliases that show it.
const HelpCommand = "help"

func isHelp(name string) bool { return name == HelpCommand || name == "start" }

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

// Parser turns Telegram input into transport envelopes.
type Parser struct {
	// RequireMention makes the transport ignore a group message that does not
	// address the bot. A private chat is 1:1, so it is always addressed and this
	// does not apply to it.
	RequireMention bool

	// BotUsername is the bot's @name, used to recognize a mention or a
	// command's "@name" suffix. It is learned from getMe; the option exists
	// for the case the identity could not be fetched.
	BotUsername string

	// BotID is the bot's user id, so a reply to the bot is recognized. Learned
	// from getMe like the username.
	BotID int64
}

// ParseUpdate classifies one inbound delivery.
//
// Three shapes are handled: a message, which becomes a prompt or a command; a
// callback query, which becomes a permission answer; and anything else, which
// is not for Hive.
func (p Parser) ParseUpdate(update Update) (v1.Envelope, bool) {
	if update.CallbackQuery != nil {
		return p.parseCallback(update.CallbackQuery)
	}
	return p.parseMessage(update.Message, update.UpdateID)
}

// parseCallback turns a button press into an envelope.
//
// The press carries the choice in its data; the parser does not resolve it,
// because which pending request it answers is state, not syntax. The plugin
// resolves it.
func (p Parser) parseCallback(query *CallbackQuery) (v1.Envelope, bool) {
	if query.From.IsBot || query.From.ID == 0 || query.Message == nil || query.Data == "" {
		return v1.Envelope{}, false
	}
	return v1.Envelope{
		Transport:      Name,
		ConversationID: strconv.FormatInt(query.Message.Chat.ID, 10),
		Principal:      Name + ":" + strconv.FormatInt(query.From.ID, 10),
		SourceID:       fmt.Sprintf("event:%d:%s", query.Message.Chat.ID, query.ID),
		Kind:           v1.EnvelopeInteraction,
		Interaction: &v1.IncomingInteraction{
			Action: ActionPermissionAnswer,
			Method: v1.MethodPermissionRespond,
			Value:  query.Data,
		},
	}, true
}

// ActionPermissionAnswer is a button press answering a permission request.
const ActionPermissionAnswer = "permission_answer"

// parseMessage turns a message into an envelope.
func (p Parser) parseMessage(message *InboundMessage, updateID int64) (v1.Envelope, bool) {
	if message == nil {
		return v1.Envelope{}, false
	}
	// A bot's own message is not user intent, and a message with no sender
	// cannot be authorized.
	if message.From.IsBot || message.From.ID == 0 {
		return v1.Envelope{}, false
	}

	text, attachments, ok := messageContent(message)
	if !ok {
		// A sticker, a video note, or a message kind with nothing an agent can
		// act on is ignored rather than guessed at.
		return v1.Envelope{}, false
	}

	env := v1.Envelope{
		Transport:      Name,
		ConversationID: strconv.FormatInt(message.Chat.ID, 10),
		Principal:      Name + ":" + strconv.FormatInt(message.From.ID, 10),
		SourceID:       fmt.Sprintf("event:%d:%d", message.Chat.ID, updateID),
	}

	if p.RequireMention && message.Chat.IsGroup() && !p.addressed(message) {
		return v1.Envelope{}, false
	}

	text = p.stripMention(text, message)

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

// messageContent reads the text and attachments of a message.
//
// A caption is the text of a media message. An attachment with no caption
// still carries the file, so the text is a placeholder rather than empty: the
// core refuses a prompt with no text, and dropping the file because nobody
// wrote a caption would be a silent loss.
func messageContent(message *InboundMessage) (string, []v1.Attachment, bool) {
	text := strings.TrimSpace(message.Text)
	if text == "" {
		text = strings.TrimSpace(message.Caption)
	}

	var attachments []v1.Attachment
	switch {
	case len(message.Photo) > 0:
		// The last rendition is the largest.
		photo := message.Photo[len(message.Photo)-1]
		attachments = append(attachments, v1.Attachment{
			Name:     "photo.jpg",
			MimeType: "image/jpeg",
			URL:      photo.FileID,
		})
	case message.Document != nil:
		doc := message.Document
		name := doc.FileName
		if name == "" {
			name = "document"
		}
		mimeType := doc.MimeType
		if mimeType == "" {
			mimeType = mimeOfName(name)
		}
		attachments = append(attachments, v1.Attachment{
			Name:     name,
			MimeType: mimeType,
			URL:      doc.FileID,
		})
	case message.Voice != nil:
		mimeType := message.Voice.MimeType
		if mimeType == "" {
			mimeType = "audio/ogg"
		}
		attachments = append(attachments, v1.Attachment{
			Name:     "voice" + extensionOf(mimeType),
			MimeType: mimeType,
			URL:      message.Voice.FileID,
		})
	}

	if len(attachments) == 0 {
		return text, nil, text != ""
	}
	if text == "" {
		text = "[attachment]"
	}
	return text, attachments, true
}

// addressed reports whether a group message addresses the bot.
//
// Telegram parses mentions and commands itself, so addressing is structured:
// a bot_command in the first token, an @mention of the bot, or a reply to one
// of the bot's own messages.
func (p Parser) addressed(message *InboundMessage) bool {
	if p.isReplyToBot(message) {
		return true
	}

	text := message.Text
	entities := message.Entities
	if text == "" {
		text = message.Caption
		entities = message.CaptionEntities
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	if strings.HasPrefix(fields[0], "/") {
		return p.commandForUs(fields[0])
	}

	for _, entity := range entities {
		if entity.Type != EntityMention {
			continue
		}
		mention := entityText(text, entity)
		if p.isBotMention(mention) {
			return true
		}
	}
	// With no name to compare against, a first-token "@word" is accepted
	// rather than refused: a group that ignores every message because one
	// option was left empty is a worse failure than answering one that was
	// not addressed.
	if p.BotUsername == "" && strings.HasPrefix(fields[0], "@") {
		return true
	}
	return p.isBotMention(fields[0])
}

// commandForUs reports whether a "/name" or "/name@bot" token is a command
// this transport should run.
//
// The "@otherbot" suffix names a different bot, so a group command for it is
// not addressed here. With no learned username the suffix cannot be checked,
// and accepting beats silently ignoring a command.
func (p Parser) commandForUs(token string) bool {
	token = strings.TrimPrefix(token, "/")
	if i := strings.IndexByte(token, '@'); i >= 0 {
		target := token[i+1:]
		if p.BotUsername != "" && !strings.EqualFold(target, p.BotUsername) {
			return false
		}
		token = token[:i]
	}
	_, known := Commands[strings.ToLower(token)]
	return known
}

// isReplyToBot reports whether the message answers one of the bot's own.
func (p Parser) isReplyToBot(message *InboundMessage) bool {
	return message.ReplyTo != nil && p.BotID != 0 && message.ReplyTo.From.ID == p.BotID
}

// isBotMention reports whether a token is an @mention of the bot.
func (p Parser) isBotMention(token string) bool {
	if p.BotUsername == "" || !strings.HasPrefix(token, "@") {
		return false
	}
	return strings.EqualFold(strings.TrimPrefix(token, "@"), p.BotUsername)
}

// stripMention removes an "@botname" prefix when one is there.
func (p Parser) stripMention(text string, message *InboundMessage) string {
	fields := strings.Fields(text)
	if len(fields) == 0 || !p.isBotMention(fields[0]) {
		return text
	}
	return strings.Join(fields[1:], " ")
}

// parseCommand recognizes a command in the first token and resolves it.
//
// Telegram delivers commands with a bot_command entity and intercepts the
// slash itself, so only "/name" is recognized — a bare word is a prompt, not
// a command. The "/name@otherbot" suffix is recognized and stripped; a command
// addressed to a different bot is not this transport's.
func parseCommand(text string) (name, method string, args []string, ok bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", "", nil, false
	}

	first := fields[0]
	if !strings.HasPrefix(first, "/") {
		return "", "", nil, false
	}
	name = strings.TrimPrefix(first, "/")
	// The "@bot" suffix selects which bot a group command is for.
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	name = strings.ToLower(name)

	resolved, known := Commands[name]
	if !known {
		return name, "", fields[1:], true
	}
	return name, resolved, fields[1:], true
}

// entityText reads the text an entity covers.
//
// Telegram measures entities in UTF-16 code units, so the range is walked in
// runes rather than bytes.
func entityText(text string, entity Entity) string {
	runes := []rune(text)
	if entity.Offset < 0 || entity.Offset >= len(runes) {
		return ""
	}
	end := entity.Offset + entity.Length
	if end > len(runes) {
		end = len(runes)
	}
	return string(runes[entity.Offset:end])
}

// mimeOfName guesses a file's media type from its name.
func mimeOfName(name string) string {
	if ext := filepath.Ext(name); ext != "" {
		if mimeType := mime.TypeByExtension(ext); mimeType != "" {
			return mimeType
		}
	}
	return "application/octet-stream"
}

// extensionOf names a file extension for a media type, so a downloaded voice
// note keeps a playable name.
func extensionOf(mimeType string) string {
	switch {
	case strings.Contains(mimeType, "ogg"):
		return ".ogg"
	case strings.Contains(mimeType, "mpeg"), strings.Contains(mimeType, "mp3"):
		return ".mp3"
	case strings.Contains(mimeType, "mp4"):
		return ".mp4"
	default:
		return ".bin"
	}
}
