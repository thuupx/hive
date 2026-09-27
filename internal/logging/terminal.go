package logging

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// Rendering a record for a person to watch.
//
// A raw record leads with a timestamp nobody reads and buries the level in the
// middle of key=value pairs. On a terminal the clock is shortened and dimmed, the
// level is colored, and the message keeps its own words. The log file is written
// exactly as before, so a reader or a grep still sees the record.

var (
	styleDim   = lipgloss.NewStyle().Faint(true)
	styleInfo  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleError = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// RenderLine renders one record, readable.
//
// A line that is not a record is returned as it is: a plugin's own message on
// stderr is not a slog line, and pretending to parse it would lose it.
func RenderLine(line string) string {
	ts, level, rest, ok := splitLogLine(line)
	if !ok {
		return line
	}
	return fmt.Sprintf("%s %s %s",
		styleDim.Render(shortTime(ts)),
		levelStyle(level).Render(fmt.Sprintf("%-5s", level)),
		rest)
}

// levelStyle colors a record's level.
func levelStyle(level string) lipgloss.Style {
	switch strings.ToUpper(level) {
	case "ERROR":
		return styleError
	case "WARN", "WARNING":
		return styleWarn
	case "DEBUG":
		return styleDim
	default:
		return styleInfo
	}
}

// splitLogLine reads the time, the level, and the message out of a slog text line.
func splitLogLine(line string) (ts, level, rest string, ok bool) {
	if !strings.HasPrefix(line, "time=") {
		return "", "", "", false
	}
	i := strings.Index(line, " level=")
	if i < 0 {
		return "", "", "", false
	}

	ts = strings.TrimPrefix(line[:i], "time=")
	after := line[i+len(" level="):]

	// msg is written right after level, so the first occurrence is the message.
	if j := strings.Index(after, " msg="); j >= 0 {
		level = after[:j]
		rest = after[j+len(" msg="):]
	} else {
		level = after
	}
	return ts, level, rest, true
}

// shortTime keeps the clock from an RFC 3339 timestamp.
func shortTime(ts string) string {
	if i := strings.IndexByte(ts, 'T'); i >= 0 {
		ts = ts[i+1:]
	}
	if i := strings.IndexAny(ts, ".+"); i >= 0 {
		ts = ts[:i]
	}
	return ts
}

// Terminal wraps a writer so each record is rendered for a person to watch.
//
// A writer that is not a terminal is returned unchanged, so a pipe or a redirect
// gets the record exactly as the log file has it: what a script reads should not
// depend on where it was written. Call Flush when the writer is done with, to
// write a trailing line that never got its newline.
func Terminal(w io.Writer) io.Writer {
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return w
	}
	return &terminalWriter{out: w}
}

// Flush writes anything a terminal writer is holding.
func Flush(w io.Writer) {
	if t, ok := w.(interface{ Flush() }); ok {
		t.Flush()
	}
}

// terminalWriter renders complete lines as they are written.
type terminalWriter struct {
	out io.Writer
	buf []byte
}

func (w *terminalWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)

	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		if _, err := fmt.Fprintln(w.out, RenderLine(string(w.buf[:i]))); err != nil {
			return 0, err
		}
		w.buf = append(w.buf[:0], w.buf[i+1:]...)
	}
	return len(p), nil
}

// Flush renders a line that was written without its newline.
func (w *terminalWriter) Flush() {
	if len(w.buf) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w.out, RenderLine(string(w.buf)))
	w.buf = w.buf[:0]
}
