package slack

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	slackgo "github.com/slack-go/slack"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// Action IDs for interactive controls.
//
// The value carried by a button is the transport's own correlation data. Hive
// performs the domain transition; the transport only renders the result.
const (
	ActionPermissionAllow = "permission_allow"
	ActionPermissionDeny  = "permission_deny"

	// ActionPermissionRespond is one of the agent's own choices.
	//
	// Each button carries this plus its position — "permission_respond.2" — because
	// Slack rejects a message whose elements in one block share an action id
	// ("action_id ... already exists"), and rejecting the message means a
	// permission card that never appears. Which choice a button stands for is in
	// its value, so the suffix is only there to keep the ids apart.
	ActionPermissionRespond = "permission_respond"
)

// Renderer turns Hive events into Slack messages.
type Renderer struct {
	// SessionID scopes rendering to the session bound to this conversation.
	SessionID string
}

// Rendered is a message to post or replace.
type Rendered struct {
	Message Message

	// ToolCall is the tool call this event carried, when it carried one. A turn's
	// calls share one message, so the transport accumulates them rather than
	// posting each.
	ToolCall *v1.ToolCall

	// ToolRunID is the turn the tool call belongs to, so the calls of one turn
	// land in one message. It is empty for a call the core did not attribute to a
	// run, and the call's own id is used instead.
	ToolRunID string

	// Detail is posted as a reply in the message's thread, so a long tool output
	// does not bury the conversation.
	Detail string
}

// RenderEvent renders one Hive event, reporting false when it has no Slack
// representation.
//
// Agent streaming updates are deliberately not rendered: Hive preserves them as
// raw protocol data, and turning every protocol notification into a Slack message
// would be noise.
func (r Renderer) RenderEvent(ev v1.Event) (Rendered, bool) {
	if r.SessionID != "" && ev.SessionID != r.SessionID {
		return Rendered{}, false
	}

	switch ev.Type {
	case v1.EventMessage:
		return Rendered{Message: textMessage(renderText(ev))}, true

	case v1.EventRunStarted:
		return Rendered{Message: textMessage(fmt.Sprintf("Run started (%s)", runLabel(ev)))}, true

	case v1.EventRunFinished:
		return Rendered{Message: textMessage(fmt.Sprintf("Run finished (%s)", runLabel(ev)))}, true

	case v1.EventError:
		return Rendered{Message: textMessage(fmt.Sprintf("Error: %s", renderText(ev)))}, true

	case v1.EventPermissionRequested:
		return Rendered{Message: permissionMessage(ev)}, true

	case v1.EventPermissionResponded:
		return Rendered{Message: textMessage("Permission resolved.")}, true

	case v1.EventStatus:
		return Rendered{Message: textMessage(renderText(ev))}, true

	case v1.EventTool:
		return renderTool(ev)

	default:
		// agent.raw, usage, and anything unrecognized stay out of the
		// conversation. A usage report is answered by the status command rather
		// than announced: one line per turn is noise in a busy conversation.
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
		// Nothing was running. That is not a failure, and a warning would read like
		// one for an operation that had nothing to do.
		return textMessage(":information_source: nothing is running")
	case outcome.Error.Code == v1.CodeConflict && outcome.Method == v1.MethodSessionConfig:
		// The agent session lives in the agent process, so it does not survive a
		// restart. This is advice, not a failure.
		return textMessage(":information_source: " + outcome.Error.Message)
	case outcome.Error.Code == v1.CodeRetryable:
		// The operation is already in flight. A retry is not a failure, and a
		// warning would read like one.
		return textMessage("Already working on it.")
	default:
		return textMessage(fmt.Sprintf(":warning: %s", outcome.Error.Message))
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
			// The turn in front of it is still running, so this one waits. Saying
			// so is what stops a user sending it again.
			return textMessage(":hourglass_flowing_sand: Queued — it runs when the current turn finishes.")
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
		// A user must not see a Hive method name. The operation succeeded, so say
		// that without leaking the protocol into the conversation.
		return textMessage("Done.")
	}
}

