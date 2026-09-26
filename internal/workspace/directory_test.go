package workspace_test

import (
	"errors"
	"testing"

	"github.com/thuupx/hive/internal/workspace"
)

// A run works where the coordinator said, then where the configuration said.
func TestDirectoryPrefersTheNamedLocation(t *testing.T) {
	for _, tc := range []struct {
		requested  string
		configured string
		want       string
	}{
		{"/repos/a", "/repos/b", "/repos/a"},
		{"", "/repos/b", "/repos/b"},
	} {
		got, err := workspace.Directory(tc.requested, tc.configured)
		if err != nil {
			t.Fatalf("Directory(%q, %q): %v", tc.requested, tc.configured, err)
		}
		if got != tc.want {
			t.Errorf("Directory(%q, %q) = %q, want %q", tc.requested, tc.configured, got, tc.want)
		}
	}
}

// A run with no workspace is refused rather than started in whatever directory
// the daemon happens to be in.
//
// Found in a live installation: a daemon started by the system runs in the
// filesystem root, so the agent was told its working directory was `/` — every
// file its user could reach.
func TestDirectoryRefusesARunWithNoWorkspace(t *testing.T) {
	got, err := workspace.Directory("", "")
	if !errors.Is(err, workspace.ErrNoWorkspace) {
		t.Fatalf("Directory(\"\", \"\") = %q, %v; want ErrNoWorkspace", got, err)
	}
	if got != "" {
		t.Errorf("a refused run must have no directory, got %q", got)
	}
}
