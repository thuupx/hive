package telegram

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// Renderer turns Hive events into Telegram messages.
type Renderer struct {
	// SessionID scopes rendering to the session bound to this conversation.
	SessionID string
}

// Rendered is a message to post.
type Rendered struct {
	Message Message

	// Permission is the request this event carried, when it carried one. The
	// transport remembers it so a button press or a worded reply can be
	// resolved into an option.
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

// RenderEvent renders one Hive event, reporting false when it has no Telegram
// representation.
//
// Tool calls, run boundaries, usage, and the agent's raw stream are
// deliberately not rendered: a chat is not a log, and the transport shows the
// answer and the problems rather than narrating every step.
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
// A command can complete synchronously or asynchronously, so the transport
// must not assume the request lifetime equals the operation lifetime. This
// renders the acknowledgement; later state arrives as events.
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

// permissionMessage renders a permission request as a message with buttons.
//
// Telegram has buttons where Zalo has none, so a request offers the agent's
// options directly. The worded replies stay accepted as well, for a keyboard
// that never renders or a user who types.
func permissionMessage(request permissionRequest) Message {
	var text strings.Builder
	fmt.Fprintf(&text, "🔐 Permission requested\n<b>%s</b>\n", escapeHTML(request.label()))

	message := textMessage(text.String())
	if len(request.Options) == 0 {
		message.Text += "\nReply `allow` to approve or `deny` to refuse."
		return message
	}

	var lines strings.Builder
	buttons := make([][]InlineButton, 0, len(request.Options))
	for i, option := range request.Options {
		fmt.Fprintf(&lines, "%d. %s\n", i+1, escapeHTML(optionLabel(option)))
		// The callback data names the option by index; the transport remembers
		// the request and resolves the number into the option's own id.
		buttons = append(buttons, []InlineButton{{
			Text:         optionLabel(option),
			CallbackData: fmt.Sprintf("perm:%d", i),
		}})
	}
	message.Text += "\n" + lines.String() + "\nTap a button, or reply with the number or `allow` / `deny`."
	message.Keyboard = buttons
	return message
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

// textMessage renders text a user reads, as formatted markdown turned to HTML.
func textMessage(text string) Message {
	return Message{Text: markdown(text), ParseMode: "HTML"}
}

// plainMessage renders text with no formatting.
//
// It is for output whose own punctuation is the point: a trace full of
// backticks and asterisks would be read as markup and arrive mangled.
func plainMessage(text string) Message {
	return Message{Text: text}
}

// markdown normalizes what an agent writes into the HTML Telegram renders.
//
// Telegram's HTML mode accepts a small set of tags, so the conversion is
// deliberate rather than full markdown: bold, italic, and strikethrough become
// tags; a fence becomes <pre> and a span <code>; a link becomes an anchor;
// everything else is escaped and left as plain text.
func markdown(text string) string {
	text = unwrapProseFence(text)
	var b strings.Builder
	last := 0
	for _, r := range codeRanges(text) {
		b.WriteString(markdownProse(text[last:r[0]]))
		b.WriteString(codeHTML(text[r[0]:r[1]]))
		last = r[1]
	}
	b.WriteString(markdownProse(text[last:]))
	return b.String()
}

// markdownProse formats the non-code parts of an answer.
func markdownProse(text string) string {
	text = escapeHTML(text)
	text = boldPattern.ReplaceAllString(text, "<b>$1</b>")
	// Two alternatives share one pattern: a lone-asterisk span needs no
	// boundary, but an underscore inside a word — snake_case — is a name, not
	// emphasis, which is why the underscore form asks for non-word edges.
	text = italicPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := italicPattern.FindStringSubmatch(match)
		if parts[1] != "" {
			return "<i>" + parts[1] + "</i>"
		}
		return parts[2] + "<i>" + parts[3] + "</i>" + parts[4]
	})
	text = strikePattern.ReplaceAllString(text, "<s>$1</s>")
	text = headingPattern.ReplaceAllString(text, "<b>$1</b>")
	return linkPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := linkPattern.FindStringSubmatch(match)
		if parts[1] == "!" {
			// An image the extractor left alone — a data: URI, say — is shown
			// literally rather than linkified with a stray "!" beside it.
			return match
		}
		// The URL is already entity-escaped; escaping it again writes a
		// double-escaped href ("&amp;amp;") that opens nowhere.
		return fmt.Sprintf(`<a href="%s">%s</a>`, escapeAttr(unescapeHTML(parts[3])), parts[2])
	})
}

