package main

import (
	"os"
	"path/filepath"
	"runtime"
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
