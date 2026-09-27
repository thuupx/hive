package logging

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// A record is rendered readably: the clock is kept, the level is its own column,
// and the message keeps its own words.
func TestRenderLine(t *testing.T) {
	line := `time=2026-09-27T10:05:06.300+07:00 level=INFO msg="agent tool" run=run_1 status=in_progress`

	rendered := RenderLine(line)
	if !strings.Contains(rendered, "10:05:06") {
		t.Errorf("the clock was lost: %q", rendered)
	}
	if !strings.Contains(rendered, "INFO") {
		t.Errorf("the level was lost: %q", rendered)
	}
	if !strings.Contains(rendered, `"agent tool" run=run_1 status=in_progress`) {
		t.Errorf("the message was rewritten: %q", rendered)
	}
	if strings.Contains(rendered, "time=") {
		t.Errorf("the raw timestamp survived: %q", rendered)
	}
}

// A line that is not a record is left to speak for itself: a plugin's own message
// on stderr is not a slog line.
func TestRenderLineLeavesPlainLines(t *testing.T) {
	for _, line := range []string{
		"something else entirely",
		`hive-plugin-zalo: zalo: a bot token is required`,
		`{"time":"2026-09-27T10:05:06Z","level":"INFO","msg":"json"}`,
	} {
		if got := RenderLine(line); got != line {
			t.Errorf("RenderLine(%q) = %q, want it unchanged", line, got)
		}
	}
}

// Only a terminal gets the rendering; a pipe or a redirect gets the record as the
// log file has it.
func TestTerminalOnlyRendersAFile(t *testing.T) {
	var buf bytes.Buffer
	if got := Terminal(&buf); got != &buf {
		t.Error("a non-file writer should be returned unchanged")
	}

	// A file that is not a terminal — the common case in a test — is unchanged
	// too, so the same rendering cannot depend on where it was written.
	file := t.TempDir() + "/log"
	f, err := os.Create(file)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()

	if got := Terminal(f); got != f {
		t.Error("a file that is not a terminal should be returned unchanged")
	}
}

// A terminal writer renders complete lines and holds the rest until Flush.
func TestTerminalWriterRendersByLine(t *testing.T) {
	var out bytes.Buffer
	w := &terminalWriter{out: &out}

	record := "time=2026-09-27T10:05:06.300+07:00 level=WARN msg=\"careful\"\n"
	if _, err := w.Write([]byte(record)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(out.String(), "WARN") || strings.Contains(out.String(), "time=") {
		t.Errorf("the record was not rendered: %q", out.String())
	}

	// A trailing line without a newline is held, then flushed.
	out.Reset()
	if _, err := w.Write([]byte("time=2026-09-27T10:05:07.000+07:00 level=INFO msg=partial")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("an incomplete line should be held, got %q", out.String())
	}
	w.Flush()
	if !strings.Contains(out.String(), "partial") {
		t.Errorf("the held line was not flushed: %q", out.String())
	}
}
