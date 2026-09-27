package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A level filter compares the records' own levels, so it does not need a second
// logging format.
func TestLinePassesFiltersByLevel(t *testing.T) {
	info := `time=2026-01-01T00:00:00Z level=INFO msg="started"`
	warn := `time=2026-01-01T00:00:00Z level=WARN msg="careful"`
	debug := `time=2026-01-01T00:00:00Z level=DEBUG msg="detail"`

	cases := map[string]struct {
		line  string
		level string
		want  bool
	}{
		"no filter":              {info, "", true},
		"info shows info":        {info, "info", true},
		"warn hides info":        {info, "warn", false},
		"warn shows warn":        {warn, "warn", true},
		"warn hides debug":       {debug, "warn", false},
		"debug shows everything": {info, "debug", true},
		"error hides warn":       {warn, "error", false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := linePasses(tc.line, tc.level, ""); got != tc.want {
				t.Fatalf("linePasses = %v, want %v", got, tc.want)
			}
		})
	}
}

// A line with no level of its own belongs to a record, and dropping it would
// split one record in half.
func TestLinePassesKeepsALineWithNoLevel(t *testing.T) {
	continuation := "  at github.com/thuupx/hive/internal/control/service.go:123"

	if !linePasses(continuation, "error", "") {
		t.Fatal("a continuation line should survive a level filter")
	}
}

func TestLinePassesFiltersByText(t *testing.T) {
	line := `level=INFO msg="plugin ready" plugin=slack`

	if !linePasses(line, "", "slack") {
		t.Error("a matching line should survive")
	}
	if linePasses(line, "", "telegram") {
		t.Error("a line that does not match should be dropped")
	}
}

// The tail is the last lines, which is what a user wants.
func TestTailLinesKeepsTheEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.log")

	content := ""
	for i := 0; i < 100; i++ {
		content += "line " + itoa(i) + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer file.Close()

	kept, err := tailLines(file, 3, "", "")
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if len(kept) != 3 {
		t.Fatalf("kept %d line(s), want 3", len(kept))
	}
	if kept[2] != "line 99" {
		t.Fatalf("the last line is %q, want the end of the file", kept[2])
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// A log line is rendered readably: the clock is kept, the level is its own
// column, and the message keeps its own words.
func TestSplitLogLine(t *testing.T) {
	ts, level, rest, ok := splitLogLine(
		`time=2026-09-27T10:05:06.300+07:00 level=INFO msg="agent tool" run=run_1 status=in_progress`)
	if !ok {
		t.Fatal("a slog line should be understood")
	}
	if ts != "2026-09-27T10:05:06.300+07:00" {
		t.Errorf("time = %q", ts)
	}
	if level != "INFO" {
		t.Errorf("level = %q", level)
	}
	if rest != `"agent tool" run=run_1 status=in_progress` {
		t.Errorf("rest = %q", rest)
	}

	if got := shortTime(ts); got != "10:05:06" {
		t.Errorf("shortTime = %q, want the clock", got)
	}

	// A line that is not a slog line is left to speak for itself.
	if _, _, _, ok := splitLogLine("something else entirely"); ok {
		t.Error("a plain line should not be parsed as a record")
	}
}
