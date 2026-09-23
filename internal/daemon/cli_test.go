package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
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

// An agent declares its own selectors, and a choice is stored on the session so a
// new run continues with it.
//
// Hive never interprets a selector: it passes the agent's declaration through and
// records the user's choice.
func TestSessionConfigReadsAndRecordsASelector(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{CommandID: "cmd_1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	waitFor(t, "the run to start", func() bool {
		run, err := store.GetAgentRun(ctx, created.RunID)
		return err == nil && run.State != agent.StateCreated
	})

	// Reading returns whatever the agent declared, without Hive understanding it.
	read, err := c.SessionConfig(ctx, v1.SessionConfigParams{SessionID: created.SessionID})
	if err != nil {
		t.Fatalf("SessionConfig read: %v", err)
	}
	if len(read.Options) == 0 {
		t.Fatal("the agent declared no selectors")
	}
	if read.Options[0].ID != "model" || read.Options[0].Category != "model" {
		t.Fatalf("options = %+v", read.Options)
	}

	// Setting a value records it on the session.
	if _, err := c.SessionConfig(ctx, v1.SessionConfigParams{
		SessionID: created.SessionID,
		ConfigID:  "model",
		Value:     "m2",
	}); err != nil {
		t.Fatalf("SessionConfig set: %v", err)
	}

	sess, err := store.GetSession(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got := sess.AgentConfig["model"]; got != "m2" {
		t.Fatalf("recorded model = %q, want m2", got)
	}
}

