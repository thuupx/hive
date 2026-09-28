package zalo

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// Renderer turns Hive events into Zalo messages.
type Renderer struct {
	// SessionID scopes rendering to the session bound to this conversation.
	SessionID string
}

// Rendered is a message to post.
type Rendered struct {
	Message Message

	// Permission is the request this event carried, when it carried one. Zalo
	// has no buttons, so the transport remembers it and answers it in words.
	Permission *permissionRequest
}

// permissionRequest is a permission an agent is waiting on.
type permissionRequest struct {
	AgentRequestID string `json:"agentRequestId"`
	ToolCall       struct {
		Title string `json:"title"`
	} `json:"toolCall"`
	Options []permissionOption `json:"options"`
}

// Title is what the request is about, in words.
func (r permissionRequest) label() string {
	if r.ToolCall.Title != "" {
		return r.ToolCall.Title
	}
	return "a sensitive operation"
}

// permissionOption is one choice the agent offered.
type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// RenderEvent renders one Hive event, reporting false when it has no Zalo
// representation.
//
// Tool calls, run boundaries, usage, and the agent's raw stream are deliberately
// not rendered: a chat is not a log, and the transport shows the answer and the
// problems rather than narrating every step.
func (r Renderer) RenderEvent(ev v1.Event) (Rendered, bool) {
	if r.SessionID != "" && ev.SessionID != r.SessionID {
		return Rendered{}, false
	}

	switch ev.Type {
	case v1.EventMessage:
		// The pictures come out before markdown(), which would otherwise read
		// an image's link as a plain link and leave a stray "!".
		text, images := extractImages(renderText(ev))
		message := textMessage(text)
		message.Images = images
		return Rendered{Message: message}, true

	case v1.EventError:
		return Rendered{Message: textMessage("⚠️ " + renderText(ev))}, true

	case v1.EventPermissionRequested:
		request, ok := parsePermission(ev)
		if !ok {
			return Rendered{}, false
		}
		return Rendered{Message: permissionMessage(request), Permission: &request}, true

	case v1.EventPermissionResponded:
		return Rendered{Message: textMessage("Permission resolved.")}, true

	default:
		return Rendered{}, false
	}
}

// RenderOutcome renders the immediate result of an inbound delivery.
//
// A command can complete synchronously or asynchronously, so the transport must
// not assume the request lifetime equals the operation lifetime. This renders the
// acknowledgement; later state arrives as events.
func RenderOutcome(outcome v1.TransportOutcome) Message {
	switch {
	case outcome.Error == nil:
	case outcome.Error.Code == v1.CodeConflict && outcome.Method == v1.MethodSessionCancel:
		return textMessage("ℹ️ nothing is running")
	case outcome.Error.Code == v1.CodeConflict && outcome.Method == v1.MethodSessionConfig:
		return textMessage("ℹ️ " + outcome.Error.Message)
	case outcome.Error.Code == v1.CodeRetryable:
		return textMessage("Already working on it.")
	default:
		return textMessage("⚠️ " + outcome.Error.Message)
	}

	switch outcome.Method {
	case v1.MethodSessionCreate:
		var created v1.SessionCreateResult
		if err := json.Unmarshal(outcome.Result, &created); err == nil && created.SessionID != "" {
			return textMessage(fmt.Sprintf(
				"New chat created.\nSession: `%s`\nAgent: `%s`", created.SessionID, created.AgentID))
		}
		return textMessage("New chat created.")

	case v1.MethodSessionPrompt:
		var accepted v1.SessionPromptResult
		if err := json.Unmarshal(outcome.Result, &accepted); err == nil && accepted.Queued {
			return textMessage("⏳ Queued — it runs when the current turn finishes.")
		}
		return textMessage("Working on it.")

	case v1.MethodSessionCancel:
		return textMessage("Cancelled.")

	case v1.MethodSessionHandoff:
		var handed v1.SessionHandoffResult
		if err := json.Unmarshal(outcome.Result, &handed); err == nil && handed.AgentID != "" {
			return textMessage(fmt.Sprintf(
				"Handed off.\nSession: `%s`\nAgent: `%s`\nTarget run: `%s`",
				handed.SessionID, handed.AgentID, handed.TargetRunID))
		}
		return textMessage("Handed off.")

	case v1.MethodSessionConfig:
		var config v1.SessionConfigResult
		if err := json.Unmarshal(outcome.Result, &config); err == nil {
			return configMessage(config)
		}
		return textMessage("No agent settings are available.")

	case v1.MethodSessionStatus:
		var status v1.SessionStatusResult
		if err := json.Unmarshal(outcome.Result, &status); err == nil && status.SessionID != "" {
			return statusMessage(&status)
		}
		return textMessage("Status unavailable.")

	case v1.MethodSessionEvents:
		var replay v1.EventReplayResult
		if err := json.Unmarshal(outcome.Result, &replay); err == nil {
			return traceMessage(&replay)
		}
		return textMessage("No activity.")

	case v1.MethodSessionList:
		var list v1.SessionListResult
		if err := json.Unmarshal(outcome.Result, &list); err == nil {
			return sessionListMessage(&list)
		}
		return textMessage("No sessions.")

	case v1.MethodAgentList:
		var list v1.AgentListResult
		if err := json.Unmarshal(outcome.Result, &list); err == nil {
			return agentListMessage(&list)
		}
		return textMessage("No agents.")

	case v1.MethodNodeList:
		var list v1.NodeListResult
		if err := json.Unmarshal(outcome.Result, &list); err == nil {
			return nodeListMessage(&list)
		}
		return textMessage("No nodes.")

	default:
		return textMessage("Done.")
	}
}

