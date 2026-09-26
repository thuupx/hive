package slack

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	slackgo "github.com/slack-go/slack"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
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

	cards := buttons(t, actionsBlock(t, rendered.Message))
	if len(cards) != 2 {
		t.Fatalf("buttons = %d, want allow and deny", len(cards))
	}

	// The value must carry the agent's own request id, so Hive can correlate the
	// response without the transport understanding the request.
	var allowValue struct {
		AgentRequestID string `json:"agentRequestId"`
		Approved       bool   `json:"approved"`
	}
	if err := json.Unmarshal([]byte(cards[0].Value), &allowValue); err != nil {
		t.Fatalf("allow value is not valid json: %v", err)
	}
	if allowValue.AgentRequestID != "7" || !allowValue.Approved {
		t.Fatalf("allow value = %+v", allowValue)
	}

	var denyValue struct {
		Approved bool `json:"approved"`
	}
	if err := json.Unmarshal([]byte(cards[1].Value), &denyValue); err != nil {
		t.Fatalf("deny value is not valid json: %v", err)
	}
	if denyValue.Approved {
		t.Fatal("the deny button must not carry an approval")
	}
}

// The agent decides what a user may choose, so the card offers what it offered.
//
// Found in a live conversation: an agent that offers "Allow once" and "Allow
// always" was rendered as Allow/Deny, so one of its choices could not be made at
// all.
func TestPermissionRendersTheAgentsOwnOptions(t *testing.T) {
	ev := v1.Event{
		Type:      v1.EventPermissionRequested,
		SessionID: "sess_1",
		Payload: json.RawMessage(`{
			"agentRequestId": "e1bf",
			"toolCall": {"title": "Run ls"},
			"options": [
				{"optionId": "allow-once", "name": "Allow once", "kind": "allow"},
				{"optionId": "allow-always", "name": "Allow always", "kind": "allow"},
				{"optionId": "deny", "name": "Deny", "kind": "deny"}
			]
		}`),
	}

	rendered, ok := Renderer{}.RenderEvent(ev)
	if !ok {
		t.Fatal("a permission request must be rendered")
	}

	cards := buttons(t, actionsBlock(t, rendered.Message))
	if len(cards) != 3 {
		t.Fatalf("buttons = %d, want one per option the agent offered", len(cards))
	}
	for i, want := range []string{"Allow once", "Allow always", "Deny"} {
		if cards[i].Text.Text != want {
			t.Errorf("button %d = %q, want %q", i, cards[i].Text.Text, want)
		}
		if !strings.HasPrefix(cards[i].ActionID, ActionPermissionRespond) {
			t.Errorf("button %d action = %q, want a permission response", i, cards[i].ActionID)
		}
	}
	if cards[0].Style != slackgo.StylePrimary || cards[2].Style != slackgo.StyleDanger {
		t.Errorf("styles = %q and %q, want primary and danger", cards[0].Style, cards[2].Style)
	}

	// The value names the choice, so the agent's own option is the one answered.
	var value struct {
		AgentRequestID string `json:"agentRequestId"`
		OptionID       string `json:"optionId"`
		Label          string `json:"label"`
	}
	if err := json.Unmarshal([]byte(cards[1].Value), &value); err != nil {
		t.Fatalf("value is not valid json: %v", err)
	}
	if value.AgentRequestID != "e1bf" || value.OptionID != "allow-always" || value.Label != "Allow always" {
		t.Fatalf("value = %+v", value)
	}
}

// Every action id in a message must be unique.
//
// Found in a live conversation: every option button carried "permission_respond",
// and Slack answered chat.postMessage with "invalid_blocks" — for the whole card.
// A permission card that never appears is an agent waiting for an answer nobody
// can give, which reads as an agent that stopped working.
func TestActionIDsAreUniqueWithinAMessage(t *testing.T) {
	ev := v1.Event{
		Type:      v1.EventPermissionRequested,
		SessionID: "sess_1",
		Payload: json.RawMessage(`{
			"agentRequestId": "e1bf",
			"toolCall": {"title": "Run git log"},
			"options": [
				{"optionId": "allow_once", "name": "Allow", "kind": "allow_once"},
				{"optionId": "allow_session", "name": "Yes, allow for this session", "kind": "allow_always"},
				{"optionId": "allow_always", "name": "Yes, always allow", "kind": "allow_always"},
				{"optionId": "switch_bypass", "name": "Yes, switch to bypass mode", "kind": "allow_always"},
				{"optionId": "reject_once", "name": "Reject", "kind": "reject_once"}
			]
		}`),
	}

	rendered, ok := Renderer{}.RenderEvent(ev)
	if !ok {
		t.Fatal("a permission request must be rendered")
	}

	seen := map[string]bool{}
	for _, card := range buttons(t, actionsBlock(t, rendered.Message)) {
		if card.ActionID == "" {
			t.Error("a button has no action id")
			continue
		}
		if seen[card.ActionID] {
			t.Errorf("action id %q is used twice; Slack rejects the whole message for it", card.ActionID)
		}
		seen[card.ActionID] = true
	}
	if len(seen) != 5 {
		t.Fatalf("action ids = %d, want one per option the agent offered", len(seen))
	}
}