// unescapeHTML undoes the entity escaping a URL picked up inside escaped
// text, so it can be escaped once for the attribute it lands in.
func unescapeHTML(text string) string {
	return htmlUnescaper.Replace(text)
}

var htmlUnescaper = strings.NewReplacer(
	"&lt;", "<",
	"&gt;", ">",
	"&amp;", "&",
)

// codeHTML renders a code region: a fence as <pre>, a span as <code>.
func codeHTML(region string) string {
	if strings.HasPrefix(region, "```") {
		inner := fenceBody(region)
		return "<pre>" + escapeHTML(inner) + "</pre>"
	}
	// An inline span is wrapped in a run of backticks of equal length.
	trimmed := strings.Trim(region, "`")
	return "<code>" + escapeHTML(trimmed) + "</code>"
}

// fenceBody is what is inside a fenced code block's markers.
func fenceBody(region string) string {
	lines := strings.Split(region, "\n")
	if len(lines) < 2 {
		return ""
	}
	// The first line is the opening fence with its language; the last is the
	// closing fence when it is one.
	inner := lines[1:]
	if last := strings.TrimSpace(inner[len(inner)-1]); fence(last) {
		inner = inner[:len(inner)-1]
	}
	return strings.Join(inner, "\n")
}

// unwrapProseFence removes a fence an agent wrapped its whole answer in.
//
// Agents often label an answer ````markdown` and fence the lot. A fence
// labelled markdown, md, text, or nothing is unwrapped, and a fence labelled
// with a real language is left alone because it is code.
func unwrapProseFence(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "```") {
		return text
	}

	lines := strings.Split(trimmed, "\n")
	if len(lines) < 3 {
		return text
	}

	open, language, ok := fenceLine(strings.TrimSpace(lines[0]))
	if !ok {
		return text
	}
	closing, rest, ok := fenceLine(strings.TrimSpace(lines[len(lines)-1]))
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

// fenceLine reads a code fence, returning its length, its language, and the
// rest of the line after it.
func fenceLine(line string) (int, string, bool) {
	length := 0
	for length < len(line) && line[length] == '`' {
		length++
	}
	if length < 3 {
		return 0, "", false
	}
	return length, strings.TrimSpace(line[length:]), true
}

// fence reports whether a line is only a fence.
func fence(line string) bool {
	length, _, ok := fenceLine(line)
	return ok && length >= 3
}

// escapeHTML makes text safe inside an HTML message.
func escapeHTML(text string) string {
	return htmlEscaper.Replace(text)
}

var htmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
)

// escapeAttr makes a URL safe inside an attribute.
func escapeAttr(url string) string {
	return strings.ReplaceAll(escapeHTML(url), `"`, "&quot;")
}

// boldPattern, italicPattern, strikePattern, headingPattern, and linkPattern
// are the pieces of markdown prose that become HTML. They run on escaped text,
// so a tag they produce is the only markup in the message.
var (
	boldPattern    = regexp.MustCompile(`\*\*([^*]+)\*\*|__([^_]+)__`)
	italicPattern  = regexp.MustCompile(`\*([^*\n]+)\*|(^|[^A-Za-z0-9])_([^_\n]+)_([^A-Za-z0-9]|$)`)
	strikePattern  = regexp.MustCompile(`~~([^~]+)~~`)
	headingPattern = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	linkPattern    = regexp.MustCompile(`(!?)\[([^\]]+)\]\(([^)\s]+)\)`)
)

// imagePattern matches a markdown image: ![alt](target).
var imagePattern = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)\)`)

// extractImages pulls the markdown images out of a text, returning what is
// left and the pictures found.
//
// An http(s) URL or a file path is pulled out: the URL becomes a photo the
// platform fetches, and the path becomes an upload. A target with another
// scheme — a data: URI, say — cannot be sent that way, so it stays in the
// text untouched rather than disappearing. An image inside code is not an
// image: ![alt](url) in a fence or a span is markup an agent is showing, not
// a picture it is sending.
func extractImages(text string) (string, []OutboundImage) {
	text = unwrapProseFence(text)
	ranges := codeRanges(text)
	var images []OutboundImage
	var b strings.Builder
	last := 0
	for _, m := range imagePattern.FindAllStringSubmatchIndex(text, -1) {
		if inRanges(ranges, m[0]) || !isPhotoTarget(text[m[4]:m[5]]) {
			continue
		}
		b.WriteString(text[last:m[0]])
		images = append(images, OutboundImage{Alt: text[m[2]:m[3]], Target: text[m[4]:m[5]]})
		last = m[1]
	}
	b.WriteString(text[last:])
	return strings.TrimSpace(b.String()), images
}

// codeRanges returns the byte ranges text marks as code: a fenced block, a
// line opening with three or more backticks closed by a line of at least as
// many, and an inline span, a run of backticks closed by an equal run. An
// unclosed fence or span runs to the end.
func codeRanges(text string) [][2]int {
	var ranges [][2]int
	i := 0
	for i < len(text) {
		if text[i] != '`' {
			i++
			continue
		}
		run := backtickRun(text, i)
		switch {
		case run >= 3 && atLineStart(text, i):
			end := fenceEnd(text, i+run, run)
			ranges = append(ranges, [2]int{i, end})
			i = end
		default:
			if end := spanEnd(text, i+run, run); end >= 0 {
				ranges = append(ranges, [2]int{i, end})
				i = end
			} else {
				i += run
			}
		}
	}
	return ranges
}

