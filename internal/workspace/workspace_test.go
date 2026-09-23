package workspace

import (
	"strings"
	"testing"
)

func TestNewAndValidate(t *testing.T) {
	w := New("ws_1", "piceta")
	if err := w.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	cases := []struct {
		name string
		w    *Workspace
		ok   bool
	}{
		{"valid", New("ws_1", "piceta"), true},
		{"nil", nil, false},
		{"no id", &Workspace{Name: "piceta"}, false},
		{"no name", &Workspace{ID: "ws_1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.w.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

// A single run on a location is not shared, so it produces no warning.
func TestNoWarningForASingleRun(t *testing.T) {
	usages := []Usage{{
		WorkspaceID: "ws_1",
		NodeID:      "node_a",
		Path:        "/tmp/piceta",
		RunIDs:      []string{"run_1"},
	}}

	if warnings := Warnings(usages); len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}
	if usages[0].Shared() {
		t.Error("one run is not a shared location")
	}
}

// Two active runs on one location is a real workflow, so it is a warning and never
// a lock.
func TestWarningForSharedLocation(t *testing.T) {
	usages := []Usage{{
		WorkspaceID: "ws_1",
		NodeID:      "node_a",
		Path:        "/tmp/piceta",
		RunIDs:      []string{"run_2", "run_1"},
	}}

	warnings := Warnings(usages)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %+v, want one", warnings)
	}

	warning := warnings[0]
	if warning.WorkspaceID != "ws_1" || warning.NodeID != "node_a" || warning.Path != "/tmp/piceta" {
		t.Fatalf("warning = %+v", warning)
	}
	// The run list is sorted so the warning is stable across calls.
	if len(warning.RunIDs) != 2 || warning.RunIDs[0] != "run_1" || warning.RunIDs[1] != "run_2" {
		t.Fatalf("run ids = %v", warning.RunIDs)
	}

	message := warning.Message()
	for _, want := range []string{"2 active runs", "/tmp/piceta", "node_a", "run_1"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not mention %q", message, want)
		}
	}
}

func TestWarningsAreStableAcrossCalls(t *testing.T) {
	usages := []Usage{
		{WorkspaceID: "ws_b", NodeID: "node_b", Path: "/b", RunIDs: []string{"r2", "r1"}},
		{WorkspaceID: "ws_a", NodeID: "node_a", Path: "/a", RunIDs: []string{"r3", "r4"}},
		{WorkspaceID: "ws_c", NodeID: "node_c", Path: "/c", RunIDs: []string{"r5"}},
	}

	first := Warnings(usages)
	second := Warnings(usages)

	if len(first) != 2 {
		t.Fatalf("warnings = %d, want 2", len(first))
	}
	for i := range first {
		if first[i].WorkspaceID != second[i].WorkspaceID {
			t.Fatalf("warning order changed: %+v then %+v", first, second)
		}
	}
	// Sorted by workspace id, so ws_a comes before ws_b.
	if first[0].WorkspaceID != "ws_a" {
		t.Fatalf("warnings = %+v", first)
	}
}

func TestLocationIsNotAWorkspacePath(t *testing.T) {
	// The same workspace may live at different paths on different machines, so a
	// location is a property of the pair, not of the workspace.
	loc := Location{WorkspaceID: "ws_1", NodeID: "mac_mini", Path: "/Users/x/piceta"}
	if loc.WorkspaceID == "" || loc.NodeID == "" || loc.Path == "" {
		t.Fatal("a location needs all three fields")
	}

	other := Location{WorkspaceID: "ws_1", NodeID: "macbook", Path: "/Users/y/piceta"}
	if other.Path == loc.Path {
		t.Fatal("the same workspace may live at different paths on different nodes")
	}
}
