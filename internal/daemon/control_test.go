package daemon_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/thupham/hive/internal/agent"
	"github.com/thupham/hive/internal/daemon"
	"github.com/thupham/hive/internal/node"
	"github.com/thupham/hive/internal/plugin"
	"github.com/thupham/hive/internal/storage"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// startStack brings up a coordinator with a Control API socket and a node with
// the test agent, then returns a Control API client connected over that socket.
func startStack(t *testing.T) (*daemon.Coordinator, *storage.Store, *v1.Peer) {
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
		Agents:        []string{testAgent},
		DefaultAgent:  testAgent,
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
		Store:          nodeStore,
		NodeID:         testNodeID,
		Version:        "test",
		LeaseSeconds:   30,
		CoordinatorURL: coordinator.URL(),
		TLSConfig:      node.ClientTLSConfig(cert),
		AgentPlugins:   []plugin.Spec{testAgentSpec()},
		AgentPluginID:  testAgent,
	})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	nodeCtx, stopNode := context.WithCancel(ctx)
	t.Cleanup(stopNode)
	go func() { _ = executionNode.Run(nodeCtx) }()

	waitFor(t, "the node to connect", func() bool {
		n, ok := coordinator.NodeServer().Node(testNodeID)
		return ok && n.Connected()
	})

	return coordinator, coordinatorStore, dialControl(t, socketPath)
}

func dialControl(t *testing.T, socketPath string) *v1.Peer {
	t.Helper()

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial the control socket: %v", err)
	}

	peer := v1.NewPeer(v1.NewStream(conn, conn, conn))
	peer.Start()
	t.Cleanup(func() { _ = peer.Close() })
	return peer
}

// shortSocketPath returns a socket path inside a short directory.
//
// A unix socket path is bounded by the platform's sockaddr_un, and a test
// temporary directory is long enough to exceed it.
func shortSocketPath(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "hive")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return filepath.Join(dir, "hive.sock")
}

// TestControlCreatePromptAndReplay drives the Control API the way a client does:
// over the unix socket, with no access to the internals.
func TestControlCreatePromptAndReplay(t *testing.T) {
	ctx := context.Background()
	_, store, peer := startStack(t)

	var created v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, v1.SessionCreateParams{
		CommandID: "cmd_1",
		Workspace: "/tmp/ws",
	}, &created); err != nil {
		t.Fatalf("session.create: %v", err)
	}
	if created.SessionID == "" || created.RunID == "" {
		t.Fatalf("created = %+v", created)
	}
	if created.AgentID != testAgent {
		t.Errorf("agent = %q, want %q", created.AgentID, testAgent)
	}
	if created.NodeID != testNodeID {
		t.Errorf("node = %q, want %q", created.NodeID, testNodeID)
	}

	// The session's default run is the one just created, so the first prompt
	// routes to it rather than creating a second run.
	sess, err := store.GetSession(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.DefaultInteractiveRunID != created.RunID {
		t.Fatalf("default run = %q, want %q", sess.DefaultInteractiveRunID, created.RunID)
	}

	waitFor(t, "the run to record its runtime session", func() bool {
		run, err := store.GetAgentRun(ctx, created.RunID)
		return err == nil && run.State == agent.StateStarting && run.RuntimeSessionID == "agent-session-1"
	})

	var prompted v1.SessionPromptResult
	if err := peer.Call(ctx, v1.MethodSessionPrompt, v1.SessionPromptParams{
		CommandID: "cmd_2",
		SessionID: created.SessionID,
		Text:      "do the thing",
	}, &prompted); err != nil {
		t.Fatalf("session.prompt: %v", err)
	}
	if prompted.RunID != created.RunID {
		t.Errorf("prompted run = %q, want the existing run %q", prompted.RunID, created.RunID)
	}
	if prompted.CreatedRun {
		t.Error("routing must reuse the session's default run")
	}

	// The agent's event became durable with a coordinator-assigned sequence.
	waitFor(t, "the agent event to become durable", func() bool {
		n, err := store.EventCount(ctx, created.SessionID)
		return err == nil && n >= 1
	})

	var replay v1.EventReplayResult
	if err := peer.Call(ctx, v1.MethodSessionEvents, v1.EventReplayParams{
		SessionID:    created.SessionID,
		FromSequence: 0,
	}, &replay); err != nil {
		t.Fatalf("session.events: %v", err)
	}
	if replay.Gap != nil {
		t.Fatalf("unexpected gap: %+v", replay.Gap)
	}
	if len(replay.Events) == 0 {
		t.Fatal("no events were replayed")
	}
	if replay.Events[0].Sequence != 1 {
		t.Errorf("first sequence = %d, want 1", replay.Events[0].Sequence)
	}
	if replay.Events[0].OriginNode != testNodeID {
		t.Errorf("origin node = %q, want %q", replay.Events[0].OriginNode, testNodeID)
	}

	var status v1.SessionStatusResult
	if err := peer.Call(ctx, v1.MethodSessionStatus, v1.SessionStatusParams{
		SessionID: created.SessionID,
	}, &status); err != nil {
		t.Fatalf("session.status: %v", err)
	}
	if status.SessionID != created.SessionID {
		t.Errorf("status session = %q", status.SessionID)
	}
	if len(status.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(status.Runs))
	}
	if status.Runs[0].RuntimeSessionID != "agent-session-1" {
		t.Errorf("runtime session = %q", status.Runs[0].RuntimeSessionID)
	}

	var list v1.SessionListResult
	if err := peer.Call(ctx, v1.MethodSessionList, v1.SessionListParams{}, &list); err != nil {
		t.Fatalf("session.list: %v", err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].SessionID != created.SessionID {
		t.Fatalf("sessions = %+v", list.Sessions)
	}

	// The command record is the async status resource.
	var cmd v1.CommandResult
	if err := peer.Call(ctx, v1.MethodCommandGet, v1.CommandGetParams{CommandID: "cmd_1"}, &cmd); err != nil {
		t.Fatalf("command.get: %v", err)
	}
	if cmd.State != "completed" {
		t.Errorf("command state = %q, want completed", cmd.State)
	}
	if cmd.Method != v1.MethodSessionCreate {
		t.Errorf("command method = %q", cmd.Method)
	}
}

