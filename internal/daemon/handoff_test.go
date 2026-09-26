package daemon_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/thuupx/hive/internal/agent"
	"github.com/thuupx/hive/internal/daemon"
	"github.com/thuupx/hive/internal/handoff"
	"github.com/thuupx/hive/internal/node"
	"github.com/thuupx/hive/internal/plugin"
	"github.com/thuupx/hive/internal/session"
	"github.com/thuupx/hive/internal/storage"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

const targetAgent = "other-agent"

// waitForRunStarted waits until a run has an execution.
//
// `starting` means the execution exists and no turn has run yet, which is not the
// same as working.
func waitForRunStarted(t *testing.T, store *storage.Store, runID string) {
	t.Helper()
	ctx := context.Background()

	waitFor(t, "the run to start", func() bool {
		run, err := store.GetAgentRun(ctx, runID)
		return err == nil && run.State != agent.StateCreated
	})
}

// setRunState drives a run to a state and persists it.
func setRunState(t *testing.T, store *storage.Store, runID string, state agent.State) {
	t.Helper()
	ctx := context.Background()

	run, err := store.GetAgentRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if err := run.Transition(state); err != nil {
		t.Fatalf("transition to %s: %v", state, err)
	}
	if err := store.WriteTx(ctx, func(tx storage.Execer) error {
		return store.UpdateAgentRun(ctx, tx, run)
	}); err != nil {
		t.Fatalf("UpdateAgentRun: %v", err)
	}
}

// finishTurn drives a run to completed and persists it.
//
// A handoff is normally asked for between turns, so a test that hands off has to
// end the turn first. Handing off mid-turn is refused on purpose.
func finishTurn(t *testing.T, store *storage.Store, runID string) {
	t.Helper()
	ctx := context.Background()

	run, err := store.GetAgentRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if run.State.IsTerminal() {
		return
	}
	if err := run.Transition(agent.StateCompleted); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := store.WriteTx(ctx, func(tx storage.Execer) error {
		return store.UpdateAgentRun(ctx, tx, run)
	}); err != nil {
		t.Fatalf("UpdateAgentRun: %v", err)
	}
}

// agentSpecFor runs the test agent under a given plugin identity, so a handoff
// between two agents can be exercised with one test binary.
// failingAgentSpecFor runs the test agent so that its start always fails, which
// makes the failed-target path reachable.
func failingAgentSpecFor(id string) plugin.Spec {
	spec := agentSpecFor(id)
	spec.Env = append(spec.Env, "HIVE_TEST_AGENT_FAIL_START=1")
	return spec
}

func agentSpecFor(id string) plugin.Spec {
	return plugin.Spec{
		ID:      id,
		Type:    v1.PluginTypeAgent,
		Version: "test",
		// os.Args[0] is the test binary, which doubles as the plugin process.
		Command:      []string{os.Args[0]},
		Env:          []string{"HIVE_TEST_AGENT=1", "HIVE_TEST_AGENT_ID=" + id},
		MaxRestarts:  1,
		RestartDelay: 50 * time.Millisecond,
	}
}

// startStackWithAgents brings up a coordinator and a node running every named
// agent.
func startStackWithAgents(t *testing.T, agents ...string) (*storage.Store, *v1.Peer) {
	t.Helper()

	specs := make([]plugin.Spec, 0, len(agents))
	for _, id := range agents {
		specs = append(specs, agentSpecFor(id))
	}
	return startStackWithAgentSpecs(t, agents, specs)
}

// startStackWithAgentSpecs is startStackWithAgents with explicit plugin specs.
func startStackWithAgentSpecs(t *testing.T, agents []string, specs []plugin.Spec) (*storage.Store, *v1.Peer) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	coordinatorStore := openCoordinatorStore(t)

	cert, err := node.LoadOrCreateCertificate(t.TempDir())
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}

	socketPath := shortSocketPath(t)
	coordinator, err := daemon.NewCoordinator(daemon.CoordinatorOptions{
		Store:         coordinatorStore,
		Listen:        "127.0.0.1:0",
		Certificate:   cert,
		Agents:        agents,
		DefaultAgent:  agents[0],
		ControlSocket: socketPath,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	if err := coordinator.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })

	nodeStore := openNodeStore(t)
	executionNode, err := daemon.NewNode(daemon.NodeOptions{
		Store:   nodeStore,
		NodeID:  testNodeID,
		Version: "test",
		// A run must have a workspace, so the tests give it one.
		WorkspaceDir:   t.TempDir(),
		LeaseSeconds:   30,
		CoordinatorURL: coordinator.URL(),
		TLSConfig:      node.ClientTLSConfig(cert),
		AgentPlugins:   specs,
		AgentPluginID:  agents[0],
	})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	nodeCtx, stopNode := context.WithCancel(ctx)
	t.Cleanup(stopNode)
	go func() { _ = executionNode.Run(nodeCtx) }()

	waitFor(t, "the node to connect", func() bool {
		n, ok := coordinator.NodeServer().Node(testNodeID)
		return ok && n.Connected() && len(n.AgentIDs()) == len(agents)
	})

	return coordinatorStore, dialControl(t, socketPath)
}

