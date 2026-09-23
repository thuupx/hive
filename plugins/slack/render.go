package slack

import (
	"encoding/json"
	"fmt"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// Action IDs for interactive controls.
//
// The value carried by a button is the transport's own correlation data. Hive
// performs the domain transition; the transport only renders the result.
const (
	ActionPermissionAllow = "permission_allow"
	ActionPermissionDeny  = "permission_deny"
)

// Renderer turns Hive events into Slack messages.
type Renderer struct {
	// SessionID scopes rendering to the session bound to this conversation.
	SessionID string
}

// Rendered is a message to post.
type Rendered struct {
	Message Message
	// ThreadKey groups related messages. Empty posts to the conversation.
	ThreadKey string
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

	default:
		// agent.raw and anything unrecognized stay out of the conversation.
		return Rendered{}, false
	}
}

// RenderOutcome renders the immediate result of an inbound delivery.
//
// A command can complete synchronously or asynchronously, so the transport must
// not assume the request lifetime equals the operation lifetime. This renders the
// acknowledgement; later state arrives as events.
func RenderOutcome(outcome v1.TransportOutcome) Message {
	if outcome.Error != nil {
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

	case v1.MethodSessionStatus:
		var status v1.SessionStatusResult
		if err := json.Unmarshal(outcome.Result, &status); err == nil && status.SessionID != "" {
			return statusMessage(&status)
		}
		return textMessage("Status unavailable.")

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

func permissionMessage(ev v1.Event) Message {
	var payload struct {
		ToolCall struct {
			Title string `json:"title"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(ev.Payload, &payload)

	title := payload.ToolCall.Title
	if title == "" {
		title = "a sensitive operation"
	}

	// The value carries the agent's own request id, so Hive can correlate the
	// response without the transport understanding the request.
	allowValue := encodeValue(ev, true)
	denyValue := encodeValue(ev, false)

	return Message{
		Text: fmt.Sprintf("Permission requested: %s", title),
		Blocks: []Block{
			{Type: "section", Text: &TextObject{Type: "mrkdwn", Text: fmt.Sprintf("*Permission requested*\n%s", title)}},
			{
				Type: "actions",
				Elements: []Element{
					{
						Type:     "button",
						Text:     &TextObject{Type: "plain_text", Text: "Allow"},
						ActionID: ActionPermissionAllow,
						Value:    allowValue,
						Style:    "primary",
					},
					{
						Type:     "button",
						Text:     &TextObject{Type: "plain_text", Text: "Deny"},
						ActionID: ActionPermissionDeny,
						Value:    denyValue,
						Style:    "danger",
					},
				},
			},
		},
	}
}

// encodeValue builds the button payload for a permission decision.
//
// It carries the agent's request id and the Hive session, which is all the router
// needs to relay the decision.
func encodeValue(ev v1.Event, approved bool) string {
	var payload struct {
		AgentRequestID string `json:"agentRequestId"`
	}
	_ = json.Unmarshal(ev.Payload, &payload)

	value, err := json.Marshal(map[string]any{
		"agentRequestId": payload.AgentRequestID,
		"approved":       approved,
	})
	if err != nil {
		return ""
	}
	return string(value)
}

func textMessage(text string) Message {
	return Message{
		Text:   text,
		Blocks: []Block{{Type: "section", Text: &TextObject{Type: "mrkdwn", Text: text}}},
	}
}

func statusMessage(status *v1.SessionStatusResult) Message {
	text := fmt.Sprintf("*Session* `%s`\nState: `%s`", status.SessionID, status.State)
	for _, run := range status.Runs {
		text += fmt.Sprintf("\n• `%s` %s on `%s` — %s", run.RunID, run.AgentID, run.NodeID, run.State)
	}
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