// renderTool reads a normalized tool call out of an event.
//
// The call is reported rather than rendered as a message: a turn's calls share
// one message, and only the transport knows the whole turn, so it assembles the
// list.
func renderTool(ev v1.Event) (Rendered, bool) {
	var call v1.ToolCall
	if err := json.Unmarshal(ev.Payload, &call); err != nil || call.ToolCallID == "" {
		return Rendered{}, false
	}

	rendered := Rendered{
		Message:   textMessage(toolLine(call)),
		ToolCall:  &call,
		ToolRunID: ev.RunID,
	}
	if call.Status == v1.ToolFailed && call.Summary != "" {
		rendered.Detail = call.Summary
	}
	return rendered, true
}

// toolRunMessage renders a turn's tool calls as one list.
//
// One message per turn rather than one per call: a turn can make many calls and
// each call produces many updates, so one message per call buries the
// conversation. Slack collapses a long message itself, so the list can be whole.
func toolRunMessage(calls []v1.ToolCall) Message {
	var text strings.Builder
	fmt.Fprintf(&text, ":toolbox: %s", toolCount(len(calls)))
	for _, call := range calls {
		text.WriteString("\n")
		text.WriteString(toolLine(call))
	}
	return textMessage(text.String())
}

// toolCount is the heading for a turn's tool calls.
func toolCount(n int) string {
	if n == 1 {
		return "1 tool call"
	}
	return fmt.Sprintf("%d tool calls", n)
}

// toolLine is one tool call as a line: its state, then what it did.
func toolLine(call v1.ToolCall) string {
	return fmt.Sprintf("%s %s", toolIcon(call.Status), toolLabel(call))
}

// toolIcon is the state a tool call is in.
func toolIcon(status string) string {
	switch status {
	case v1.ToolCompleted:
		return ":white_check_mark:"
	case v1.ToolFailed:
		return ":x:"
	case v1.ToolInProgress:
		return ":hourglass_flowing_sand:"
	default:
		return ":wrench:"
	}
}

// toolLabel is what the agent calls the operation.
func toolLabel(call v1.ToolCall) string {
	for _, label := range []string{call.Title, call.Kind} {
		if label != "" {
			return label
		}
	}
	return "tool"
}

// usageParts is what a usage report says, in words.
//
// It is read by the status command rather than announced per turn: a reader who
// asks what a conversation costs gets the numbers, and a turn that only says
// what it cost is noise.
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

func permissionMessage(ev v1.Event) Message {
	var payload struct {
		ToolCall struct {
			Title string `json:"title"`
		} `json:"toolCall"`
		Options []permissionOption `json:"options"`
	}
	_ = json.Unmarshal(ev.Payload, &payload)

	title := payload.ToolCall.Title
	if title == "" {
		title = "a sensitive operation"
	}

	blocks := []slackgo.Block{
		slackgo.NewSectionBlock(
			slackgo.NewTextBlockObject(slackgo.MarkdownType,
				fmt.Sprintf("*Permission requested*\n%s", title), false, false), nil, nil),
	}
	if actions := permissionActions(ev, payload.Options); len(actions) > 0 {
		blocks = append(blocks, slackgo.NewActionBlock("", actions...))
	}

	return Message{
		Text:   fmt.Sprintf("Permission requested: %s", title),
		Blocks: blocks,
	}
}

// permissionOption is one choice the agent offered.
type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// permissionActions renders one button per choice the agent offered.
//
// The agent decides what a user may choose. An agent that offers "Allow once"
// and "Allow always" offers two buttons, and the transport renders what it was
// given rather than deciding that a decision has two answers.
func permissionActions(ev v1.Event, options []permissionOption) []slackgo.BlockElement {
	elements := make([]slackgo.BlockElement, 0, len(options))
	for i, option := range options {
		elements = append(elements, permissionButton(
			fmt.Sprintf("%s.%d", ActionPermissionRespond, i),
			optionLabel(option),
			encodeOption(ev, option),
			optionStyle(option.Kind),
		))
	}
	if len(elements) > 0 {
		return elements
	}

	// An agent that offered no choices still needs an answer, or it waits
	// forever. The pair a permission request has always had is the fallback.
	return []slackgo.BlockElement{
		permissionButton(ActionPermissionAllow, "Allow", encodeDecision(ev, true), slackgo.StylePrimary),
		permissionButton(ActionPermissionDeny, "Deny", encodeDecision(ev, false), slackgo.StyleDanger),
	}
}

