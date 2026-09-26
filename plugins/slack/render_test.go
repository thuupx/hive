package slack

import (
	"encoding/json"
	"fmt"
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

// A user can see the conversations they can continue.
func TestRenderSessionList(t *testing.T) {
	result, err := json.Marshal(v1.SessionListResult{Sessions: []v1.SessionSummary{
		{SessionID: "sess_1", State: "active", Runs: 2},
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	message := RenderOutcome(v1.TransportOutcome{Method: v1.MethodSessionList, Result: result})
	if !strings.Contains(message.Text, "sess_1") || !strings.Contains(message.Text, "2 run") {
		t.Fatalf("text = %q", message.Text)
	}
}

func TestRenderSessionListWhenEmpty(t *testing.T) {
	result, err := json.Marshal(v1.SessionListResult{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	message := RenderOutcome(v1.TransportOutcome{Method: v1.MethodSessionList, Result: result})
	if !strings.Contains(message.Text, "No sessions") {
		t.Fatalf("text = %q", message.Text)
	}
}

// A trace shows what happened, including the steps that produced nothing.
func TestRenderTrace(t *testing.T) {
	result, err := json.Marshal(v1.EventReplayResult{Events: []v1.Event{
		{Sequence: 1, Type: v1.EventTool, Payload: json.RawMessage(
			`{"toolCallId":"tc1","title":"Listed ./","kind":"execute","status":"completed"}`)},
		{Sequence: 2, Type: v1.EventMessage, Payload: json.RawMessage(`{"text":"the answer"}`)},
		{Sequence: 3, Type: v1.EventAgentRaw, Payload: json.RawMessage(`{"sessionUpdate":"x"}`)},
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	message := RenderOutcome(v1.TransportOutcome{Method: v1.MethodSessionEvents, Result: result})
	for _, want := range []string{"1", "tool", "Listed ./", "the answer", "(agent stream)"} {
		if !strings.Contains(message.Text, want) {
			t.Errorf("the trace does not show %q:\n%s", want, message.Text)
		}
	}
}

func TestRenderTraceWhenEmpty(t *testing.T) {
	result, err := json.Marshal(v1.EventReplayResult{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	message := RenderOutcome(v1.TransportOutcome{Method: v1.MethodSessionEvents, Result: result})
	if !strings.Contains(message.Text, "No activity") {
		t.Fatalf("text = %q", message.Text)
	}
}

// A pruned cursor is reported rather than shown as a complete conversation.
func TestRenderTraceReportsAGap(t *testing.T) {
	result, err := json.Marshal(v1.EventReplayResult{Gap: &v1.CursorGap{NextSequence: 400}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	message := RenderOutcome(v1.TransportOutcome{Method: v1.MethodSessionEvents, Result: result})
	if !strings.Contains(message.Text, "pruned") || !strings.Contains(message.Text, "400") {
		t.Fatalf("text = %q", message.Text)
	}
}

// Every outcome Hive can produce must render into a message Slack accepts.
//
// Slack refuses a message whose section is over its limit, and refuses the whole
// message, so an oversized render is not a cosmetic problem: nothing is posted at
// all, which is what a user reports as no response. This checks every method the
// transport can render, including the ones with long results.
func TestEveryRenderedOutcomeIsAValidMessage(t *testing.T) {
	methods := []string{
		v1.MethodSessionCreate,
		v1.MethodSessionPrompt,
		v1.MethodSessionCancel,
		v1.MethodSessionStatus,
		v1.MethodSessionList,
		v1.MethodSessionEvents,
		v1.MethodSessionConfig,
		v1.MethodSessionHandoff,
		v1.MethodAgentList,
		v1.MethodNodeList,
	}

	// A result long enough to be interesting: more options than a real agent
	// offers, and a trace with many events.
	longConfig := v1.SessionConfigResult{SessionID: "sess_1", AgentID: "devin"}
	for i := 0; i < 60; i++ {
		longConfig.Options = append(longConfig.Options, v1.SessionConfigOption{
			ID:           fmt.Sprintf("model_%d", i),
			Name:         fmt.Sprintf("Model %d", i),
			Category:     v1.ConfigCategoryModel,
			CurrentValue: json.RawMessage(fmt.Sprintf("%q", fmt.Sprintf("model-%d", i))),
			Options: []v1.SessionConfigOptionValue{
				{Value: fmt.Sprintf("model-%d", i), Name: fmt.Sprintf("Model %d", i)},
			},
		})
	}

	longTrace := v1.EventReplayResult{}
	for i := 0; i < 200; i++ {
		longTrace.Events = append(longTrace.Events, v1.Event{
			Sequence: int64(i),
			Type:     v1.EventTool,
			Payload: json.RawMessage(
				`{"toolCallId":"tc","title":"A tool with a fairly long title","kind":"execute","status":"completed"}`),
		})
	}

	longStatus := v1.SessionStatusResult{SessionID: "sess_1", State: "active"}
	for i := 0; i < 60; i++ {
		longStatus.Runs = append(longStatus.Runs, v1.RunSummary{
			RunID:   fmt.Sprintf("run_%d", i),
			AgentID: "devin",
			NodeID:  "node",
			State:   "completed",
		})
	}

	results := map[string]any{
		v1.MethodSessionConfig: longConfig,
		v1.MethodSessionEvents: longTrace,
		v1.MethodSessionStatus: longStatus,
	}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			var raw json.RawMessage
			if result, ok := results[method]; ok {
				encoded, err := json.Marshal(result)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				raw = encoded
			}

			message := RenderOutcome(v1.TransportOutcome{Method: method, Result: raw})

			if strings.TrimSpace(message.Text) == "" {
				t.Error("a message with no text is not searchable and shows nothing in a notification")
			}
			if len(message.Blocks) == 0 {
				t.Error("a message with no blocks renders as nothing")
			}
			if len(message.Blocks) > MaxBlocks {
				t.Errorf("blocks = %d, over the limit of %d", len(message.Blocks), MaxBlocks)
			}
			for i, block := range message.Blocks {
				if block.Text == nil {
					continue
				}
				if len(block.Text.Text) > MaxSectionChars {
					t.Errorf("block %d is %d characters, over the limit of %d",
						i, len(block.Text.Text), MaxSectionChars)
				}
			}
		})
	}
}

// A very long agent answer is what a user actually sends to Slack, so it is the
// case that matters most.
func TestAVeryLongAnswerRenders(t *testing.T) {
	answer := strings.Repeat("A line of the answer that the agent wrote.\n", 300)
	result, err := json.Marshal(map[string]string{"text": answer})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rendered, ok := Renderer{SessionID: "sess_1"}.RenderEvent(v1.Event{
		Sequence:  1,
		SessionID: "sess_1",
		Type:      v1.EventMessage,
		Payload:   result,
	})
	if !ok {
		t.Fatal("a message event should render")
	}
	message := rendered.Message

	if len(message.Blocks) < 2 {
		t.Fatalf("a %d character answer became %d block(s)", len(answer), len(message.Blocks))
	}
	for i, block := range message.Blocks {
		if len(block.Text.Text) > MaxSectionChars {
			t.Errorf("block %d is over the limit", i)
		}
	}
}

// Cancelling when nothing is running is not a failure.
func TestRenderCancelWithNothingRunningIsNotAWarning(t *testing.T) {
	message := RenderOutcome(v1.TransportOutcome{
		Method: v1.MethodSessionCancel,
		Error:  v1.NewErrorf(v1.CodeConflict, "session sess_1 has no run to cancel"),
	})

	if strings.Contains(message.Text, ":warning:") {
		t.Fatalf("a no-op cancel rendered as a warning: %q", message.Text)
	}
	if !strings.Contains(message.Text, "nothing is running") {
		t.Fatalf("text = %q", message.Text)
	}
}

// An empty declaration says why, rather than reading like a Hive failure.
//
// Found in a live conversation: an agent that exposes its model picker only
// through the legacy ACP models/modes fields declares no configOptions, and the
// answer was "This agent offers no settings.", which reads like a bug in Hive
// rather than a fact about the agent.
func TestAnEmptyConfigExplainsItself(t *testing.T) {
	message := configMessage(v1.SessionConfigResult{SessionID: "sess_1", AgentID: "hermes"})

	for _, want := range []string{"hermes", "legacy ACP", "models", "modes", "configOptions"} {
		if !strings.Contains(message.Text, want) {
			t.Errorf("the explanation does not mention %q: %q", want, message.Text)
		}
	}
}

// With no agent named, the explanation still reads as a sentence.
func TestAnEmptyConfigWithoutAnAgent(t *testing.T) {
	message := configMessage(v1.SessionConfigResult{SessionID: "sess_1"})

	if !strings.HasPrefix(message.Text, "This agent declares") {
		t.Errorf("text = %q", message.Text)
	}
}
