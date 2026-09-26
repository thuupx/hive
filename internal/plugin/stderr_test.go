package plugin

import (
	"log/slog"
	"strings"
	"testing"
)

// A plugin's last words are what explains its exit, so they are kept, and kept
// bounded.
func TestPluginStderrKeepsWhatThePluginSaid(t *testing.T) {
	w := &pluginStderr{log: slog.New(slog.DiscardHandler), plugin: "test"}

	if got := w.last(); got != "" {
		t.Fatalf("last = %q, want empty before anything is written", got)
	}

	// A partial line is not a line yet, and it is still what the plugin said: a
	// plugin that dies mid-write is exactly the case this exists for.
	if _, err := w.Write([]byte("slack: an app token")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := w.last(); !strings.Contains(got, "an app token") {
		t.Fatalf("last = %q, want the partial line", got)
	}

	if _, err := w.Write([]byte(" is required\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := w.last(); got != "slack: an app token is required" {
		t.Fatalf("last = %q, want the completed line once", got)
	}

	// Blank lines are not worth reporting, and a chatty plugin must not be able to
	// grow this without limit.
	for range maxStderrLines * 3 {
		_, _ = w.Write([]byte("\n"))
		_, _ = w.Write([]byte("noise\n"))
	}
	_, _ = w.Write([]byte("the reason\n"))

	got := w.last()
	if !strings.HasSuffix(got, "the reason") {
		t.Fatalf("last = %q, want the most recent line last", got)
	}
	if lines := strings.Count(got, "|") + 1; lines > maxStderrLines {
		t.Fatalf("kept %d lines, want at most %d: %q", lines, maxStderrLines, got)
	}
	if strings.Contains(got, "| |") {
		t.Fatalf("last = %q, want no blank line kept", got)
	}
}