// A handoff creates a new AgentRun rather than migrating an agent process, and the
// two runtime session ids stay distinct.
func TestHandoffTransfersTheSession(t *testing.T) {
	ctx := context.Background()
	store, peer := startStackWithAgents(t, testAgent, targetAgent)

	var created v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, v1.SessionCreateParams{
		CommandID: "cmd_1",
		AgentID:   testAgent,
	}, &created); err != nil {
		t.Fatalf("session.create: %v", err)
	}

	waitFor(t, "the source run to start", func() bool {
		run, err := store.GetAgentRun(ctx, created.RunID)
		return err == nil && run.RuntimeSessionID == "agent-session-"+testAgent
	})

	// The handoff is asked for between turns.
	finishTurn(t, store, created.RunID)

	var transferred v1.SessionHandoffResult
	if err := peer.Call(ctx, v1.MethodSessionHandoff, v1.SessionHandoffParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		AgentID:   targetAgent,
		Summary:   "design completed",
	}, &transferred); err != nil {
		t.Fatalf("session.handoff: %v", err)
	}

	if transferred.State != string(handoff.StateActive) {
		t.Fatalf("handoff state = %q, want active", transferred.State)
	}
	if transferred.TargetRunID == "" || transferred.TargetRunID == created.RunID {
		t.Fatalf("target run = %q, want a new run", transferred.TargetRunID)
	}

	// The two runtime sessions belong to two different runtimes and must never be
	// conflated.
	source, err := store.GetAgentRun(ctx, created.RunID)
	if err != nil {
		t.Fatalf("GetAgentRun source: %v", err)
	}
	target, err := store.GetAgentRun(ctx, transferred.TargetRunID)
	if err != nil {
		t.Fatalf("GetAgentRun target: %v", err)
	}
	if source.RuntimeSessionID != "agent-session-"+testAgent {
		t.Errorf("source runtime session = %q", source.RuntimeSessionID)
	}
	if target.RuntimeSessionID != "agent-session-"+targetAgent {
		t.Errorf("target runtime session = %q", target.RuntimeSessionID)
	}
	if source.RuntimeSessionID == target.RuntimeSessionID {
		t.Fatal("the source and target runtime session ids must be distinct")
	}
	if source.ID == target.ID {
		t.Fatal("a handoff creates a new AgentRun")
	}

	// The routing pointer moves only on activation, so the next prompt reaches the
	// target.
	sess, err := store.GetSession(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.DefaultInteractiveRunID != transferred.TargetRunID {
		t.Fatalf("default run = %q, want the target %q", sess.DefaultInteractiveRunID, transferred.TargetRunID)
	}
	if sess.State != session.StateActive {
		t.Fatalf("session state = %q, want active after the handoff", sess.State)
	}

	// The handoff record is durable and inspectable.
	record, err := store.GetHandoff(ctx, transferred.HandoffID)
	if err != nil {
		t.Fatalf("GetHandoff: %v", err)
	}
	if record.State != handoff.StateActive {
		t.Errorf("handoff state = %q", record.State)
	}
	if record.SourceRunID != created.RunID || record.TargetRunID != transferred.TargetRunID {
		t.Errorf("handoff runs = %q -> %q", record.SourceRunID, record.TargetRunID)
	}

	// The context package was stored as a versioned snapshot.
	snapshot, err := store.LatestSnapshot(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if snapshot.ID != record.ContextSnapshotID {
		t.Errorf("snapshot = %q, want %q", snapshot.ID, record.ContextSnapshotID)
	}
	if snapshot.SchemaVersion < 1 {
		t.Error("a snapshot must identify its context schema version")
	}

	// A later prompt goes to the target without creating a third run.
	var prompted v1.SessionPromptResult
	if err := peer.Call(ctx, v1.MethodSessionPrompt, v1.SessionPromptParams{
		CommandID: "cmd_3",
		SessionID: created.SessionID,
		Text:      "continue",
	}, &prompted); err != nil {
		t.Fatalf("session.prompt: %v", err)
	}
	if prompted.RunID != transferred.TargetRunID || prompted.CreatedRun {
		t.Fatalf("prompted = %+v, want the target run", prompted)
	}
}

// A retry of the same logical operation must not create a second handoff.
func TestHandoffIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store, peer := startStackWithAgents(t, testAgent, targetAgent)

	var created v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, v1.SessionCreateParams{
		CommandID: "cmd_1",
	}, &created); err != nil {
		t.Fatalf("session.create: %v", err)
	}

	waitForRunStarted(t, store, created.RunID)
	finishTurn(t, store, created.RunID)

	params := v1.SessionHandoffParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		AgentID:   targetAgent,
	}

	var first v1.SessionHandoffResult
	if err := peer.Call(ctx, v1.MethodSessionHandoff, params, &first); err != nil {
		t.Fatalf("first handoff: %v", err)
	}

	var second v1.SessionHandoffResult
	if err := peer.Call(ctx, v1.MethodSessionHandoff, params, &second); err != nil {
		t.Fatalf("retry handoff: %v", err)
	}
	if second.HandoffID != first.HandoffID || second.TargetRunID != first.TargetRunID {
		t.Fatalf("retry returned %+v, want the original %+v", second, first)
	}

	records, err := store.ListHandoffs(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("ListHandoffs: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("handoffs = %d, want 1 after a retry", len(records))
	}

	runs, err := store.ListAgentRuns(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("ListAgentRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want the source and the target only", len(runs))
	}
}

