package acpbridge

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

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

// An agent that cannot accept images is told about them instead.
//
// Refusing the whole turn because a picture was attached would lose the text too.
func TestDescribeImagesNamesThePictures(t *testing.T) {
	images := []v1.PromptImage{
		{Name: "screenshot.png", MimeType: "image/png", Data: []byte{1}},
		{MimeType: "image/jpeg", Data: []byte{2}},
	}

	described := describeImages("what is this?", images)

	if !strings.Contains(described, "what is this?") {
		t.Errorf("the text was lost: %q", described)
	}
	if !strings.Contains(described, "2 image(s)") {
		t.Errorf("the count is missing: %q", described)
	}
	if !strings.Contains(described, "screenshot.png") {
		t.Errorf("the name is missing: %q", described)
	}
	// A file with no name is described by its type rather than left blank.
	if !strings.Contains(described, "image/jpeg") {
		t.Errorf("an unnamed image should be described by its type: %q", described)
	}
}

// A picture with no text still produces something to send.
func TestDescribeImagesWithoutText(t *testing.T) {
	described := describeImages("", []v1.PromptImage{{Name: "a.png", Data: []byte{1}}})

	if strings.TrimSpace(described) == "" {
		t.Fatal("an image with no text produced an empty prompt")
	}
	if strings.HasPrefix(described, "\n") {
		t.Errorf("the description starts with a blank line: %q", described)
	}
}

// A finished turn drops its per-turn tool state.
//
// Keeping it would accumulate one entry per tool call for the life of the
// process, which is a leak in anything that runs for weeks.
func TestReleaseTurnDropsToolState(t *testing.T) {
	b := &Bridge{
		tools:      map[string]map[string]bool{"run_1": {"tc1": true}},
		toolTitles: map[string]map[string]string{"run_1": {"tc1": "Listed ./"}},
	}

	b.releaseTurn("run_1")

	if _, ok := b.tools["run_1"]; ok {
		t.Error("the tool calls of a finished turn should be dropped")
	}
	if _, ok := b.toolTitles["run_1"]; ok {
		t.Error("the tool titles of a finished turn should be dropped")
	}
}

// A long-lived process must not remember every run it has ever started.
func TestRunsAreBounded(t *testing.T) {
	b := &Bridge{
		runs:       map[string]*run{},
		tools:      map[string]map[string]bool{},
		toolTitles: map[string]map[string]string{},
	}

	for i := 0; i < maxLiveRuns+10; i++ {
		id := fmt.Sprintf("run_%d", i)
		b.runs[id] = &run{agentRunID: id}
		b.tools[id] = map[string]bool{"tc": true}
		b.toolTitles[id] = map[string]string{"tc": "t"}
		b.runOrder = append(b.runOrder, id)
	}

	b.mu.Lock()
	b.evictRunsLocked()
	b.mu.Unlock()

	if len(b.runs) != maxLiveRuns {
		t.Fatalf("runs = %d, want %d", len(b.runs), maxLiveRuns)
	}
	if len(b.tools) != maxLiveRuns || len(b.toolTitles) != maxLiveRuns {
		t.Fatalf("tool state = %d/%d, want %d", len(b.tools), len(b.toolTitles), maxLiveRuns)
	}
	// The oldest go first.
	if _, ok := b.runs["run_0"]; ok {
		t.Error("the oldest run should have been evicted")
	}
	if _, ok := b.runs[fmt.Sprintf("run_%d", maxLiveRuns+9)]; !ok {
		t.Error("the newest run should still be there")
	}
}

// A message that follows a tool call is a new message, not a continuation.
//
// Found in a live conversation: an agent narrated, called a tool, and narrated
// again, and the two narrations were glued into one sentence that read as
// nonsense.
func TestMessagesAreSeparatedByWhatComesBetweenThem(t *testing.T) {
	b := &Bridge{
		tools:      map[string]map[string]bool{},
		toolTitles: map[string]map[string]string{},
	}
	r := &run{agentRunID: "run_1"}

	// Two chunks in a row are one message.
	message(t, b, r, "Mình muốn, cho mình biết")
	message(t, b, r, " thêm.")

	// A tool call comes between them.
	other(t, b, r)

	// So the next chunk starts a new message.
	message(t, b, r, "Users/th là thư mục.")

	b.mu.Lock()
	answer := r.answer.String()
	b.mu.Unlock()

	want := "Mình muốn, cho mình biết thêm.\n\nUsers/th là thư mục."
	if answer != want {
		t.Fatalf("answer = %q, want %q", answer, want)
	}
}

