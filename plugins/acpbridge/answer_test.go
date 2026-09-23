package acpbridge

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thupham/hive/plugins/acp"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// The answer to a prompt must be readable. Without this normalization the agent's
// reply is only reachable as raw protocol chunks.
func TestAgentMessageTextExtractsTheAnswer(t *testing.T) {
	payload := json.RawMessage(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"HIVE_ROUNDTRIP_OK"}}`)

	text, ok := agentMessageText(payload)
	if !ok {
		t.Fatal("an agent message chunk should yield text")
	}
	if text != "HIVE_ROUNDTRIP_OK" {
		t.Fatalf("text = %q", text)
	}
}

// Every other notification stays raw: Hive normalizes only what it needs.
func TestAgentMessageTextIgnoresOtherUpdates(t *testing.T) {
	cases := map[string]string{
		"another session update": `{"sessionUpdate":"usage_update","used":1,"size":2}`,
		"non-text content":       `{"sessionUpdate":"agent_message_chunk","content":{"type":"image","text":""}}`,
		"empty text":             `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":""}}`,
		"not an update":          `{"something":"else"}`,
		"not json":               `{`,
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if text, ok := agentMessageText(json.RawMessage(payload)); ok {
				t.Fatalf("text = %q, want the payload to stay raw", text)
			}
		})
	}
}

// Chunks accumulate into one answer for the turn, and the buffer resets so the
// next turn does not repeat the previous one.
func TestRunAccumulatesOneAnswerPerTurn(t *testing.T) {
	r := &run{agentRunID: "run_1", hiveSessionID: "sess_1"}

	for _, chunk := range []string{"HIVE_", "ROUND", "TRIP_OK"} {
		payload := json.RawMessage(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` + chunk + `"}}`)
		text, ok := agentMessageText(payload)
		if !ok {
			t.Fatalf("chunk %q did not yield text", chunk)
		}
		r.answer.WriteString(text)
	}

	if got := strings.TrimSpace(r.answer.String()); got != "HIVE_ROUNDTRIP_OK" {
		t.Fatalf("answer = %q", got)
	}

	r.answer.Reset()
	if got := strings.TrimSpace(r.answer.String()); got != "" {
		t.Fatalf("the buffer did not reset: %q", got)
	}
}

// An agent that advertises auth methods refuses to create a session until the
// client authenticates, and which method to use is configuration.
func TestAuthMethodSelection(t *testing.T) {
	methods := []acp.AuthMethod{
		{ID: "devin-browser", Name: "Log in with browser"},
		{ID: "api-key", Name: "API key"},
	}

	if !hasAuthMethod(methods, "api-key") {
		t.Error("api-key should be offered")
	}
	if hasAuthMethod(methods, "nope") {
		t.Error("nope is not offered")
	}

	names := authMethodNames(methods)
	if !strings.Contains(names, "devin-browser") || !strings.Contains(names, "api-key") {
		t.Fatalf("names = %q", names)
	}
}

// A tool update usually omits the title, so the first one has to be remembered.
//
// Found in a live conversation: the card read "tool" for its whole life because
// the remembered title was only consulted when the update already had one.
func TestToolCallRemembersItsTitle(t *testing.T) {
	b := &Bridge{
		tools:      make(map[string]map[string]bool),
		toolTitles: make(map[string]map[string]string),
	}
	r := &run{agentRunID: "run_1"}

	first, ok := b.toolCall(r, json.RawMessage(
		`{"sessionUpdate":"tool_call","toolCallId":"tc1","title":"Listed ./","kind":"execute"}`))
	if !ok {
		t.Fatal("the first update should be a tool call")
	}
	if first.Title != "Listed ./" || !first.Started {
		t.Fatalf("first = %+v", first)
	}

	update, ok := b.toolCall(r, json.RawMessage(
		`{"sessionUpdate":"tool_call_update","toolCallId":"tc1","status":"completed"}`))
	if !ok {
		t.Fatal("the update should be a tool call")
	}
	if update.Title != "Listed ./" {
		t.Fatalf("title = %q, want the remembered one", update.Title)
	}
	if update.Started {
		t.Error("a later update must not look like the first")
	}
	if update.Status != v1.ToolCompleted {
		t.Errorf("status = %q", update.Status)
	}
}