// backtickRun is the length of the run of backticks starting at i.
func backtickRun(text string, i int) int {
	j := i
	for j < len(text) && text[j] == '`' {
		j++
	}
	return j - i
}

// atLineStart reports whether only spaces stand between i and the line's
// start.
func atLineStart(text string, i int) bool {
	for k := i - 1; k >= 0 && text[k] != '\n'; k-- {
		if text[k] != ' ' {
			return false
		}
	}
	return true
}

// fenceEnd returns the index just past the line holding a closing fence of at
// least run backticks, or the end of the text when the fence never closes.
func fenceEnd(text string, pos, run int) int {
	for pos < len(text) {
		end := strings.IndexByte(text[pos:], '\n')
		lineEnd := len(text)
		if end >= 0 {
			lineEnd = pos + end
		}
		k := pos
		for k < lineEnd && text[k] == ' ' {
			k++
		}
		if r := backtickRun(text, k); r >= run {
			if strings.TrimRight(text[k+r:lineEnd], " ") == "" {
				if end < 0 {
					return len(text)
				}
				return lineEnd + 1
			}
		}
		if end < 0 {
			return len(text)
		}
		pos = lineEnd + 1
	}
	return len(text)
}

// spanEnd returns the index just past the next run of exactly run backticks
// after pos, or -1 when the span never closes.
func spanEnd(text string, pos, run int) int {
	for p := pos; p < len(text); {
		if text[p] != '`' {
			p++
			continue
		}
		r := backtickRun(text, p)
		if r == run {
			return p + r
		}
		p += r
	}
	return -1
}

// inRanges reports whether pos falls inside one of the sorted ranges.
func inRanges(ranges [][2]int, pos int) bool {
	for _, r := range ranges {
		if pos < r[0] {
			return false
		}
		if pos < r[1] {
			return true
		}
	}
	return false
}

// isPhotoTarget reports whether an image target is a picture reference: a URL
// the platform fetches, or a file path the transport uploads.
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

// chunk splits text so every piece fits in one Telegram message.
//
// Telegram refuses a text over its limit, and refuses the whole message, so a
// long answer would otherwise never arrive. The text is split on line
// boundaries where it can be, and a single line longer than the limit is
// split on the limit.
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
			cut := limit
			// A cut inside a multi-byte rune produces an invalid-UTF-8 chunk,
			// which Telegram refuses as surely as a long one.
			for cut > 0 && !utf8.ValidString(line[:cut]) {
				cut--
			}
			if cut == 0 {
				cut = limit
			}
			cut = avoidMarkupCut(line, cut)
			chunks = append(chunks, line[:cut])
			line = line[cut:]
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

// avoidMarkupCut moves a split point that lands inside a tag or an entity
// back to where the construct starts. A '<' or '&' cut in two arrives as
// literal text, because the piece that opens it never closes.
//
// The generated markup escapes every literal '<' and '&', so backing off to
// the last one is always safe; on unformatted text it only moves the cut.
func avoidMarkupCut(line string, cut int) int {
	if cut <= 0 || cut >= len(line) {
		return cut
	}
	if lt := strings.LastIndexByte(line[:cut], '<'); lt >= 0 && strings.LastIndexByte(line[:cut], '>') < lt {
		if lt > 0 {
			return lt
		}
		return cut
	}
	if amp := strings.LastIndexByte(line[:cut], '&'); amp > 0 && strings.LastIndexByte(line[:cut], ';') < amp {
		return amp
	}
	return cut
}

// tagPattern matches the markup markdown() and the renderers emit.
var tagPattern = regexp.MustCompile(`<(/?)(b|i|s|u|code|pre|a|blockquote)(\s[^>]*)?>`)

// openTag is a tag a split left open: its name to close it and its text to
// reopen it, which for an anchor carries the href.
type openTag struct {
	name string
	text string
}