// parsePermission reads a permission request out of an event.
func parsePermission(ev v1.Event) (permissionRequest, bool) {
	var request permissionRequest
	if err := json.Unmarshal(ev.Payload, &request); err != nil {
		return permissionRequest{}, false
	}
	if request.AgentRequestID == "" {
		return permissionRequest{}, false
	}
	return request, true
}

// permissionMessage renders a permission request as a message a user can answer.
//
// There are no buttons on Zalo, so the card says what to type. The agent decides
// what a user may choose; the transport renders what it was given rather than
// deciding that a decision has two answers.
func permissionMessage(request permissionRequest) Message {
	var text strings.Builder
	fmt.Fprintf(&text, "🔐 Permission requested\n%s\n", request.label())

	if len(request.Options) > 0 {
		text.WriteString("\nReply with the number:\n")
		for i, option := range request.Options {
			fmt.Fprintf(&text, "%d. %s\n", i+1, optionLabel(option))
		}
		text.WriteString("\nOr reply `allow` / `deny`.")
	} else {
		text.WriteString("\nReply `allow` to approve or `deny` to refuse.")
	}
	return textMessage(text.String())
}

// optionLabel is what a choice is called.
func optionLabel(option permissionOption) string {
	for _, label := range []string{option.Name, option.Kind} {
		if label != "" {
			return label
		}
	}
	return "Respond"
}

// textMessage renders text a user reads, as formatted markdown.
func textMessage(text string) Message {
	return Message{Text: markdown(text), ParseMode: "markdown"}
}

// plainMessage renders text with no formatting.
//
// It is for output whose own punctuation is the point: a trace full of backticks
// and asterisks would be read as markup and arrive mangled.
func plainMessage(text string) Message {
	return Message{Text: text}
}

// markdown normalizes what an agent writes into what Zalo renders.
//
// Zalo's markdown already understands bold, italic, headings, lists, and inline
// code, so most of an answer is left exactly as it is. Two things are adjusted:
// a link, which Zalo's markup does not describe, becomes "text (url)", and a
// fence an agent wrapped its whole answer in is unwrapped so the prose is not
// shown as source.
func markdown(text string) string {
	text = unwrapProseFence(text)
	return linkPattern.ReplaceAllString(text, "$1 ($2)")
}

// unwrapProseFence removes a fence an agent wrapped its whole answer in.
//
// Agents often label an answer ````markdown` and fence the lot. A fence labelled
// markdown, md, text, or nothing is unwrapped, and a fence labelled with a real
// language is left alone because it is code.
func unwrapProseFence(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "```") {
		return text
	}

	lines := strings.Split(trimmed, "\n")
	if len(lines) < 3 {
		return text
	}

	open, language, ok := fence(strings.TrimSpace(lines[0]))
	if !ok {
		return text
	}
	closing, rest, ok := fence(strings.TrimSpace(lines[len(lines)-1]))
	if !ok || rest != "" || closing < open {
		return text
	}

	switch strings.ToLower(language) {
	case "", "markdown", "md", "text", "txt":
	default:
		return text
	}
	return strings.Join(lines[1:len(lines)-1], "\n")
}

// fence reads a code fence, returning its length, its language, and the rest of
// the line after it.
func fence(line string) (int, string, bool) {
	length := 0
	for length < len(line) && line[length] == '`' {
		length++
	}
	if length < 3 {
		return 0, "", false
	}
	return length, strings.TrimSpace(line[length:]), true
}

// linkPattern matches a markdown link.
var linkPattern = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)

