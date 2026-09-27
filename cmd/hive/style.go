package main

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
)

// Styles for the commands whose output a person reads.
//
// lipgloss disables color when the writer is not a terminal, so piping
// `hive doctor` into a file still produces plain text.
var (
	styleOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleFail = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))

	styleDim  = lipgloss.NewStyle().Faint(true)
	styleFix  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styleBold = lipgloss.NewStyle().Bold(true)
)

// maxReportWidth is as wide as a finding's text is allowed to be.
//
// A line the terminal wraps loses its indentation, so the report wraps it first,
// with a hanging indent. Following the terminal is what keeps it from being
// wrapped a second time; stopping at 100 is because a longer line is hard to read
// whatever the terminal offers.
const maxReportWidth = 100

// reportBlockWidth is the width a finding's text is wrapped to.
func reportBlockWidth() int {
	width := maxReportWidth
	if w, _, err := term.GetSize(uintptr(os.Stdout.Fd())); err == nil && w > 0 {
		width = min(w, maxReportWidth)
	}
	return max(width, 40)
}

// detailBlock renders a finding's detail.
func detailBlock(text string) string {
	return renderBlock(indented(styleDim), text)
}

// fixBlock renders a finding's remedy the same way.
func fixBlock(text string) string {
	return renderBlock(indented(styleFix), text)
}

// indented gives a style the report's hanging indent.
//
// The width is the text area, so the indent is taken out of it: a line that is
// wrapped to the terminal's width and then indented is a line the terminal wraps
// again, and the second wrap loses the indent.
func indented(style lipgloss.Style) lipgloss.Style {
	return style.PaddingLeft(reportIndent).Width(reportBlockWidth() - reportIndent)
}

// reportIndent is how far a finding's text is indented.
const reportIndent = 3

// renderBlock wraps text to the report width with a hanging indent.
//
// A width in lipgloss pads every line to it, so the trailing spaces are trimmed:
// they are invisible in a terminal and noise in a pipe.
func renderBlock(style lipgloss.Style, text string) string {
	lines := strings.Split(style.Render(text), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	return strings.Join(lines, "\n")
}

// levelStyle colors a log level, and a severity the same way.
func levelStyle(level string) lipgloss.Style {
	switch strings.ToUpper(level) {
	case "ERROR", "FAIL":
		return styleFail
	case "WARN", "WARNING":
		return styleWarn
	case "DEBUG":
		return styleDim
	default:
		return styleOK
	}
}
