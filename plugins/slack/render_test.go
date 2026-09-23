package slack

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

func TestRenderPermissionRequestOffersButtons(t *testing.T) {
	ev := v1.Event{
		Type:      v1.EventPermissionRequested,
		SessionID: "sess_1",
		Payload: json.RawMessage(`{
			"agentRequestId": "7",
			"toolCall": {"title": "Write file"},
			"options": [
				{"optionId": "a1", "name": "Allow", "kind": "allow"},
				{"optionId": "d1", "name": "Deny", "kind": "deny"}
			]
		}`),
	}

	rendered, ok := Renderer{SessionID: "sess_1"}.RenderEvent(ev)
	if !ok {
		t.Fatal("a permission request must be rendered")
	}
	if !strings.Contains(rendered.Message.Text, "Write file") {
		t.Errorf("text = %q", rendered.Message.Text)
	}

	var actions *Block
	for i := range rendered.Message.Blocks {
		if rendered.Message.Blocks[i].Type == "actions" {
			actions = &rendered.Message.Blocks[i]
		}
	}
	if actions == nil {
		t.Fatal("the permission message has no actions block")
	}
	if len(actions.Elements) != 2 {
		t.Fatalf("elements = %d, want allow and deny", len(actions.Elements))
	}

	// The value must carry the agent's own request id, so Hive can correlate the
	// response without the transport understanding the request.
	var allowValue struct {
		AgentRequestID string `json:"agentRequestId"`
		Approved       bool   `json:"approved"`
	}
	if err := json.Unmarshal([]byte(actions.Elements[0].Value), &allowValue); err != nil {
		t.Fatalf("allow value is not valid json: %v", err)
	}
	if allowValue.AgentRequestID != "7" || !allowValue.Approved {
		t.Fatalf("allow value = %+v", allowValue)
	}

	var denyValue struct {
		Approved bool `json:"approved"`
	}
	if err := json.Unmarshal([]byte(actions.Elements[1].Value), &denyValue); err != nil {
		t.Fatalf("deny value is not valid json: %v", err)
	}
	if denyValue.Approved {
		t.Fatal("the deny button must not carry an approval")
	}
}

// Agent streaming updates are preserved as raw protocol data, and turning every
// one of them into a Slack message would be noise.
func TestRenderSkipsAgentRaw(t *testing.T) {
	ev := v1.Event{
		Type:      v1.EventAgentRaw,
		SessionID: "sess_1",
		Method:    "session/update",
		Payload:   json.RawMessage(`{"sessionUpdate":"agent_message_chunk"}`),
	}

	if _, ok := (Renderer{SessionID: "sess_1"}).RenderEvent(ev); ok {
		t.Fatal("agent.raw must not be rendered")
	}
}

func TestRenderScopesToTheSession(t *testing.T) {
	ev := v1.Event{Type: v1.EventMessage, SessionID: "sess_other", Payload: json.RawMessage(`{"text":"hi"}`)}

	if _, ok := (Renderer{SessionID: "sess_1"}).RenderEvent(ev); ok {
		t.Fatal("an event from another session must not be rendered")
	}
}

func TestRenderOutcomeCreate(t *testing.T) {
	result, err := json.Marshal(v1.SessionCreateResult{
		SessionID: "sess_1",
		RunID:     "run_1",
		AgentID:   "claude",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	message := RenderOutcome(v1.TransportOutcome{Method: v1.MethodSessionCreate, Result: result})
	if !strings.Contains(message.Text, "sess_1") || !strings.Contains(message.Text, "claude") {
		t.Fatalf("text = %q", message.Text)
	}
}

func TestRenderOutcomeError(t *testing.T) {
	message := RenderOutcome(v1.TransportOutcome{
		Error: v1.NewErrorf(v1.CodeMethodNotFound, "slack/teleport"),
	})
	if !strings.Contains(message.Text, "slack/teleport") {
		t.Fatalf("text = %q", message.Text)
	}
}

func TestRenderOutcomeStatus(t *testing.T) {
	result, err := json.Marshal(v1.SessionStatusResult{
		SessionID: "sess_1",
		State:     "active",
		Runs:      []v1.RunSummary{{RunID: "run_1", AgentID: "claude", NodeID: "node_a", State: "running"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	message := RenderOutcome(v1.TransportOutcome{Method: v1.MethodSessionStatus, Result: result})
	if !strings.Contains(message.Text, "run_1") || !strings.Contains(message.Text, "running") {
		t.Fatalf("text = %q", message.Text)
	}
}

// A user must not see a Hive method name in a conversation.
func TestRenderOutcomeNeverLeaksAMethodName(t *testing.T) {
	// Every method the transport can map, rendered with no result.
	for _, method := range []string{
		v1.MethodSessionCreate,
		v1.MethodSessionPrompt,
		v1.MethodSessionCancel,
		v1.MethodSessionStatus,
		v1.MethodSessionHandoff,
		v1.MethodAgentList,
		v1.MethodNodeList,
	} {
		message := RenderOutcome(v1.TransportOutcome{Method: method})
		if strings.Contains(message.Text, "session.") || strings.Contains(message.Text, "agent.") {
			t.Errorf("%s rendered %q, which leaks a Hive method name", method, message.Text)
		}
		if strings.TrimSpace(message.Text) == "" {
			t.Errorf("%s rendered nothing", method)
		}
	}
}

func TestRenderOutcomeHandoff(t *testing.T) {
	result, err := json.Marshal(v1.SessionHandoffResult{
		SessionID:   "sess_1",
		AgentID:     "devin",
		TargetRunID: "run_2",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	message := RenderOutcome(v1.TransportOutcome{Method: v1.MethodSessionHandoff, Result: result})
	for _, want := range []string{"Handed off", "sess_1", "devin", "run_2"} {
		if !strings.Contains(message.Text, want) {
			t.Errorf("text %q does not mention %q", message.Text, want)
		}
	}
}

// A retry of a command still in flight is not a failure, so it must not read like
// one.
func TestRenderOutcomeRetryableIsNotAWarning(t *testing.T) {
	message := RenderOutcome(v1.TransportOutcome{
		Error: v1.NewErrorf(v1.CodeRetryable, "session.prompt has not recorded its outcome yet"),
	})

	if strings.Contains(message.Text, ":warning:") {
		t.Fatalf("a retryable outcome rendered as a warning: %q", message.Text)
	}
	if !strings.Contains(message.Text, "Already working") {
		t.Fatalf("text = %q", message.Text)
	}
}
