package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thuupx/hive/internal/config"
)

// A workspace inside a folder macOS guards is reported, because the dialog that
// follows names Hive rather than the agent — which reads like a bug and is not.
func TestDoctorWarnsAboutAGuardedWorkspace(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the guarded folders are macOS-specific")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}

	cfg := config.Default()
	cfg.WorkspaceDir = filepath.Join(home, "Documents", "Workspace")

	findings := checkGuardedFolder(cfg)
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one", findings)
	}
	if findings[0].level != "warn" {
		t.Errorf("level = %q, want warn: the prompt is expected, not a fault", findings[0].level)
	}
	if findings[0].fix == "" {
		t.Error("a finding without a remedy is a dead end for a user")
	}
}

// A workspace outside them needs no dialog, so there is nothing to say.
func TestDoctorIsQuietAboutAnUnguardedWorkspace(t *testing.T) {
	cfg := config.Default()
	cfg.WorkspaceDir = filepath.Join(t.TempDir(), "project")

	if findings := checkGuardedFolder(cfg); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none", findings)
	}
}

// The default workspace is not inside a guarded folder, which is part of why it
// is the default.
func TestTheDefaultWorkspaceIsNotGuarded(t *testing.T) {
	if findings := checkGuardedFolder(config.Default()); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none", findings)
	}
}

func TestWithin(t *testing.T) {
	for _, tc := range []struct {
		path   string
		folder string
		want   bool
	}{
		{"/a/b/c", "/a/b", true},
		{"/a/b", "/a/b", true},
		{"/a/bc", "/a/b", false},
		{"/a", "/a/b", false},
	} {
		if got := within(tc.path, tc.folder); got != tc.want {
			t.Errorf("within(%q, %q) = %v, want %v", tc.path, tc.folder, got, tc.want)
		}
	}
}

// A report puts what needs fixing first, and fails when something did.
func TestReportOrdersBySeverity(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = write

	reportErr := report([]finding{
		{level: "ok", what: "fine"},
		{level: "warn", what: "careful"},
		{level: "fail", what: "broken"},
	})

	write.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, read); err != nil {
		t.Fatalf("read: %v", err)
	}
	out := buf.String()

	if reportErr == nil {
		t.Error("a failing finding should make the report fail")
	}
	broken := strings.Index(out, "broken")
	careful := strings.Index(out, "careful")
	fine := strings.Index(out, "fine")
	if broken < 0 || careful < 0 || fine < 0 {
		t.Fatalf("the report is missing a finding:\n%s", out)
	}
	if !(broken < careful && careful < fine) {
		t.Errorf("findings are not ordered most urgent first:\n%s", out)
	}
}
