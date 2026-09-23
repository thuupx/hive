package slack

import (
	"strings"
	"testing"
)

// An agent writes markdown and Slack renders mrkdwn. What looks wrong is the
// difference: literal asterisks, a literal hash, a link shown as its own source.
func TestMrkdwnConvertsWhatSlackRenders(t *testing.T) {
	cases := map[string]string{
		"**bold**":                        "*bold*",
		"__bold__":                        "*bold*",
		"~~gone~~":                        "~gone~",
		"[the docs](https://example.com)": "<https://example.com|the docs>",
		"# Heading":                       "*Heading*",
		"## Smaller":                      "*Smaller*",
		"- an item":                       "• an item",
		"* an item":                       "• an item",
		"  - nested":                      "  • nested",
		"plain text":                      "plain text",
	}

	for input, want := range cases {
		if got := mrkdwn(input); got != want {
			t.Errorf("mrkdwn(%q) = %q, want %q", input, got, want)
		}
	}
}

// A code block is left exactly as it is: the one thing a reader needs from code
// is that nothing was rewritten.
func TestMrkdwnLeavesCodeAlone(t *testing.T) {
	input := "before\n```\n# not a heading\n**not bold**\n- not a bullet\n```\nafter"

	got := mrkdwn(input)

	for _, want := range []string{"# not a heading", "**not bold**", "- not a bullet"} {
		if !strings.Contains(got, want) {
			t.Errorf("code was rewritten: %q is missing from\n%s", want, got)
		}
	}
	// The prose around it is still converted.
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Errorf("the prose was lost:\n%s", got)
	}
}

// Inline code is not read as markup.
func TestMrkdwnLeavesInlineCodeAlone(t *testing.T) {
	got := mrkdwn("run `a ** b` now")

	if !strings.Contains(got, "`a ** b`") {
		t.Fatalf("inline code was rewritten: %q", got)
	}
}

// A message that is too long for one section is split, not refused.
//
// Slack refuses a message whose section is over its limit, and refuses the whole
// message, so a long answer used to never arrive at all.
func TestLongTextIsSplitIntoBlocks(t *testing.T) {
	// A line per sentence, as an answer usually is.
	var b strings.Builder
	for i := 0; i < 400; i++ {
		b.WriteString("This is one line of a long answer that keeps going.\n")
	}
	text := b.String()

	if len(text) <= MaxSectionChars {
		t.Fatalf("the test text is only %d characters", len(text))
	}

	blocks := blocksFor(text)
	if len(blocks) < 2 {
		t.Fatalf("a %d character answer became %d block(s)", len(text), len(blocks))
	}
	for i, block := range blocks {
		if block.Text == nil {
			t.Fatalf("block %d has no text", i)
		}
		if len(block.Text.Text) > MaxSectionChars {
			t.Errorf("block %d is %d characters, over the limit", i, len(block.Text.Text))
		}
	}
}

// A single line longer than a section is split rather than dropped.
func TestOneVeryLongLineIsSplit(t *testing.T) {
	text := strings.Repeat("x", MaxSectionChars*2+10)

	blocks := blocksFor(text)
	if len(blocks) < 2 {
		t.Fatalf("a %d character line became %d block(s)", len(text), len(blocks))
	}

	var rebuilt strings.Builder
	for _, block := range blocks {
		rebuilt.WriteString(block.Text.Text)
	}
	if rebuilt.Len() != len(text) {
		t.Errorf("the line lost %d characters", len(text)-rebuilt.Len())
	}
}

// Slack takes a bounded number of blocks, so a very long answer is truncated
// rather than refused: a message that arrives shortened beats one that never
// arrives.
func TestAVeryLongAnswerIsTruncatedNotRefused(t *testing.T) {
	text := strings.Repeat("a line of text that repeats\n", MaxSectionChars*MaxBlocks/25)

	blocks := blocksFor(text)
	if len(blocks) > MaxBlocks {
		t.Fatalf("blocks = %d, over the limit of %d", len(blocks), MaxBlocks)
	}
	last := blocks[len(blocks)-1].Text.Text
	if !strings.Contains(last, "longer than Slack accepts") {
		t.Error("a truncated answer should say so")
	}
}

// A message needs fallback text: it is what a notification shows and what a
// search matches.
func TestFallbackTextIsNeverEmpty(t *testing.T) {
	if got := fallbackText(""); got == "" {
		t.Error("fallback text must not be empty")
	}
	if got := fallbackText("  \n \n"); got == "" {
		t.Error("fallback text must not be empty for whitespace")
	}
}