// balanceChunks makes every chunk well-formed HTML. A split at a line
// boundary can land inside a <pre> or an emphasis span, and Telegram refuses
// malformed markup — the client then reposts the text plain and a user reads
// the tags literally. Each chunk gets the still-open tags closed, and the
// next chunk reopens them.
func balanceChunks(chunks []string) []string {
	balanced := make([]string, 0, len(chunks))
	var open []openTag
	for _, part := range chunks {
		if len(open) > 0 {
			var reopened strings.Builder
			for _, tag := range open {
				reopened.WriteString(tag.text)
			}
			part = reopened.String() + part
		}
		open = tagStack(part)
		balanced = append(balanced, closeTags(part, open))
	}
	return balanced
}

// closeTags appends the closers for tags a chunk leaves open.
func closeTags(part string, stack []openTag) string {
	var b strings.Builder
	b.WriteString(part)
	for i := len(stack) - 1; i >= 0; i-- {
		b.WriteString("</")
		b.WriteString(stack[i].name)
		b.WriteString(">")
	}
	return b.String()
}

// tagStack is the list of tags still open after a chunk.
func tagStack(part string) []openTag {
	var stack []openTag
	for _, match := range tagPattern.FindAllStringSubmatch(part, -1) {
		if match[1] == "" {
			stack = append(stack, openTag{name: match[2], text: match[0]})
			continue
		}
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].name == match[2] {
				stack = stack[:i]
				break
			}
		}
	}
	return stack
}

// configMessage renders the selectors an agent declared.
//
// The agent decides what it offers, so this renders whatever it sent rather
// than knowing what a model is.
func configMessage(config v1.SessionConfigResult) Message {
	if len(config.Options) == 0 {
		return textMessage(noSettingsText(config.AgentID))
	}

	var text strings.Builder
	fmt.Fprintf(&text, "<b>%s settings</b>", escapeHTML(config.AgentID))
	for _, option := range config.Options {
		current := strings.Trim(string(option.CurrentValue), `"`)

		fmt.Fprintf(&text, "\n\n<b>%s</b>", escapeHTML(option.Name))
		if current != "" {
			fmt.Fprintf(&text, " — now <code>%s</code>", escapeHTML(current))
		}

		for _, value := range option.Options {
			mark := "•"
			if value.Value == current {
				mark = "• ✅"
			}
			fmt.Fprintf(&text, "\n%s <code>%s</code> — %s", mark, escapeHTML(value.Value), escapeHTML(value.Name))
		}

		if len(option.Options) > 0 {
			fmt.Fprintf(&text, "\nSwitch with: <code>model %s &lt;name&gt;</code>", escapeHTML(option.ID))
		}
	}
	return Message{Text: text.String(), ParseMode: "HTML"}
}

// noSettingsText explains a declaration that is empty.
func noSettingsText(agentID string) string {
	who := "This agent"
	if agentID != "" {
		who = "<b>" + escapeHTML(agentID) + "</b>"
	}
	return fmt.Sprintf(
		"%s declares no session settings over ACP.\n\n"+
			"Hive renders the settings an agent declares for a session. An agent that "+
			"exposes its model picker only through the legacy ACP models/modes fields "+
			"declares nothing here: Hive reads configOptions, the surface that supersedes "+
			"them.\n\n"+
			"Change the model with the agent's own command, or hand off to an agent that "+
			"declares settings.",
		who)
}

func statusMessage(status *v1.SessionStatusResult) Message {
	var text strings.Builder
	fmt.Fprintf(&text, "<b>Session</b> <code>%s</code>\nState: <code>%s</code>",
		escapeHTML(status.SessionID), escapeHTML(status.State))
	for _, run := range status.Runs {
		fmt.Fprintf(&text, "\n• <code>%s</code> %s on <code>%s</code> — %s",
			escapeHTML(run.RunID), escapeHTML(run.AgentID), escapeHTML(run.NodeID), escapeHTML(run.State))
	}

	if settings := settingParts(status.Settings); len(settings) > 0 {
		fmt.Fprintf(&text, "\n\n<b>Settings</b>: %s", strings.Join(settings, " · "))
	}
	if status.ToolCalls > 0 {
		fmt.Fprintf(&text, "\n<b>Tool calls</b>: %d", status.ToolCalls)
	}
	if status.Usage != nil {
		if parts := usageParts(*status.Usage); len(parts) > 0 {
			fmt.Fprintf(&text, "\n<b>Tokens</b>: %s", strings.Join(parts, " · "))
		}
	}
	return Message{Text: text.String(), ParseMode: "HTML"}
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
		parts = append(parts, fmt.Sprintf("%s <code>%s</code>", escapeHTML(id), escapeHTML(settings[id])))
	}
	return parts
}