// A handoff that cannot start its target must not be reported as a transfer.
func TestHandoffWithoutATargetNodeIsRefused(t *testing.T) {
	ctx := context.Background()
	store, peer := startStackWithAgents(t, testAgent)

	var created v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, v1.SessionCreateParams{
		CommandID: "cmd_1",
	}, &created); err != nil {
		t.Fatalf("session.create: %v", err)
	}

	// The target agent exists in configuration but no node runs it.
	err := peer.Call(ctx, v1.MethodSessionHandoff, v1.SessionHandoffParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		AgentID:   "not-configured",
	}, nil)
	if err == nil {
		t.Fatal("expected the handoff to be refused")
	}
	if e := v1.AsError(err); e.Code != v1.CodeInvalidParams {
		t.Fatalf("code = %d, want %d", e.Code, v1.CodeInvalidParams)
	}

	// Nothing was recorded, and the session still routes to its original run.
	records, err := store.ListHandoffs(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("ListHandoffs: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("handoffs = %d, want none", len(records))
	}

	sess, err := store.GetSession(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.State != session.StateActive {
		t.Fatalf("session state = %q, want active", sess.State)
	}
	if sess.DefaultInteractiveRunID != created.RunID {
		t.Fatalf("default run = %q, want the original %q", sess.DefaultInteractiveRunID, created.RunID)
	}
}

// A handoff between turns is the normal case, and a finished source run is exactly
// when a user asks for one.
//
// Found by handing a real session to another agent: refusing a terminal source run
// made the operation unusable, because a run is finished most of the time.
func TestHandoffFromAFinishedRunIsAllowed(t *testing.T) {
	ctx := context.Background()
	store, peer := startStackWithAgents(t, testAgent, targetAgent)

	var created v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, v1.SessionCreateParams{
		CommandID: "cmd_1",
		AgentID:   testAgent,
	}, &created); err != nil {
		t.Fatalf("session.create: %v", err)
	}

	waitFor(t, "the run to start", func() bool {
		run, err := store.GetAgentRun(ctx, created.RunID)
		return err == nil && run.State == agent.StateStarting
	})

	// Finish the turn, which is the state a user hands off from.
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

	var transferred v1.SessionHandoffResult
	if err := peer.Call(ctx, v1.MethodSessionHandoff, v1.SessionHandoffParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		AgentID:   targetAgent,
	}, &transferred); err != nil {
		t.Fatalf("a finished source run should be handoff-able: %v", err)
	}
	if transferred.State != string(handoff.StateActive) {
		t.Fatalf("state = %q, want active", transferred.State)
	}
	if transferred.SourceRunID != created.RunID {
		t.Errorf("source run = %q, want %q", transferred.SourceRunID, created.RunID)
	}
}