// imagePattern matches a markdown image: ![alt](target).
var imagePattern = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)\)`)

// extractImages pulls the markdown images out of a text, returning what is left
// and the pictures found.
//
// A target that is neither an http(s) URL nor a file path cannot become a
// photo — a data: URI, say — so it stays in the text untouched rather than
// disappearing.
func extractImages(text string) (string, []OutboundImage) {
	var images []OutboundImage
	clean := imagePattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := imagePattern.FindStringSubmatch(match)
		if !isPhotoTarget(parts[2]) {
			return match
		}
		images = append(images, OutboundImage{Alt: parts[1], Target: parts[2]})
		return ""
	})
	return strings.TrimSpace(clean), images
}

// isPhotoTarget reports whether an image target can reach the platform: a URL
// it fetches, or a file path the transport reads and uploads.
func isPhotoTarget(target string) bool {
	switch {
	case strings.HasPrefix(target, "http://"), strings.HasPrefix(target, "https://"):
		return true
	case strings.Contains(target, ":"):
		// Another scheme cannot become a photo.
		return false
	default:
		return true
	}
}

// chunk splits text so every piece fits in one Zalo message.
//
// Zalo refuses a text over its limit, and refuses the whole message, so a long
// answer would otherwise never arrive. The text is split on line boundaries where
// it can be, and a single line longer than the limit is split on the limit.
func chunk(text string, limit int) []string {
	if limit <= 0 {
		limit = ChunkChars
	}
	if len(text) <= limit {
		return []string{text}
	}

	var chunks []string
	var current strings.Builder

	flush := func() {
		if current.Len() == 0 {
			return
		}
		chunks = append(chunks, strings.TrimRight(current.String(), "\n"))
		current.Reset()
	}

	for _, line := range strings.Split(text, "\n") {
		for len(line) > limit {
			flush()
			chunks = append(chunks, line[:limit])
			line = line[limit:]
		}
		if current.Len()+len(line)+1 > limit {
			flush()
		}
		if current.Len() > 0 {
			current.WriteString("\n")
		}
		current.WriteString(line)
	}
	flush()

	if len(chunks) == 0 {
		return []string{text}
	}
	return chunks
}

// configMessage renders the selectors an agent declared.
//
// The agent decides what it offers, so this renders whatever it sent rather than
// knowing what a model is.
func configMessage(config v1.SessionConfigResult) Message {
	if len(config.Options) == 0 {
		return textMessage(noSettingsText(config.AgentID))
	}

	text := fmt.Sprintf("*%s settings*", config.AgentID)
	for _, option := range config.Options {
		current := strings.Trim(string(option.CurrentValue), `"`)

		text += fmt.Sprintf("\n\n*%s*", option.Name)
		if current != "" {
			text += fmt.Sprintf(" — now `%s`", current)
		}

		for _, value := range option.Options {
			mark := "•"
			if value.Value == current {
				mark = "• ✅"
			}
			text += fmt.Sprintf("\n%s `%s` — %s", mark, value.Value, value.Name)
		}

		if len(option.Options) > 0 {
			text += fmt.Sprintf("\nSwitch with: `model %s <name>`", option.ID)
		}
	}
	return textMessage(text)
}

// noSettingsText explains a declaration that is empty.
func noSettingsText(agentID string) string {
	who := "This agent"
	if agentID != "" {
		who = "*" + agentID + "*"
	}
	return fmt.Sprintf(
		"%s declares no session settings over ACP.\n\n"+
			"Hive renders the settings an agent declares for a session. An agent that "+
			"exposes its model picker only through the legacy ACP `models`/`modes` fields "+
			"declares nothing here: Hive reads `configOptions`, the surface that supersedes "+
			"them.\n\n"+
			"Change the model with the agent's own command, or hand off to an agent that "+
			"declares settings.",
		who)
}

func statusMessage(status *v1.SessionStatusResult) Message {
	text := fmt.Sprintf("*Session* `%s`\nState: `%s`", status.SessionID, status.State)
	for _, run := range status.Runs {
		text += fmt.Sprintf("\n• `%s` %s on `%s` — %s", run.RunID, run.AgentID, run.NodeID, run.State)
	}

	if settings := settingParts(status.Settings); len(settings) > 0 {
		text += "\n\n*Settings*: " + strings.Join(settings, " · ")
	}
	if status.ToolCalls > 0 {
		text += fmt.Sprintf("\n*Tool calls*: %d", status.ToolCalls)
	}
	if status.Usage != nil {
		if parts := usageParts(*status.Usage); len(parts) > 0 {
			text += "\n*Tokens*: " + strings.Join(parts, " · ")
		}
	}
	return textMessage(text)
}

// settingParts renders the selectors a session recorded, in a stable order.
func settingParts(settings map[string]string) []string {
	ids := make([]string, 0, len(settings))
	for id := range settings {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%s `%s`", id, settings[id]))
	}
	return parts
}

