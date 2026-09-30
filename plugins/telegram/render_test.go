package telegram

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// Markdown an agent writes becomes the HTML Telegram renders.
func TestMarkdownBecomesHTML(t *testing.T) {
	got := markdown("see **this** and `code` and [the doc](https://x.dev)")
	for _, want := range []string{
		"<b>this</b>", "<code>code</code>", `<a href="https://x.dev">the doc</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("markdown = %q, want %q", got, want)
		}
	}
}

// HTML in the prose is escaped so it arrives as text, not markup.
func TestHTMLIsEscaped(t *testing.T) {
	got := markdown("a < b & c > d")
	if strings.Contains(got, "<b") || !strings.Contains(got, "a &lt; b &amp; c &gt; d") {
		t.Fatalf("markdown = %q, want the angle brackets escaped", got)
	}
}

// A fenced block is a <pre>, and its contents are shown, not formatted.
func TestAFenceIsPreformatted(t *testing.T) {
	got := markdown("before\n```go\nif a < b {\n}\n```\nafter")
	if !strings.Contains(got, "<pre>") || !strings.Contains(got, "if a &lt; b") {
		t.Fatalf("markdown = %q, want the fence preformatted", got)
	}
}

// A link inside code is shown as markup, not rewritten as an anchor.
func TestALinkInsideCodeStaysAsMarkup(t *testing.T) {
	got := markdown("run `[a](b)` then see [the doc](https://x.dev)")
	if !strings.Contains(got, "<code>[a](b)</code>") {
		t.Fatalf("the code span must keep its markup, got %q", got)
	}
	if !strings.Contains(got, `<a href="https://x.dev">the doc</a>`) {
		t.Fatalf("the prose link should become an anchor, got %q", got)
	}
}

// A markdown image becomes a photo: Telegram draws no picture inside a text
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
// and not a path cannot become a photo and stays in the text.
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

// Markup inside code is shown, not sent: an image inside a span or a fence is
// an example an agent is quoting, not a picture it is sending.
func TestAnImageInsideCodeStaysAsMarkup(t *testing.T) {
	text, images := extractImages("example `![alt](https://example/x.png)` and ![real](https://cdn.example/a.png)")
	if len(images) != 1 || images[0].Target != "https://cdn.example/a.png" {
		t.Fatalf("only the prose image should extract, images = %+v", images)
	}
	if !strings.Contains(text, "`![alt](https://example/x.png)`") {
		t.Fatalf("the code span must keep its markup, text = %q", text)
	}

	fenced := "```go\n![alt](https://example/x.png)\n```"
	text, images = extractImages(fenced)
	if len(images) != 0 || !strings.Contains(text, "![alt](https://example/x.png)") {
		t.Fatalf("a fenced image must stay as markup, text = %q images %+v", text, images)
	}
}

// A permission request renders its options as buttons.
func TestAPermissionRendersButtons(t *testing.T) {
	request := permissionRequest{
		AgentRequestID: "req1",
		Options: []permissionOption{
			{OptionID: "a1", Name: "Allow once", Kind: "allow_once"},
			{OptionID: "d1", Name: "Deny", Kind: "reject_once"},
		},
	}
	request.ToolCall.Title = "Run rm -rf"

	message := permissionMessage(request)
	if len(message.Keyboard) != 2 {
		t.Fatalf("keyboard = %+v, want one row per option", message.Keyboard)
	}
	if message.Keyboard[0][0].CallbackData != "perm:0" || message.Keyboard[0][0].Text != "Allow once" {
		t.Fatalf("button = %+v", message.Keyboard[0][0])
	}
	if !strings.Contains(message.Text, "Run rm -rf") {
		t.Fatalf("text = %q, want what is being asked", message.Text)
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
	if rendered.Message.ParseMode != "HTML" {
		t.Fatalf("parse mode = %q", rendered.Message.ParseMode)
	}
}

// A permission event renders with buttons remembered for the callback.
func TestAPermissionEventRenders(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"agentRequestId": "req1",
		"toolCall":       map[string]string{"title": "write a file"},
		"options":        []map[string]string{{"optionId": "a1", "name": "Allow", "kind": "allow_once"}},
	})
	rendered, ok := Renderer{}.RenderEvent(v1.Event{Type: v1.EventPermissionRequested, Payload: payload})
	if !ok {
		t.Fatal("a permission request should render")
	}
	if rendered.Permission == nil || len(rendered.Message.Keyboard) != 1 {
		t.Fatalf("rendered = %+v", rendered)
	}
}

// A fence an agent wrapped its whole answer in is unwrapped, but real code is
// left alone.
func TestMarkdownUnwrapsProseFence(t *testing.T) {
	prose := "```markdown\n# Title\nsome text\n```"
	if got := markdown(prose); !strings.Contains(got, "<b>Title</b>") {
		t.Fatalf("prose fence = %q", got)
	}

	code := "```go\nfunc main() {}\n```"
	if got := markdown(code); !strings.Contains(got, "<pre>") || !strings.Contains(got, "func main()") {
		t.Fatalf("a real code fence must render as code, got %q", got)
	}
}

// A link URL with a query string must not be entity-escaped twice: "&amp;" in
// the escaped text would become "&amp;amp;" in the href, opening nowhere.
func TestALinkURLIsEscapedOnce(t *testing.T) {
	got := markdown("see [docs](https://a.example/?x=1&y=2)")
	want := `<a href="https://a.example/?x=1&amp;y=2">docs</a>`
	if !strings.Contains(got, want) {
		t.Fatalf("link = %q, want %q", got, want)
	}
}

// An underscore inside a word is a name, not emphasis.
func TestSnakeCaseIsNotItalic(t *testing.T) {
	got := markdown("check_file_name is the field")
	if strings.Contains(got, "<i>") {
		t.Fatalf("snake_case must not render as italic, got %q", got)
	}
	if got = markdown("this is _emphasized_ text"); !strings.Contains(got, "<i>emphasized</i>") {
		t.Fatalf("an underscore span should still be italic, got %q", got)
	}
}

// An image target that is not a photo stays in the text literally rather than
// becoming a link with a stray bang.
func TestADataImageIsNotLinkified(t *testing.T) {
	got := markdown("an inline dot: ![dot](data:image/png;base64,xyz)")
	if strings.Contains(got, "<a ") {
		t.Fatalf("a data: image should not become a link, got %q", got)
	}
	if !strings.Contains(got, "![dot](data:image/png;base64,xyz)") {
		t.Fatalf("the image markup should stay literal, got %q", got)
	}
}

// A chunk never ends inside a multi-byte rune.
func TestAChunkEndsOnARuneBoundary(t *testing.T) {
	line := strings.Repeat("é", 500) // two bytes each
	parts := chunk(line, 51)
	for _, part := range parts {
		if !utf8.ValidString(part) {
			t.Fatalf("a chunk is not valid UTF-8: %q...", part[:10])
		}
	}
}

// A split inside a <pre> block closes and reopens the tag, so no chunk is
// malformed markup — Telegram refuses that, and the plain repost shows the
// tags literally.
func TestAChunkInsidePreIsBalanced(t *testing.T) {
	text := "lead-in\n\n<pre>" + strings.Repeat("diagram line\n", 40) + "</pre>\ntail"
	parts := balanceChunks(chunk(text, 120))
	if len(parts) < 2 {
		t.Fatalf("want the message split, got %d part(s)", len(parts))
	}
	for i, part := range parts {
		if open := tagStack(part); len(open) != 0 {
			t.Fatalf("chunk %d leaves %v open: %q", i, open, part)
		}
	}
	if parts[0][len(parts[0])-6:] != "</pre>" || !strings.HasPrefix(parts[1], "<pre>") {
		t.Fatalf("the boundary chunks should carry the split pre, got %q / %q", parts[0], parts[1])
	}
	if strings.Count(strings.Join(parts, ""), "diagram line") != 40 {
		t.Fatal("balancing dropped or duplicated content")
	}
	if !strings.HasSuffix(strings.Join(parts, ""), "</pre>\ntail") {
		t.Fatalf("the tail lost its context: %q", parts[len(parts)-1])
	}
}

// A cut inside a tag or an entity moves back to where it starts, so a chunk
// never ends in half a markup construct.
func TestAChunkNeverSplitsATagOrEntity(t *testing.T) {
	long := strings.Repeat("x", 60)
	line := `<a href="https://a.example/?x=1&amp;y=2">` + long + `</a>`
	for _, part := range chunk(line, 50) {
		if lt, gt := strings.LastIndexByte(part, '<'), strings.LastIndexByte(part, '>'); lt > gt {
			t.Fatalf("a chunk ends inside a tag: %q", part)
		}
		if amp, semi := strings.LastIndexByte(part, '&'), strings.LastIndexByte(part, ';'); amp > semi {
			t.Fatalf("a chunk ends inside an entity: %q", part)
		}
	}
}