// A suffixed button performs the same Hive operation as the plain one.
func TestASuffixedPermissionActionResolves(t *testing.T) {
	for _, id := range []string{
		ActionPermissionRespond,
		ActionPermissionRespond + ".0",
		ActionPermissionRespond + ".4",
	} {
		if got := actionMethod(id); got != v1.MethodPermissionRespond {
			t.Errorf("actionMethod(%q) = %q, want %q", id, got, v1.MethodPermissionRespond)
		}
	}

	for _, id := range []string{"", "permission_denied", "permission_respondx.0"} {
		if got := actionMethod(id); got != "" {
			t.Errorf("actionMethod(%q) = %q, want empty", id, got)
		}
	}
}

// An agent that offered no choices still needs an answer, or it waits forever.
func TestPermissionWithoutOptionsOffersAllowAndDeny(t *testing.T) {
	ev := v1.Event{
		Type:      v1.EventPermissionRequested,
		SessionID: "sess_1",
		Payload:   json.RawMessage(`{"agentRequestId": "7", "toolCall": {"title": "Write file"}}`),
	}

	rendered, ok := Renderer{}.RenderEvent(ev)
	if !ok {
		t.Fatal("a permission request must be rendered")
	}

	cards := buttons(t, actionsBlock(t, rendered.Message))
	if len(cards) != 2 {
		t.Fatalf("buttons = %d, want allow and deny", len(cards))
	}
	if cards[0].ActionID != ActionPermissionAllow || cards[1].ActionID != ActionPermissionDeny {
		t.Errorf("actions = %q, %q", cards[0].ActionID, cards[1].ActionID)
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
				section, ok := block.(*slackgo.SectionBlock)
				if !ok || section.Text == nil {
					continue
				}
				if len(section.Text.Text) > MaxSectionChars {
					t.Errorf("block %d is %d characters, over the limit of %d",
						i, len(section.Text.Text), MaxSectionChars)
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
		if len(sectionText(t, block)) > MaxSectionChars {
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

// A prompt that had to wait says so, rather than looking like it is already
// working.
func TestRenderQueuedPrompt(t *testing.T) {
	result, err := json.Marshal(v1.SessionPromptResult{CommandID: "cmd_1", Queued: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	message := RenderOutcome(v1.TransportOutcome{Method: v1.MethodSessionPrompt, Result: result})
	if !strings.Contains(message.Text, "Queued") {
		t.Fatalf("text = %q, want the prompt to say it is queued", message.Text)
	}
}

// A prompt that ran immediately is unchanged.
func TestRenderPromptThatRan(t *testing.T) {
	result, err := json.Marshal(v1.SessionPromptResult{CommandID: "cmd_1", RunID: "run_1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	message := RenderOutcome(v1.TransportOutcome{Method: v1.MethodSessionPrompt, Result: result})
	if !strings.Contains(message.Text, "Working on it") {
		t.Fatalf("text = %q", message.Text)
	}
}

// A turn's tool list stays inside the edit limit.
//
// Found in a live conversation: the list is edited as calls arrive, and
// chat.update refuses a text over 4,000 characters while chat.postMessage
// accepts far more. A busy turn's list therefore stopped updating, silently, and
// the reader saw a turn that had quietly stopped reporting.
func TestATurnsToolListStaysInsideTheEditLimit(t *testing.T) {
	calls := make([]v1.ToolCall, 0, 400)
	for i := range 400 {
		calls = append(calls, v1.ToolCall{
			ToolCallID: fmt.Sprintf("call_%d", i),
			Status:     v1.ToolCompleted,
			Title:      "Ran a command with a title long enough to fill the list",
		})
	}

	message := toolRunMessage(calls)

	if len(message.Text) > MaxUpdateChars {
		t.Fatalf("text is %d characters, over the edit limit of %d", len(message.Text), MaxUpdateChars)
	}
	for i, block := range message.Blocks {
		if text := sectionText(t, block); len(text) > MaxSectionChars {
			t.Errorf("block %d is %d characters, over the section limit", i, len(text))
		}
	}
	if !strings.Contains(message.Text, "more") {
		t.Error("a truncated list should say how many calls it left out")
	}
}

// A short list is rendered whole.
func TestAShortToolListIsNotTruncated(t *testing.T) {
	message := toolRunMessage([]v1.ToolCall{
		{ToolCallID: "c1", Status: v1.ToolCompleted, Title: "Ran pwd"},
		{ToolCallID: "c2", Status: v1.ToolFailed, Title: "Ran rtk"},
	})

	if strings.Contains(message.Text, "more") {
		t.Errorf("a two-call list should not be truncated: %q", message.Text)
	}
	if !strings.Contains(message.Text, "2 tool calls") {
		t.Errorf("text = %q, want the count", message.Text)
	}
}

// An agent's kinds are its own: "allow_once" is an approval.
//
// Found in a live conversation: the transport read the kind against "allow", so
// every approval carried approved=false — and the recorded decision said the
// opposite of what the user chose.
func TestAnAgentsAllowOptionCountsAsAnApproval(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want bool
	}{
		{"allow", true},
		{"allow_once", true},
		{"allow_always", true},
		{"deny", false},
		{"reject_once", false},
		{"switch_bypass", false},
		{"", false},
	} {
		if got := allows(tc.kind); got != tc.want {
			t.Errorf("allows(%q) = %v, want %v", tc.kind, got, tc.want)
		}
	}

	ev := v1.Event{
		Type:      v1.EventPermissionRequested,
		SessionID: "sess_1",
		Payload: json.RawMessage(`{
			"agentRequestId": "e1bf",
			"toolCall": {"title": "Run git"},
			"options": [{"optionId": "allow_once", "name": "Allow", "kind": "allow_once"}]
		}`),
	}

	rendered, ok := Renderer{}.RenderEvent(ev)
	if !ok {
		t.Fatal("a permission request must be rendered")
	}

	var value struct {
		Approved bool `json:"approved"`
	}
	cards := buttons(t, actionsBlock(t, rendered.Message))
	if err := json.Unmarshal([]byte(cards[0].Value), &value); err != nil {
		t.Fatalf("value is not valid json: %v", err)
	}
	if !value.Approved {
		t.Error("an allow option must carry an approval, or a reader falls back to denying it")
	}
}

// What a turn cost is not a message of its own.
//
// One line per turn is noise in a busy conversation, and the status command
// already answers the question. The report is still published, so /status has the
// numbers; it is simply not announced.
func TestUsageIsNotRenderedIntoTheConversation(t *testing.T) {
	payload, err := json.Marshal(v1.Usage{
		InputTokens: 52854, OutputTokens: 677, TotalTokens: 53531,
		ContextUsed: 53531, ContextSize: 200000,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if _, ok := (Renderer{}).RenderEvent(v1.Event{Type: v1.EventUsage, Payload: payload}); ok {
		t.Error("a usage report should not be rendered into the conversation")
	}
}

// A count is read at a glance, so it is grouped.
func TestCommas(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{53531, "53,531"},
		{1234567, "1,234,567"},
	} {
		if got := commas(tc.n); got != tc.want {
			t.Errorf("commas(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// A status read answers what a user asks about a conversation: which agent and
// settings it runs with, what it has done, and what the last turn cost.
func TestRenderStatusWithStatistics(t *testing.T) {
	status := v1.SessionStatusResult{
		SessionID: "sess_1",
		State:     "running",
		Runs:      []v1.RunSummary{{RunID: "run_1", AgentID: "hermes", NodeID: "n1", State: "completed"}},
		Settings:  map[string]string{"model": "anthropic/claude-opus-4.6", "mode": "code"},
		ToolCalls: 12,
		Usage: &v1.Usage{
			InputTokens: 52854, OutputTokens: 677, TotalTokens: 53531,
			ContextUsed: 53531, ContextSize: 200000,
		},
	}

	message := statusMessage(&status)
	for _, want := range []string{
		"hermes",
		"model `anthropic/claude-opus-4.6`",
		"mode `code`",
		"12",
		"53,531 tokens",
	} {
		if !strings.Contains(message.Text, want) {
			t.Errorf("the status does not mention %q: %q", want, message.Text)
		}
	}
}

// A session with nothing recorded says so by omission rather than by a line of
// zeroes.
func TestRenderStatusWithoutStatistics(t *testing.T) {
	message := statusMessage(&v1.SessionStatusResult{SessionID: "sess_1", State: "idle"})

	for _, unwanted := range []string{"Settings", "Tool calls", "Tokens"} {
		if strings.Contains(message.Text, unwanted) {
			t.Errorf("the status claims %q with nothing recorded: %q", unwanted, message.Text)
		}
	}
}
