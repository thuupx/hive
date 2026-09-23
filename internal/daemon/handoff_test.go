package daemon_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/thupham/hive/internal/agent"
	"github.com/thupham/hive/internal/daemon"
	"github.com/thupham/hive/internal/handoff"
	"github.com/thupham/hive/internal/node"
	"github.com/thupham/hive/internal/plugin"
	"github.com/thupham/hive/internal/session"
	"github.com/thupham/hive/internal/storage"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

const targetAgent = "other-agent"

// agentSpecFor runs the test agent under a given plugin identity, so a handoff
// between two agents can be exercised with one test binary.
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

	specs := make([]plugin.Spec, 0, len(agents))
	for _, id := range agents {
		specs = append(specs, agentSpecFor(id))
	}

	nodeStore := openNodeStore(t)
	executionNode, err := daemon.NewNode(daemon.NodeOptions{
		Store:          nodeStore,
		NodeID:         testNodeID,
		Version:        "test",
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

// A handoff to an agent that is configured but unreachable fails, and the session
// stays usable.
func TestFailedHandoffLeavesTheSessionUsable(t *testing.T) {
	ctx := context.Background()
	store, peer := startStackWithAgents(t, testAgent, targetAgent)

	var created v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, v1.SessionCreateParams{
		CommandID: "cmd_1",
	}, &created); err != nil {
		t.Fatalf("session.create: %v", err)
	}

	// A source run that is already terminal cannot be handed off.
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

	err = peer.Call(ctx, v1.MethodSessionHandoff, v1.SessionHandoffParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		AgentID:   targetAgent,
	}, nil)
	if err == nil {
		t.Fatal("expected a terminal source run to be refused")
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
