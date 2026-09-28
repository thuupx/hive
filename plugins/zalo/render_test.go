package zalo

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// A markdown link becomes text and a URL, which is what Zalo's markup renders.
func TestMarkdownConvertsLinks(t *testing.T) {
	got := markdown("see [the docs](https://example.com/a) for more")
	if got != "see the docs (https://example.com/a) for more" {
		t.Fatalf("markdown = %q", got)
	}
}

// A markdown image becomes a photo: Zalo draws no picture inside a text
// message, so the markup comes out of the text and the target is carried
// alongside.
func TestAMarkdownImageBecomesAPhoto(t *testing.T) {
	text, images := extractImages("here it is ![the shot](https://cdn.example/a.png) done")

	if text != "here it is  done" && text != "here it is done" {
		t.Fatalf("text = %q, want the markup removed", text)
	}
	if len(images) != 1 || images[0].Target != "https://cdn.example/a.png" || images[0].Alt != "the shot" {
		t.Fatalf("images = %+v", images)
	}
}

// A local path is a photo the transport uploads; a scheme that is not http(s)
// cannot become a photo and stays in the text.
func TestImageTargetsAreSortedByScheme(t *testing.T) {
	_, images := extractImages("![shot](/tmp/shot.png)")
	if len(images) != 1 || images[0].Target != "/tmp/shot.png" {
		t.Fatalf("a file path should be a photo, images = %+v", images)
	}

	text, images := extractImages("![x](data:image/png;base64,AAA)")
	if len(images) != 0 || !strings.Contains(text, "data:image/png") {
		t.Fatalf("a data: URI must stay in the text, got %q images %+v", text, images)
	}
}

// A message event carrying a markdown image renders as text plus a photo.
func TestAMessageEventWithAnImage(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"text": "look ![s](https://cdn.example/a.png)"})
	rendered, ok := Renderer{}.RenderEvent(v1.Event{Type: v1.EventMessage, Payload: payload})
	if !ok {
		t.Fatal("a message event should render")
	}
	if strings.Contains(rendered.Message.Text, "![") || len(rendered.Message.Images) != 1 {
		t.Fatalf("rendered = %+v", rendered.Message)
	}
}

// A fence an agent wrapped its whole answer in is unwrapped, but real code is
// left alone.
func TestMarkdownUnwrapsProseFence(t *testing.T) {
	prose := "```markdown\n# Title\nsome text\n```"
	if got := markdown(prose); got != "# Title\nsome text" {
		t.Fatalf("prose fence = %q", got)
	}

	code := "```go\nfunc main() {}\n```"
	if got := markdown(code); got != code {
		t.Fatalf("a real code fence must be left as it is, got %q", got)
	}
}

// A long answer is split so it fits Zalo's limit, and nothing is lost.
func TestChunkSplitsLongText(t *testing.T) {
	line := strings.Repeat("x", 100)
	text := strings.Join([]string{line, line, line, line, line}, "\n")

	chunks := chunk(text, 220)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want the text split", len(chunks))
	}
	for i, part := range chunks {
		if len(part) > 220 {
			t.Errorf("chunk %d is %d chars, over the limit", i, len(part))
		}
	}
	if joined := strings.Join(chunks, "\n"); joined != text {
		t.Errorf("splitting lost or changed text:\n got %q\nwant %q", joined, text)
	}
}

// A short message is not split.
func TestChunkKeepsShortText(t *testing.T) {
	if chunks := chunk("hello", 100); len(chunks) != 1 || chunks[0] != "hello" {
		t.Fatalf("chunks = %v", chunks)
	}
}

// A permission card says how to answer, because there are no buttons.
func TestPermissionMessageListsOptions(t *testing.T) {
	message := permissionMessage(permissionRequest{
		AgentRequestID: "req1",
		Options: []permissionOption{
			{OptionID: "a", Name: "Allow once", Kind: "allow_once"},
			{OptionID: "b", Name: "Deny", Kind: "reject"},
		},
	})

	for _, want := range []string{"Permission requested", "1. Allow once", "2. Deny", "allow", "deny"} {
		if !strings.Contains(message.Text, want) {
			t.Errorf("the card does not mention %q: %q", want, message.Text)
		}
	}
}

// A card with no options still tells the user how to answer.
func TestPermissionMessageWithoutOptions(t *testing.T) {
	message := permissionMessage(permissionRequest{AgentRequestID: "req1"})
	if !strings.Contains(message.Text, "allow") || !strings.Contains(message.Text, "deny") {
		t.Fatalf("the card should say how to answer: %q", message.Text)
	}
}

// The transport shows the answer and the problems, not the agent's steps.
func TestRenderEventSuppressesSteps(t *testing.T) {
	renderer := Renderer{SessionID: "s1"}

	answer := v1.Event{SessionID: "s1", Type: v1.EventMessage, Payload: mustJSON(t, map[string]string{"text": "done"})}
	if _, ok := renderer.RenderEvent(answer); !ok {
		t.Error("an answer should render")
	}

	for _, eventType := range []string{v1.EventTool, v1.EventRunStarted, v1.EventRunFinished, v1.EventStatus, v1.EventAgentRaw} {
		ev := v1.Event{SessionID: "s1", Type: eventType, Payload: mustJSON(t, map[string]any{})}
		if _, ok := renderer.RenderEvent(ev); ok {
			t.Errorf("%s should not be rendered", eventType)
		}
	}
}

// A permission request is rendered and handed back so the transport can remember
// it.
func TestRenderPermissionRequest(t *testing.T) {
	ev := v1.Event{
		SessionID: "s1",
		Type:      v1.EventPermissionRequested,
		Payload: mustJSON(t, map[string]any{
			"agentRequestId": "req1",
			"toolCall":       map[string]string{"title": "Write file"},
			"options":        []map[string]string{{"optionId": "a", "name": "Allow", "kind": "allow_once"}},
		}),
	}

	rendered, ok := (Renderer{SessionID: "s1"}).RenderEvent(ev)
	if !ok {
		t.Fatal("a permission request should render")
	}
	if rendered.Permission == nil || rendered.Permission.AgentRequestID != "req1" {
		t.Fatalf("permission = %+v, want the request remembered", rendered.Permission)
	}
	if !strings.Contains(rendered.Message.Text, "Write file") {
		t.Errorf("the card should name the operation: %q", rendered.Message.Text)
	}
}

// A cancel that had nothing to cancel is not a failure.
func TestRenderOutcomeConflict(t *testing.T) {
	outcome := v1.TransportOutcome{
		Method: v1.MethodSessionCancel,
		Error:  v1.Conflict("nothing to cancel"),
	}
	if got := RenderOutcome(outcome).Text; !strings.Contains(got, "nothing is running") {
		t.Fatalf("text = %q", got)
	}
}

// The help text is generated from the catalog, so it cannot drift from it.
func TestHelpListsEveryCommand(t *testing.T) {
	message := HelpMessage()
	for _, entry := range Catalog() {
		if !strings.Contains(message.Text, entry.Command) {
			t.Errorf("help does not list %q", entry.Command)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}