// usageParts is what a usage report says, in words.
func usageParts(usage v1.Usage) []string {
	parts := make([]string, 0, 3)
	if turn := usage.Turn(); turn > 0 {
		parts = append(parts, fmt.Sprintf("<b>%s tokens</b>", commas(turn)))
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
	return Message{Text: "<b>Recent activity</b>\n<pre>" + escapeHTML(strings.Join(lines, "\n")) + "</pre>", ParseMode: "HTML"}
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

	var text strings.Builder
	text.WriteString("<b>Sessions</b>")
	for _, session := range list.Sessions {
		fmt.Fprintf(&text, "\n• <code>%s</code> — %s, %d run(s)",
			escapeHTML(session.SessionID), escapeHTML(session.State), session.Runs)
	}
	return Message{Text: text.String(), ParseMode: "HTML"}
}

// HelpMessage renders the transport's own catalog.
//
// The catalog belongs to the transport, so this needs no round trip to the
// core.
func HelpMessage() Message {
	var text strings.Builder
	text.WriteString("<b>Commands</b>")
	for _, entry := range Catalog() {
		fmt.Fprintf(&text, "\n• <code>/%s</code> — %s", entry.Command, escapeHTML(entry.Description))
	}
	text.WriteString("\n\nAnything else is a prompt.\n" +
		"When an agent asks permission, tap a button or reply `allow`, `deny`, or a number.")
	return Message{Text: text.String(), ParseMode: "HTML"}
}

func agentListMessage(list *v1.AgentListResult) Message {
	var text strings.Builder
	text.WriteString("<b>Agents</b>")
	for _, agent := range list.Agents {
		fmt.Fprintf(&text, "\n• <code>%s</code> (%s)", escapeHTML(agent.ID), escapeHTML(agent.Protocol))
	}
	return Message{Text: text.String(), ParseMode: "HTML"}
}

func nodeListMessage(list *v1.NodeListResult) Message {
	if len(list.Nodes) == 0 {
		return textMessage("No nodes.")
	}

	var text strings.Builder
	text.WriteString("<b>Nodes</b>")
	for _, node := range list.Nodes {
		state := "offline"
		if node.Connected {
			state = "connected"
		}
		fmt.Fprintf(&text, "\n• <code>%s</code> %s — %d executions",
			escapeHTML(node.NodeID), state, node.Executions)
	}
	return Message{Text: text.String(), ParseMode: "HTML"}
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

// permissionDecision reads a text reply as a decision on a pending request.
//
// The worded reply is the fallback: buttons answer most requests, but a typed
// answer still works when there are no buttons or none render. The grammar is
// deliberately narrow — one word, or the number of an option — so a sentence
// that happens to contain "allow" is still a prompt rather than an accidental
// authorization.
func permissionDecision(text string, pending *permissionRequest) (optionID string, approved bool, ok bool) {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	if len(fields) != 1 {
		return "", false, false
	}

	switch fields[0] {
	case "allow", "approve", "yes":
		return firstOption(pending, allows), true, true
	case "deny", "reject", "no":
		return firstOption(pending, rejects), false, true
	}

	// A numbered reply selects one of the agent's own options.
	n, err := strconv.Atoi(fields[0])
	if err != nil || n < 1 || n > len(pending.Options) {
		return "", false, false
	}
	option := pending.Options[n-1]
	return option.OptionID, allows(option.Kind), true
}

// callbackDecision reads a button press as a decision on a pending request.
func callbackDecision(data string, pending *permissionRequest) (optionID string, approved bool, ok bool) {
	index, found := strings.CutPrefix(data, "perm:")
	if !found {
		return "", false, false
	}
	n, err := strconv.Atoi(index)
	if err != nil || n < 0 || n >= len(pending.Options) {
		return "", false, false
	}
	option := pending.Options[n]
	return option.OptionID, allows(option.Kind), true
}

// firstOption is the first option matching a predicate, or empty.
func firstOption(pending *permissionRequest, match func(string) bool) string {
	for _, option := range pending.Options {
		if match(option.Kind) {
			return option.OptionID
		}
	}
	return ""
}

// allows and rejects read an option's kind.
func allows(kind string) bool { return strings.HasPrefix(kind, "allow") }

func rejects(kind string) bool {
	return strings.HasPrefix(kind, "deny") || strings.HasPrefix(kind, "reject")
}