// permissionButton builds one button of a permission card.
func permissionButton(actionID, label, value string, style slackgo.Style) slackgo.BlockElement {
	button := slackgo.NewButtonBlockElement(actionID, value,
		slackgo.NewTextBlockObject(slackgo.PlainTextType, label, false, false))
	if style != "" {
		button = button.WithStyle(style)
	}
	return button
}

// optionLabel is what a button says.
func optionLabel(option permissionOption) string {
	for _, label := range []string{option.Name, option.Kind} {
		if label != "" {
			return label
		}
	}
	return "Respond"
}

// optionStyle styles a button by the kind the agent gave it. A kind the
// transport does not know is neither safe nor dangerous, so it is left plain.
func optionStyle(kind string) slackgo.Style {
	switch kind {
	case "allow", "allow_once", "allow_always":
		return slackgo.StylePrimary
	case "deny", "reject_once", "reject_always":
		return slackgo.StyleDanger
	default:
		return ""
	}
}

// encodeOption builds the button payload for one of the agent's own choices.
//
// It carries the agent's request id, the option the user picked, and the label
// the card showed, so the card can say what was chosen once the buttons are
// gone.
func encodeOption(ev v1.Event, option permissionOption) string {
	return encodeValue(ev, map[string]any{
		"optionId": option.OptionID,
		"approved": option.Kind == "allow",
		"label":    optionLabel(option),
	})
}

// encodeDecision builds the button payload for a plain allow or deny, which is
// what a card posted before the agent's own options were rendered carries.
func encodeDecision(ev v1.Event, approved bool) string {
	return encodeValue(ev, map[string]any{"approved": approved})
}

// encodeValue builds a button payload around the agent's request id.
//
// It carries the agent's request id and the decision, which is all the router
// needs to relay it.
func encodeValue(ev v1.Event, fields map[string]any) string {
	var payload struct {
		AgentRequestID string `json:"agentRequestId"`
	}
	_ = json.Unmarshal(ev.Payload, &payload)

	fields["agentRequestId"] = payload.AgentRequestID
	value, err := json.Marshal(fields)
	if err != nil {
		return ""
	}
	return string(value)
}

// textMessage renders text a user reads.
//
// The text is converted to mrkdwn and split across as many blocks as it needs:
// Slack refuses a message whose section is over its limit, and refuses the whole
// message, so a long answer would otherwise never arrive at all.
func textMessage(text string) Message {
	converted := mrkdwn(text)
	return Message{
		Text:   fallbackText(converted),
		Blocks: blocksFor(converted),
	}
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
				mark = "•  :white_check_mark:"
			}
			text += fmt.Sprintf("\n%s `%s` — %s", mark, value.Value, value.Name)
		}

		if len(option.Options) > 0 {
			text += fmt.Sprintf("\nSwitch with: `@Hive %s <name>`", option.ID)
		}
	}
	return textMessage(text)
}

// noSettingsText explains a declaration that is empty.
//
// "No settings" on its own reads like a Hive failure and sends the reader looking
// for a bug. It is not one: Hive renders the settings an agent declares for a
// session, and this agent declared none. An agent whose model picker is exposed
// only through the legacy ACP `models`/`modes` fields declares nothing here,
// because Hive reads `configOptions`, the surface that supersedes them.
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

	// The statistics a reader asks a status for: which selectors the session
	// runs with, what it has done, and what the last turn cost.
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

// traceMessage renders recent activity so a user can see what happened.
//
// A conversation that shows only the final answer is a black box: this is what
// makes the steps visible, including the ones that produced nothing.
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

	return textMessage("*Recent activity*\n```\n" + strings.Join(lines, "\n") + "\n```")
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
		// The raw stream is noise in a conversation, so it is counted rather than
		// shown.
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
	text += "\n\nAddress me first: `@Hive <command>`. Anything else is a prompt."
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

func runLabel(ev v1.Event) string {
	if ev.RunID == "" {
		return ev.Type
	}
	return ev.RunID
}