// A handoff while the source is still working is refused, because its turn would
// be abandoned mid-flight.
func TestHandoffWhileTheSourceIsWorkingIsRefused(t *testing.T) {
	ctx := context.Background()
	store, peer := startStackWithAgents(t, testAgent, targetAgent)

	var created v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, v1.SessionCreateParams{
		CommandID: "cmd_1",
		AgentID:   testAgent,
	}, &created); err != nil {
		t.Fatalf("session.create: %v", err)
	}

	// A turn is in flight only while the run is `running`.
	waitForRunStarted(t, store, created.RunID)
	setRunState(t, store, created.RunID, agent.StateRunning)

	err := peer.Call(ctx, v1.MethodSessionHandoff, v1.SessionHandoffParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		AgentID:   targetAgent,
	}, nil)
	if err == nil {
		t.Fatal("expected a working source run to be refused")
	}
	if e := v1.AsError(err); e.Code != v1.CodeConflict {
		t.Fatalf("code = %d, want %d", e.Code, v1.CodeConflict)
	}

	// Nothing was recorded as a transfer.
	records, err := store.ListHandoffs(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("ListHandoffs: %v", err)
	}
	for _, record := range records {
		if record.State.IsUsable() {
			t.Fatalf("handoff %s claims a transfer that did not happen", record.ID)
		}
	}
}

// A handoff whose target cannot start leaves the session usable: it still routes
// to its source run, and it is not stuck in the handoff state.
func TestFailedHandoffLeavesTheSessionUsable(t *testing.T) {
	ctx := context.Background()
	store, peer := startStackWithAgentSpecs(t,
		[]string{testAgent, targetAgent},
		[]plugin.Spec{agentSpecFor(testAgent), failingAgentSpecFor(targetAgent)},
	)

	var created v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, v1.SessionCreateParams{
		CommandID: "cmd_1",
	}, &created); err != nil {
		t.Fatalf("session.create: %v", err)
	}

	// The target agent exists but its command does not, so the start fails after
	// the handoff record and the target run were committed.
	err := peer.Call(ctx, v1.MethodSessionHandoff, v1.SessionHandoffParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		AgentID:   targetAgent,
	}, nil)
	if err == nil {
		t.Fatal("expected the handoff to fail")
	}

	// No handoff is reported as a transfer.
	records, err := store.ListHandoffs(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("ListHandoffs: %v", err)
	}
	for _, record := range records {
		if record.State.IsUsable() {
			t.Fatalf("handoff %s claims a transfer that did not happen", record.ID)
		}
	}
}

// A handoff whose target cannot start must not leave a live run behind.
//
// An orphan run would look like work in progress, and it would keep a workspace
// location looking shared by runs that will never execute.
func TestFailedHandoffTerminatesTheTargetRun(t *testing.T) {
	ctx := context.Background()
	store, peer := startStackWithAgentSpecs(t,
		[]string{testAgent, targetAgent},
		[]plugin.Spec{agentSpecFor(testAgent), failingAgentSpecFor(targetAgent)},
	)

	var created v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, v1.SessionCreateParams{
		CommandID: "cmd_1",
		AgentID:   testAgent,
	}, &created); err != nil {
		t.Fatalf("session.create: %v", err)
	}

	waitForRunStarted(t, store, created.RunID)
	finishTurn(t, store, created.RunID)

	// The target agent is configured but its command is not installed, so the
	// start fails after the handoff record and the target run were committed.
	err := peer.Call(ctx, v1.MethodSessionHandoff, v1.SessionHandoffParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		AgentID:   targetAgent,
	}, nil)
	if err == nil {
		t.Fatal("expected the handoff to fail")
	}

	records, err := store.ListHandoffs(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("ListHandoffs: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("handoffs = %d, want the failed attempt to be inspectable", len(records))
	}
	if records[0].State != handoff.StateFailed {
		t.Fatalf("handoff state = %q, want failed", records[0].State)
	}
	if records[0].State.IsUsable() {
		t.Fatal("a failed handoff must not count as a transfer")
	}

	// The target run is terminated rather than left live.
	target, err := store.GetAgentRun(ctx, records[0].TargetRunID)
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if !target.State.IsTerminal() {
		t.Fatalf("target run state = %q, want a terminal state", target.State)
	}

	// And the session still routes to the source run.
	sess, err := store.GetSession(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.DefaultInteractiveRunID != created.RunID {
		t.Fatalf("default run = %q, want the source %q", sess.DefaultInteractiveRunID, created.RunID)
	}
	if sess.State != session.StateActive {
		t.Fatalf("session state = %q, want active", sess.State)
	}
}
