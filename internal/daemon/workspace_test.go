package daemon_test

import (
	"context"
	"testing"

	"github.com/thuupx/hive/internal/agent"
	"github.com/thuupx/hive/internal/client"
	"github.com/thuupx/hive/internal/session"
	"github.com/thuupx/hive/internal/storage"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// A workspace is identity plus a node-local location, and naming one on a session
// is enough.
func TestWorkspaceIsResolvedFromASession(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{
		CommandID: "cmd_1",
		Workspace: "piceta",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	sess, err := store.GetSession(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.WorkspaceID == "" {
		t.Fatal("the session should reference a workspace")
	}

	record, err := store.GetWorkspaceByName(ctx, "piceta")
	if err != nil {
		t.Fatalf("GetWorkspaceByName: %v", err)
	}
	if record.ID != sess.WorkspaceID {
		t.Fatalf("workspace = %q, want %q", record.ID, sess.WorkspaceID)
	}

	// The same name resolves to the same workspace, so a second session joins it
	// instead of creating another.
	second, err := c.CreateSession(ctx, v1.SessionCreateParams{
		CommandID: "cmd_2",
		Workspace: "piceta",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	other, err := store.GetSession(ctx, second.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if other.WorkspaceID != sess.WorkspaceID {
		t.Fatalf("second session workspace = %q, want %q", other.WorkspaceID, sess.WorkspaceID)
	}
}

// Several active runs on one location are a real workflow: Hive warns and does not
// lock.
func TestSharedLocationIsAWarningNotALock(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if _, err := c.CreateWorkspace(ctx, v1.WorkspaceCreateParams{
		CommandID: "cmd_ws",
		Name:      "piceta",
		Locations: map[string]string{testNodeID: "/tmp/piceta"},
	}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	// Two sessions on the same workspace, each with a live run.
	var runIDs []string
	for _, commandID := range []string{"cmd_1", "cmd_2"} {
		created, err := c.CreateSession(ctx, v1.SessionCreateParams{
			CommandID: commandID,
			Workspace: "piceta",
		})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		runIDs = append(runIDs, created.RunID)
	}

	// Both runs are live, because the agent is not installed and the start fails
	// after the run was created.
	waitFor(t, "both runs to exist", func() bool {
		runs, err := store.ListAgentRunsByNode(ctx, testNodeID)
		return err == nil && len(runs) >= 2
	})

	result, err := c.ListWorkspaces(ctx)
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(result.Workspaces) != 1 {
		t.Fatalf("workspaces = %+v", result.Workspaces)
	}
	if result.Workspaces[0].Locations[testNodeID] != "/tmp/piceta" {
		t.Errorf("locations = %v", result.Workspaces[0].Locations)
	}
	if result.Workspaces[0].ActiveRuns < 2 {
		t.Errorf("active runs = %d, want at least 2", result.Workspaces[0].ActiveRuns)
	}

	if len(result.Warnings) != 1 {
		t.Fatalf("warnings = %+v, want one", result.Warnings)
	}
	warning := result.Warnings[0]
	if warning.NodeID != testNodeID || warning.Path != "/tmp/piceta" {
		t.Fatalf("warning = %+v", warning)
	}
	if len(warning.RunIDs) < 2 {
		t.Fatalf("warning run ids = %v, want at least two", warning.RunIDs)
	}

	// A warning never becomes a lock: both runs are still live and reachable.
	for _, runID := range runIDs {
		run, err := store.GetAgentRun(ctx, runID)
		if err != nil {
			t.Fatalf("GetAgentRun %s: %v", runID, err)
		}
		if run.State.IsTerminal() {
			t.Errorf("run %s was terminated by the warning", runID)
		}
	}

	// The session states are untouched too.
	sessions, err := store.ListSessions(ctx, 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	for _, sess := range sessions {
		if sess.State != session.StateActive {
			t.Errorf("session %s state = %q, want active", sess.ID, sess.State)
		}
	}
}

// A finished run does not overlap with anything, so it produces no warning.
func TestTerminalRunDoesNotShareALocation(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if _, err := c.CreateWorkspace(ctx, v1.WorkspaceCreateParams{
		CommandID: "cmd_ws",
		Name:      "piceta",
		Locations: map[string]string{testNodeID: "/tmp/piceta"},
	}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{
		CommandID: "cmd_1",
		Workspace: "piceta",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Drive the run to a terminal state through the domain, then persist it.
	run, err := store.GetAgentRun(ctx, created.RunID)
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if err := run.Transition(agent.StateStarting); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := run.Transition(agent.StateCancelled); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := store.WriteTx(ctx, func(tx storage.Execer) error {
		return store.UpdateAgentRun(ctx, tx, run)
	}); err != nil {
		t.Fatalf("UpdateAgentRun: %v", err)
	}

	result, err := c.ListWorkspaces(ctx)
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("warnings = %+v, want none for a finished run", result.Warnings)
	}
	if result.Workspaces[0].ActiveRuns != 0 {
		t.Errorf("active runs = %d, want 0", result.Workspaces[0].ActiveRuns)
	}
}

// The workspace name is identity, not a path.
func TestWorkspacePathComesFromTheLocation(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	summary, err := c.CreateWorkspace(ctx, v1.WorkspaceCreateParams{
		CommandID: "cmd_ws",
		Name:      "piceta",
		Locations: map[string]string{testNodeID: "/tmp/elsewhere"},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if summary.Locations[testNodeID] != "/tmp/elsewhere" {
		t.Fatalf("locations = %v", summary.Locations)
	}

	locations, err := store.ListWorkspaceLocations(ctx, summary.WorkspaceID)
	if err != nil {
		t.Fatalf("ListWorkspaceLocations: %v", err)
	}
	if len(locations) != 1 || locations[0].NodeID != testNodeID || locations[0].Path != "/tmp/elsewhere" {
		t.Fatalf("locations = %+v", locations)
	}
}