// A run created by routing becomes the session's default interactive run.
//
// Found in a live conversation: the pointer kept naming a run whose execution was
// long gone, so asking the agent about the session failed with "no live execution".
func TestRoutingCreatedRunBecomesTheDefault(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{CommandID: "cmd_1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	waitForRunStarted(t, store, created.RunID)
	finishTurn(t, store, created.RunID)

	// The default run is finished, so this prompt creates a new one.
	prompted, err := c.Prompt(ctx, v1.SessionPromptParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		Text:      "again",
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !prompted.CreatedRun {
		t.Fatal("a finished default run means a new run")
	}

	sess, err := store.GetSession(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.DefaultInteractiveRunID != prompted.RunID {
		t.Fatalf("default run = %q, want the new run %q", sess.DefaultInteractiveRunID, prompted.RunID)
	}
}

// Reading a selector must not record a value for it.
//
// Found in a live conversation: asking to see the models recorded an empty model,
// and every later run failed because the agent was asked to apply a value that is
// not one.
func TestReadingASelectorDoesNotRecordIt(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{CommandID: "cmd_1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitForRunStarted(t, store, created.RunID)

	// A read names the selector but has no value.
	if _, err := c.SessionConfig(ctx, v1.SessionConfigParams{
		SessionID: created.SessionID,
		ConfigID:  "model",
	}); err != nil {
		t.Fatalf("SessionConfig read: %v", err)
	}

	sess, err := store.GetSession(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if value, recorded := sess.AgentConfig["model"]; recorded {
		t.Fatalf("a read recorded model=%q", value)
	}
}

// A run left in `created` is started when it is prompted.
//
// Found in a live conversation: a restart left a run that was created but never
// launched, and every later prompt routed to it and failed. The session answered
// nothing, for good.
func TestPromptStartsARunThatWasNeverLaunched(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{CommandID: "cmd_1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitForRunStarted(t, store, created.RunID)
	finishTurn(t, store, created.RunID)

	// A run that exists with no execution, as a restart leaves behind.
	var orphan v1.SessionPromptResult
	if err := c.PeerCall(ctx, v1.MethodSessionPrompt, v1.SessionPromptParams{
		CommandID: "cmd_orphan",
		SessionID: created.SessionID,
		Text:      "first",
	}, &orphan); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Force it back to created, which is what a start that never happened looks
	// like. The domain refuses the transition, so the row is written directly.
	if err := store.WriteTx(ctx, func(tx storage.Execer) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE agent_runs SET state = ? WHERE id = ?", string(agent.StateCreated), orphan.RunID)
		return err
	}); err != nil {
		t.Fatalf("reset the run state: %v", err)
	}

	sess, err := store.GetSession(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.DefaultInteractiveRunID != orphan.RunID {
		t.Fatalf("default run = %q, want the orphan", sess.DefaultInteractiveRunID)
	}

	if _, err := c.Prompt(ctx, v1.SessionPromptParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		Text:      "again",
	}); err != nil {
		t.Fatalf("a prompt to a run that was never launched must start it: %v", err)
	}

	waitFor(t, "the run to be started", func() bool {
		run, err := store.GetAgentRun(ctx, orphan.RunID)
		return err == nil && run.State != agent.StateCreated
	})
}

// A continued conversation restores the agent session of the previous run.
//
// Found in a live conversation: a new run created after the previous one finished
// started a fresh agent session, so the agent had lost everything it had been
// told. A conversation belongs to the session even though the agent session
// belongs to a run.
func TestAContinuedRunRestoresThePreviousAgentSession(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{CommandID: "cmd_1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitForRunStarted(t, store, created.RunID)

	// The first run has an agent session of its own.
	first, err := store.GetAgentRun(ctx, created.RunID)
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if first.RuntimeSessionID == "" {
		t.Fatal("the first run should have an agent session")
	}
	finishTurn(t, store, created.RunID)

	// The next prompt creates a run, which must ask for that session back.
	prompted, err := c.Prompt(ctx, v1.SessionPromptParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		Text:      "again",
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !prompted.CreatedRun {
		t.Fatal("a finished default run means a new run")
	}

	waitFor(t, "the new run to start", func() bool {
		run, err := store.GetAgentRun(ctx, prompted.RunID)
		return err == nil && run.State != agent.StateCreated
	})

	run, err := store.GetAgentRun(ctx, prompted.RunID)
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if run.RuntimeSessionID != first.RuntimeSessionID {
		t.Fatalf("runtime session = %q, want the previous run's %q",
			run.RuntimeSessionID, first.RuntimeSessionID)
	}
}

// mustRun reads a run or fails.
func mustRun(t *testing.T, store *storage.Store, runID string) *agent.AgentRun {
	t.Helper()

	run, err := store.GetAgentRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	return run
}

// A run left behind by a restart is marked interrupted, and the conversation
// continues in a new run.
//
// Found in a live conversation: a daemon restart left a run saying it was running
// with nothing behind it, and every later message routed to it and failed. The
// session answered nothing, for good.
func TestARunWithNoExecutionIsInterruptedAndReplaced(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{CommandID: "cmd_1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// The turn has to run first: the agent holds a run while its turn runs, and a
	// run it still holds is live. A turn that has ended is a run it no longer
	// holds, which is exactly the state a restart leaves behind.
	waitForRunStarted(t, store, created.RunID)
	if _, err := c.Prompt(ctx, v1.SessionPromptParams{
		CommandID: "cmd_turn",
		SessionID: created.SessionID,
		Text:      "first",
	}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	waitFor(t, "the turn to finish", func() bool {
		return mustRun(t, store, created.RunID).State.IsTerminal()
	})

	// A run that says it is working, as a restart leaves behind. The agent has no
	// execution for it, which is what makes it a lie. The domain refuses the
	// transition, so the row is written directly.
	if err := store.WriteTx(ctx, func(tx storage.Execer) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE agent_runs SET state = ? WHERE id = ?", string(agent.StateRunning), created.RunID)
		return err
	}); err != nil {
		t.Fatalf("reset the run state: %v", err)
	}
	// The next prompt must not route into it.
	prompted, err := c.Prompt(ctx, v1.SessionPromptParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		Text:      "again",
	})
	if err != nil {
		t.Fatalf("a prompt must recover the session, not fail: %v", err)
	}
	if !prompted.CreatedRun {
		t.Fatal("the prompt should have created a new run")
	}

	// The abandoned run says what happened to it.
	abandoned := mustRun(t, store, created.RunID)
	if abandoned.State != agent.StateInterrupted {
		t.Fatalf("the abandoned run is %q, want interrupted", abandoned.State)
	}
}

// A permission request becomes a durable event, so a transport can ask a user.
//
// Found in a live conversation: two requests were recorded as pending and no event
// was appended, so no transport ever learned there was a question. The agent waited
// for an answer nobody could give, and the user reported the bot as stuck.
func TestAPermissionRequestBecomesAnEvent(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{CommandID: "cmd_1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitForRunStarted(t, store, created.RunID)

	// The test agent asks for permission when the prompt says so, which is the real
	// path: plugin to node to coordinator.
	if _, err := c.Prompt(ctx, v1.SessionPromptParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		Text:      "ask permission please",
	}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	waitFor(t, "the permission to be recorded", func() bool {
		pending, err := store.ListOpenPermissionRequests(ctx, created.SessionID)
		return err == nil && len(pending) == 1
	})

	events, err := store.ReadEvents(ctx, created.SessionID, 0, 200)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}

	var found bool
	for _, ev := range events {
		if ev.Type != v1.EventPermissionRequested {
			continue
		}
		found = true

		// The transport is told how to name the request, because the answer has to
		// name it and a transport must not know how Hive identifies one.
		var payload struct {
			AgentRequestID string `json:"agentRequestId"`
			ToolCall       struct {
				Title string `json:"title"`
			} `json:"toolCall"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("the event payload could not be read: %v", err)
		}
		if payload.AgentRequestID == "" {
			t.Error("the event does not say which request it is")
		}
		if payload.ToolCall.Title != "Run rm -rf" {
			t.Errorf("the tool call was not passed through: %q", payload.ToolCall.Title)
		}
	}

	if !found {
		t.Fatal("no permission event was appended, so no transport can ask")
	}
}

// A request whose agent is gone expires rather than waiting forever.
//
// Found in a live conversation: a plugin restart left a request pending in Hive
// with no agent behind it. It could not be answered, it never stopped being
// shown, and the run it belonged to was already interrupted.
func TestAPermissionExpiresWithItsAgent(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{CommandID: "cmd_1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitForRunStarted(t, store, created.RunID)

	if _, err := c.Prompt(ctx, v1.SessionPromptParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		Text:      "ask permission please",
	}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	waitFor(t, "the permission to be recorded", func() bool {
		pending, err := store.ListOpenPermissionRequests(ctx, created.SessionID)
		return err == nil && len(pending) == 1
	})

	// The turn ends and the agent no longer holds the request, which is what a
	// restart leaves behind.
	waitFor(t, "the turn to finish", func() bool {
		return mustRun(t, store, created.RunID).State.IsTerminal()
	})
	if err := store.WriteTx(ctx, func(tx storage.Execer) error {
		_, err := tx.ExecContext(ctx,
			"UPDATE agent_runs SET state = ? WHERE id = ?", string(agent.StateRunning), created.RunID)
		return err
	}); err != nil {
		t.Fatalf("reset the run state: %v", err)
	}

	// The next prompt reconciles the run, and the request goes with it.
	if _, err := c.Prompt(ctx, v1.SessionPromptParams{
		CommandID: "cmd_3",
		SessionID: created.SessionID,
		Text:      "again",
	}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	waitFor(t, "the request to expire", func() bool {
		pending, err := store.ListOpenPermissionRequests(ctx, created.SessionID)
		return err == nil && len(pending) == 0
	})
}
