package daemon_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thupham/hive/internal/agent"
	"github.com/thupham/hive/internal/client"
	"github.com/thupham/hive/internal/storage"
	"github.com/thupham/hive/internal/tui"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// TestClientDrivesTheControlAPI exercises the whole client surface the CLI and the
// TUI use, over the real owner-only socket.
func TestClientDrivesTheControlAPI(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)
	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	agents, err := c.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents.Agents) != 1 || agents.Agents[0].ID != testAgent {
		t.Fatalf("agents = %+v", agents.Agents)
	}

	nodes, err := c.ListNodes(ctx)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes.Nodes) != 1 || !nodes.Nodes[0].Connected {
		t.Fatalf("nodes = %+v", nodes.Nodes)
	}

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{CommandID: "cmd_1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if created.SessionID == "" || created.RunID == "" {
		t.Fatalf("created = %+v", created)
	}

	waitFor(t, "the run to start", func() bool {
		run, err := store.GetAgentRun(ctx, created.RunID)
		return err == nil && run.State == agent.StateStarting
	})

	status, err := c.Status(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.SessionID != created.SessionID || len(status.Runs) != 1 {
		t.Fatalf("status = %+v", status)
	}
	if status.Runs[0].RuntimeSessionID == "" {
		t.Error("the run should have recorded its runtime session")
	}

	prompted, err := c.Prompt(ctx, v1.SessionPromptParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		Text:      "do the thing",
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if prompted.RunID != created.RunID || prompted.CreatedRun {
		t.Fatalf("prompted = %+v", prompted)
	}

	// The command record is the async status resource.
	command, err := c.GetCommand(ctx, "cmd_1")
	if err != nil {
		t.Fatalf("GetCommand: %v", err)
	}
	if command.State != "completed" || command.Method != v1.MethodSessionCreate {
		t.Fatalf("command = %+v", command)
	}

	waitFor(t, "the agent event to become durable", func() bool {
		n, err := store.EventCount(ctx, created.SessionID)
		return err == nil && n >= 1
	})

	events, err := c.Replay(ctx, v1.EventReplayParams{SessionID: created.SessionID, FromSequence: 0})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(events.Events) == 0 {
		t.Fatal("no events were replayed")
	}
	if events.Events[0].Sequence != 1 {
		t.Errorf("first sequence = %d, want 1", events.Events[0].Sequence)
	}

	permissions, err := c.ListPermissions(ctx, v1.PermissionListParams{})
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	if len(permissions.Permissions) != 0 {
		t.Fatalf("permissions = %+v, want none", permissions.Permissions)
	}

	sessions, err := c.ListSessions(ctx, 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions.Sessions) != 1 {
		t.Fatalf("sessions = %+v", sessions.Sessions)
	}
}

// The management plane reads through the Control API and renders without
// orchestration logic of its own.
func TestTUIRendersTheManagementPlane(t *testing.T) {
	ctx := context.Background()
	_, _, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if _, err := c.CreateSession(ctx, v1.SessionCreateParams{CommandID: "cmd_1"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	overview, err := tui.Gather(ctx, c)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(overview.Nodes) != 1 || len(overview.Agents) != 1 || len(overview.Sessions) != 1 {
		t.Fatalf("overview = %+v", overview)
	}

	var out bytes.Buffer
	tui.Render(&out, overview)

	rendered := out.String()
	for _, want := range []string{"Hive", "1 of 1 nodes", "1 agents", "1 sessions", testNodeID} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the overview does not mention %q:\n%s", want, rendered)
		}
	}

	status, err := c.Status(ctx, overview.Sessions[0].SessionID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	out.Reset()
	tui.RenderSession(&out, status)
	if !strings.Contains(out.String(), "Runs:") {
		t.Errorf("the session view has no run list:\n%s", out.String())
	}
}

// Run with a zero interval renders once, which is what a scripted check wants.
func TestTUIRunRendersOnce(t *testing.T) {
	ctx := context.Background()
	_, _, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	var out bytes.Buffer
	if err := tui.Run(ctx, c, &out, 0); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "Hive") {
		t.Fatalf("output = %q", out.String())
	}
}

// A cancelled context stops a refreshing dashboard.
func TestTUIRunStopsOnCancel(t *testing.T) {
	_, _, _, socketPath := startStack(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	runCtx, stop := context.WithCancel(ctx)
	var out bytes.Buffer

	done := make(chan error, 1)
	go func() { done <- tui.Run(runCtx, c, &out, 20*time.Millisecond) }()

	time.Sleep(80 * time.Millisecond)
	stop()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
}

// A prompt to a session whose default run has finished starts a new run, and it
// continues with the same agent.
//
// Found by prompting twice against a real agent: routing created the run but
// never started it, so the second prompt had nowhere to go.
func TestPromptAfterTerminalRunStartsANewRun(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{
		CommandID: "cmd_1",
		AgentID:   testAgent,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	waitFor(t, "the first run to start", func() bool {
		run, err := store.GetAgentRun(ctx, created.RunID)
		return err == nil && run.State == agent.StateStarting
	})

	// Finish the run through the domain, then persist it.
	run, err := store.GetAgentRun(ctx, created.RunID)
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if err := run.Transition(agent.StateCompleted); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := store.WriteTx(ctx, func(tx storage.Execer) error {
		return store.UpdateAgentRun(ctx, tx, run)
	}); err != nil {
		t.Fatalf("UpdateAgentRun: %v", err)
	}

	prompted, err := c.Prompt(ctx, v1.SessionPromptParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		Text:      "keep going",
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !prompted.CreatedRun {
		t.Fatal("a finished default run means a new run is created")
	}
	if prompted.RunID == created.RunID {
		t.Fatal("the new run should be a different AgentRun")
	}

	// The new run has an execution. Before the fix it was created and then left
	// behind, so it stayed in `created` with no runtime session.
	waitFor(t, "the new run to have an execution", func() bool {
		next, err := store.GetAgentRun(ctx, prompted.RunID)
		return err == nil && next.State != agent.StateCreated && next.RuntimeSessionID != ""
	})

	next, err := store.GetAgentRun(ctx, prompted.RunID)
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	// It continues the conversation with the agent the session was already using.
	if next.AgentID != testAgent {
		t.Errorf("agent = %q, want %q", next.AgentID, testAgent)
	}
}
