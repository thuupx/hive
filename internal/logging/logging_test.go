package logging_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/thuupx/hive/internal/logging"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":    slog.LevelDebug,
		"DEBUG":    slog.LevelDebug,
		" info ":   slog.LevelInfo,
		"warn":     slog.LevelWarn,
		"warning":  slog.LevelWarn,
		"error":    slog.LevelError,
		"":         slog.LevelInfo,
		"nonsense": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := logging.ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, "debug", "json")
	log.Info("started", logging.FieldComponent, "coordinator", logging.FieldRole, "auto")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("json handler did not emit json: %v (%q)", err, buf.String())
	}
	if rec[logging.FieldComponent] != "coordinator" {
		t.Errorf("component = %v", rec[logging.FieldComponent])
	}
	if rec["msg"] != "started" {
		t.Errorf("msg = %v", rec["msg"])
	}
}

func TestNewTextFormatFiltersByLevel(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, "warn", "text")
	log.Info("hidden")
	log.Warn("shown")
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Errorf("info record should be filtered: %q", out)
	}
	if !strings.Contains(out, "shown") {
		t.Errorf("warn record should be present: %q", out)
	}
}