// A retry of the same logical operation must resolve to the existing session,
// not create a second one.
func TestControlCreateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	_, store, peer := startStack(t)

	params := v1.SessionCreateParams{CommandID: "cmd_1", Workspace: "/tmp/ws"}

	var first v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, params, &first); err != nil {
		t.Fatalf("first session.create: %v", err)
	}

	var second v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, params, &second); err != nil {
		t.Fatalf("retry session.create: %v", err)
	}

	if second.SessionID != first.SessionID || second.RunID != first.RunID {
		t.Fatalf("retry returned %+v, want the original %+v", second, first)
	}

	sessions, err := store.ListSessions(ctx, 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1 after a retry", len(sessions))
	}

	runs, err := store.ListAgentRuns(ctx, first.SessionID)
	if err != nil {
		t.Fatalf("ListAgentRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1 after a retry", len(runs))
	}
}

// A duplicate inbound delivery reuses the source id, so it resolves to the same
// command; a deliberate repeat by the user is a new message and a new command.
func TestControlSourceIDDeduplicatesDelivery(t *testing.T) {
	ctx := context.Background()
	_, store, peer := startStack(t)

	var first v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, v1.SessionCreateParams{
		CommandID: "cmd_1",
		SourceID:  "slack:ev:1",
	}, &first); err != nil {
		t.Fatalf("session.create: %v", err)
	}

	// The same transport delivery, deduplicated by the caller to the same
	// command id.
	var duplicate v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, v1.SessionCreateParams{
		CommandID: "cmd_1",
		SourceID:  "slack:ev:1",
	}, &duplicate); err != nil {
		t.Fatalf("duplicate session.create: %v", err)
	}
	if duplicate.SessionID != first.SessionID {
		t.Fatalf("a redelivery created a second session")
	}

	// A new user message is a new source id and therefore a new command.
	var second v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, v1.SessionCreateParams{
		CommandID: "cmd_2",
		SourceID:  "slack:ev:2",
	}, &second); err != nil {
		t.Fatalf("second session.create: %v", err)
	}
	if second.SessionID == first.SessionID {
		t.Fatal("a new delivery must create a new session")
	}

	sessions, err := store.ListSessions(ctx, 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(sessions))
	}
}

func TestControlRejectsUnknownMethod(t *testing.T) {
	ctx := context.Background()
	_, _, peer := startStack(t)

	if err := peer.Call(ctx, "cluster.elect", nil, nil); err == nil {
		t.Fatal("expected an unknown method to be refused")
	} else if e := v1.AsError(err); e.Code != v1.CodeMethodNotFound {
		t.Fatalf("code = %d, want %d", e.Code, v1.CodeMethodNotFound)
	}
}

func TestControlStatusRejectsUnknownSession(t *testing.T) {
	ctx := context.Background()
	_, _, peer := startStack(t)

	err := peer.Call(ctx, v1.MethodSessionStatus, v1.SessionStatusParams{
		SessionID: "sess_missing",
	}, nil)
	if err == nil {
		t.Fatal("expected an unknown session to be rejected")
	}
	if e := v1.AsError(err); e.Code != v1.CodeNotFound {
		t.Fatalf("code = %d, want %d", e.Code, v1.CodeNotFound)
	}
}

func TestControlListsAgentsAndNodes(t *testing.T) {
	ctx := context.Background()
	_, _, peer := startStack(t)

	var agents v1.AgentListResult
	if err := peer.Call(ctx, v1.MethodAgentList, nil, &agents); err != nil {
		t.Fatalf("agent.list: %v", err)
	}
	if len(agents.Agents) != 1 || agents.Agents[0].ID != testAgent {
		t.Fatalf("agents = %+v", agents.Agents)
	}

	var nodes v1.NodeListResult
	if err := peer.Call(ctx, v1.MethodNodeList, nil, &nodes); err != nil {
		t.Fatalf("node.list: %v", err)
	}
	if len(nodes.Nodes) != 1 {
		t.Fatalf("nodes = %+v", nodes.Nodes)
	}
	if !nodes.Nodes[0].Connected {
		t.Error("the node should be reported as connected")
	}
}