// A thought between two chunks does not separate them.
//
// Found in a live conversation: a two-word answer came out as "FIRST\n\n_TURN"
// because a thought chunk had arrived between the two halves of one message.
func TestAThoughtDoesNotSeparateAMessage(t *testing.T) {
	b := &Bridge{
		tools:      map[string]map[string]bool{},
		toolTitles: map[string]map[string]string{},
	}
	r := &run{agentRunID: "run_1"}

	thought(t, b, r)
	message(t, b, r, "FIRST")
	thought(t, b, r)
	message(t, b, r, "_TURN")

	b.mu.Lock()
	answer := r.answer.String()
	b.mu.Unlock()

	if answer != "FIRST_TURN" {
		t.Fatalf("answer = %q, want the two halves joined", answer)
	}
}

// message feeds one assistant text chunk through the bridge.
func message(t *testing.T, b *Bridge, r *run, text string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]any{"type": "text", "text": text},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b.collectAnswer(r, payload)
}

// other feeds a tool call, which is what ends a narration.
func other(t *testing.T, b *Bridge, r *run) {
	t.Helper()
	b.collectAnswer(r, json.RawMessage(
		`{"sessionUpdate":"tool_call","toolCallId":"tc1","title":"Ran pwd","kind":"execute"}`))
}

// thought feeds a thought, which arrives between chunks of one message.
func thought(t *testing.T, b *Bridge, r *run) {
	t.Helper()
	b.collectAnswer(r, json.RawMessage(
		`{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}}`))
}

// An update goes to the run whose turn is in flight, not to a random run.
//
// Found in a live conversation: a conversation continues in a new run that
// restores the same agent session, so two runs named one session, and picking one
// by ranging over a map split an answer between them. The text read as nonsense.
func TestAnUpdateGoesToTheRunInATurn(t *testing.T) {
	b := &Bridge{runs: map[string]*run{}}

	older := &run{agentRunID: "run_old", sessionID: "sess_1", startedAt: time.Now().Add(-time.Minute)}
	newer := &run{agentRunID: "run_new", sessionID: "sess_1", startedAt: time.Now()}
	b.runs["run_old"] = older
	b.runs["run_new"] = newer

	// Between turns the newest is the answer, and it is the same every time.
	for i := 0; i < 20; i++ {
		if got := b.runFor("sess_1"); got != newer {
			t.Fatalf("between turns the update went to %q, want the newest run", got.agentRunID)
		}
	}

	// The run in a turn is the answer, even when an older run is the newest.
	older.inTurn = true
	for i := 0; i < 20; i++ {
		if got := b.runFor("sess_1"); got != older {
			t.Fatalf("the update went to %q, want the run in a turn", got.agentRunID)
		}
	}
}

// An update for a session no run knows is dropped rather than guessed at.
func TestAnUpdateForAnUnknownSessionIsDropped(t *testing.T) {
	b := &Bridge{runs: map[string]*run{
		"run_1": {agentRunID: "run_1", sessionID: "sess_1"},
	}}

	if got := b.runFor("sess_other"); got != nil {
		t.Fatalf("runFor returned %q for a session no run knows", got.agentRunID)
	}
}

// The agent's request id is text, not the JSON that carries it.
//
// Found in a live permission card: the value the button carried was
// "03b7dd82-..." with the quotes still on it, so the id a click sent back did not
// match the id that was stored and the click answered nothing.
func TestTheRequestIDLosesItsJSONQuotes(t *testing.T) {
	cases := map[string]string{
		`"03b7dd82-eb1b-4362-bb6e-a269f0818cd3"`: "03b7dd82-eb1b-4362-bb6e-a269f0818cd3",
		`"99"`:                                   "99",
		`99`:                                     "99",
		`"a\"b"`:                                 `a"b`,
	}

	for raw, want := range cases {
		if got := requestIDText(json.RawMessage(raw)); got != want {
			t.Errorf("requestIDText(%s) = %q, want %q", raw, got, want)
		}
	}
}
