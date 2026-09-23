package slack

import (
	"regexp"
	"strings"
)

// Slack's own limits, which a message must respect or it is rejected whole.
//
// They are the reason a long answer used to vanish: the text was put in one
// section block, Slack refused the message for being too long, and nothing was
// posted at all.
const (
	// MaxSectionChars is the limit for one section block's text.
	MaxSectionChars = 2900

	// MaxBlocks is how many blocks one message may carry.
	MaxBlocks = 50

	// MaxMessageChars is the limit for a message's fallback text.
	MaxMessageChars = 40000
)

// mrkdwn converts what an agent writes into what Slack renders.
//
// An agent writes markdown: `**bold**`, `# heading`, `[text](url)`, and `- ` for
// bullets. Slack speaks mrkdwn, which is close enough to look right and different
// enough to look wrong: `**bold**` renders as literal asterisks, a heading renders
// as a literal hash, and a link renders as its own source.
//
// Code is left exactly as it is, because the one thing a reader needs from a code
// block is that nothing was rewritten.
func mrkdwn(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))

	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// A fence toggles verbatim mode, including the fence line itself.
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			out = append(out, line)
			continue
		}
		if inFence {
			out = append(out, line)
			continue
		}

		out = append(out, mrkdwnLine(line))
	}
	return strings.Join(out, "\n")
}

// mrkdwnLine converts one line outside a code fence.
func mrkdwnLine(line string) string {
	// A heading has no mrkdwn equivalent, so it becomes bold.
	if level, rest, ok := heading(line); ok {
		_ = level
		return "*" + inline(rest) + "*"
	}

	// A bullet marker becomes a bullet, keeping its indentation so a nested list
	// still reads as nested.
	if indent, rest, ok := bullet(line); ok {
		return indent + "• " + inline(rest)
	}

	return inline(line)
}

// heading reads an ATX heading, returning its level and text.
func heading(line string) (int, string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "#") {
		return 0, "", false
	}

	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level > 6 || level == len(trimmed) || trimmed[level] != ' ' {
		return 0, "", false
	}
	return level, strings.TrimSpace(trimmed[level:]), true
}

// bullet reads a list marker, returning the indentation and the item text.
func bullet(line string) (string, string, bool) {
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	trimmed := strings.TrimLeft(line, " \t")

	for _, marker := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(trimmed, marker) {
			// A marker with no text after it is not a list item.
			rest := strings.TrimSpace(trimmed[len(marker):])
			if rest == "" {
				return "", "", false
			}
			return indent, rest, true
		}
	}
	return "", "", false
}

// Patterns for the inline conversions.
var (
	// A link, which Slack writes as <url|text>.
	linkPattern = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)

	// Inline code, which is converted around rather than through.
	codePattern = regexp.MustCompile("`[^`]+`")

	// Bold, in both markdown spellings.
	boldAsterisk   = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	boldUnderscore = regexp.MustCompile(`__([^_\n]+)__`)

	// Strikethrough.
	strike = regexp.MustCompile(`~~([^~\n]+)~~`)
)

// inline converts the inline markup on one line.
//
// Inline code is pulled out first and put back afterwards, so a code span
// containing an asterisk is not read as bold.
func inline(line string) string {
	var spans []string
	line = codePattern.ReplaceAllStringFunc(line, func(span string) string {
		spans = append(spans, span)
		return "\x00" + string(rune(len(spans)-1)) + "\x00"
	})

	line = linkPattern.ReplaceAllString(line, "<$2|$1>")
	line = boldAsterisk.ReplaceAllString(line, "*$1*")
	line = boldUnderscore.ReplaceAllString(line, "*$1*")
	line = strike.ReplaceAllString(line, "~$1~")

	for i, span := range spans {
		line = strings.ReplaceAll(line, "\x00"+string(rune(i))+"\x00", span)
	}
	return line
}

// blocksFor turns text into as many section blocks as it needs.
//
// Slack refuses a message whose section text is over the limit, and refuses the
// whole message, so a long answer would otherwise never arrive. The text is split
// on line boundaries where it can be, and a single line longer than a block is
// split on the limit.
func blocksFor(text string) []Block {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	chunks := splitForBlocks(text)
	blocks := make([]Block, 0, len(chunks))
	for _, chunk := range chunks {
		blocks = append(blocks, Block{
			Type: "section",
			Text: &TextObject{Type: "mrkdwn", Text: chunk},
		})
	}
	return blocks
}

// splitForBlocks splits text so every piece fits in a section block.
func splitForBlocks(text string) []string {
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
		// A line longer than a block is split on the limit; nothing else can be
		// done with it, and dropping it would lose the answer.
		for len(line) > MaxSectionChars {
			flush()
			chunks = append(chunks, line[:MaxSectionChars])
			line = line[MaxSectionChars:]
		}

		if current.Len()+len(line)+1 > MaxSectionChars {
			flush()
		}
		if current.Len() > 0 {
			current.WriteString("\n")
		}
		current.WriteString(line)
	}
	flush()

	// Slack takes a bounded number of blocks, so a very long answer is truncated
	// rather than refused. Saying so is better than a message that never arrives.
	if len(chunks) > MaxBlocks {
		chunks = chunks[:MaxBlocks]
		chunks[MaxBlocks-1] = strings.TrimRight(chunks[MaxBlocks-1], "\n") +
			"\n… the answer was longer than Slack accepts in one message."
	}
	return chunks
}

// fallbackText is the plain-text form Slack shows in a notification.
//
// It has its own limit, and a message with no text is not searchable.
func fallbackText(text string) string {
	flat := strings.Join(strings.Fields(text), " ")
	if len(flat) > MaxMessageChars {
		flat = flat[:MaxMessageChars]
	}
	if flat == "" {
		return " "
	}
	return flat
}