// usageParts is what a usage report says, in words.
func usageParts(usage v1.Usage) []string {
	parts := make([]string, 0, 3)
	if turn := usage.Turn(); turn > 0 {
		parts = append(parts, fmt.Sprintf("*%s tokens*", commas(turn)))
	}
	if usage.InputTokens > 0 || usage.OutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("in %s · out %s",
			commas(usage.InputTokens), commas(usage.OutputTokens)))
	}
	if usage.ContextSize > 0 {
		parts = append(parts, fmt.Sprintf("context %d%% (%s/%s)",
			usage.ContextUsed*100/usage.ContextSize,
			commas(usage.ContextUsed), commas(usage.ContextSize)))
	}
	return parts
}

// commas groups a number in threes, because a token count is read at a glance.
func commas(n int) string {
	digits := strconv.Itoa(n)
	if len(digits) <= 3 {
		return digits
	}

	var out strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(digit)
	}
	return out.String()
}

// traceMessage renders recent activity so a user can see what happened.
func traceMessage(replay *v1.EventReplayResult) Message {
	if replay.Gap != nil {
		return textMessage(fmt.Sprintf(
			"The history was pruned; the trace starts at %d.", replay.Gap.NextSequence))
	}
	if len(replay.Events) == 0 {
		return textMessage("No activity yet.")
	}

	lines := make([]string, 0, len(replay.Events))
	for _, ev := range replay.Events {
		line := fmt.Sprintf("%d  %s", ev.Sequence, ev.Type)
		if summary := traceSummary(ev); summary != "" {
			line += "  " + summary
		}
		lines = append(lines, line)
	}
	return plainMessage("*Recent activity*\n```\n" + strings.Join(lines, "\n") + "\n```")
}

// traceSummary is the readable part of an event.
func traceSummary(ev v1.Event) string {
	switch ev.Type {
	case v1.EventTool:
		var call v1.ToolCall
		if err := json.Unmarshal(ev.Payload, &call); err != nil {
			return ""
		}
		return fmt.Sprintf("%s [%s] %s", call.Status, call.Kind, call.Title)

	case v1.EventMessage:
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			return ""
		}
		text := strings.Join(strings.Fields(payload.Text), " ")
		if len(text) > 80 {
			text = text[:80] + "…"
		}
		return text

	case v1.EventError:
		var payload struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			return ""
		}
		return payload.Message

	case v1.EventPermissionRequested, v1.EventPermissionResponded:
		return ""

	default:
		if ev.Type == v1.EventAgentRaw {
			return "(agent stream)"
		}
		return ""
	}
}

// sessionListMessage lists the conversations a user can continue.
func sessionListMessage(list *v1.SessionListResult) Message {
	if len(list.Sessions) == 0 {
		return textMessage("No sessions yet. Send a message to start one.")
	}

	text := "*Sessions*"
	for _, session := range list.Sessions {
		text += fmt.Sprintf("\n• `%s` — %s, %d run(s)", session.SessionID, session.State, session.Runs)
	}
	return textMessage(text)
}

// HelpMessage renders the transport's own catalog.
//
// The catalog belongs to the transport, so this needs no round trip to the core.
func HelpMessage() Message {
	text := "*Commands*"
	for _, entry := range Catalog() {
		text += fmt.Sprintf("\n• `%s` — %s", entry.Command, entry.Description)
	}
	text += "\n\nAnything else is a prompt.\n" +
		"When an agent asks permission, reply `allow`, `deny`, or the number of a choice."
	return textMessage(text)
}

func agentListMessage(list *v1.AgentListResult) Message {
	text := "*Agents*"
	for _, agent := range list.Agents {
		text += fmt.Sprintf("\n• `%s` (%s)", agent.ID, agent.Protocol)
	}
	return textMessage(text)
}

func nodeListMessage(list *v1.NodeListResult) Message {
	if len(list.Nodes) == 0 {
		return textMessage("No nodes.")
	}

	text := "*Nodes*"
	for _, node := range list.Nodes {
		state := "offline"
		if node.Connected {
			state = "connected"
		}
		text += fmt.Sprintf("\n• `%s` %s — %d executions", node.NodeID, state, node.Executions)
	}
	return textMessage(text)
}

// renderText extracts a human-readable string from an event payload without
// interpreting the protocol it came from.
func renderText(ev v1.Event) string {
	var payload struct {
		Text    string `json:"text"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err == nil {
		switch {
		case payload.Text != "":
			return payload.Text
		case payload.Message != "":
			return payload.Message
		}
	}
	if len(ev.Payload) == 0 {
		return ev.Type
	}
	return string(ev.Payload)
}
