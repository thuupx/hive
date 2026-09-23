package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thupham/hive/internal/agent"
	"github.com/thupham/hive/internal/daemon"
	"github.com/thupham/hive/internal/node"
	"github.com/thupham/hive/internal/plugin"
	"github.com/thupham/hive/internal/session"
	"github.com/thupham/hive/internal/storage"
	"github.com/thupham/hive/plugins/sdk"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

const (
	testNodeID  = "node_test"
	testSession = "sess_1"
	testRun     = "run_1"
	testAgent   = "test-agent"
)

// The test binary doubles as the agent plugin process, so the whole stack runs
// without a build step or a fixture binary.
func TestMain(m *testing.M) {
	switch {
	case os.Getenv("HIVE_TEST_AGENT") != "":
		runTestAgent()
		return
	case os.Getenv("HIVE_TEST_TRANSPORT") != "":
		runTestTransport()
		return
	default:
		os.Exit(m.Run())
	}
}

func runTestAgent() {
	ctx := context.Background()

	pluginID := os.Getenv("HIVE_TEST_AGENT_ID")
	if pluginID == "" {
		pluginID = testAgent
	}
	runtimeSessionID := "agent-session-" + pluginID

	host, err := sdk.Connect(ctx, sdk.Stdio(), sdk.Options{
		ID:      pluginID,
		Type:    v1.PluginTypeAgent,
		Version: "test",
		Capabilities: []string{
			v1.CapabilityEventWrite,
			v1.CapabilityPermissionWrite,
			v1.CapabilityExecutionWrite,
		},
	})
	if err != nil {
		os.Exit(2)
	}

	var (
		mu        sync.Mutex
		sessions  = map[string]string{}
		responded = map[string]bool{}
	)

	host.Handle(v1.MethodExecutionStart, func(ctx context.Context, params json.RawMessage) (any, error) {
		var req v1.ExecutionStartParams
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, v1.InvalidParams("invalid start")
		}
		if os.Getenv("HIVE_TEST_AGENT_FAIL_START") != "" {
			return nil, v1.Unavailable("agent %s could not be started", pluginID)
		}
		mu.Lock()
		sessions[req.AgentRunID] = req.SessionID
		mu.Unlock()

		_ = host.Call(ctx, v1.MethodExecutionReport, v1.ExecutionReportParams{
			AgentRunID:       req.AgentRunID,
			Generation:       req.Generation,
			State:            "starting",
			RuntimeSessionID: runtimeSessionID,
		}, nil)

		return map[string]any{"runtimeSessionId": runtimeSessionID}, nil
	})

	host.Handle(v1.MethodExecutionPrompt, func(ctx context.Context, params json.RawMessage) (any, error) {
		var req v1.ExecutionPromptParams
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, v1.InvalidParams("invalid prompt")
		}
		mu.Lock()
		sessionID := sessions[req.AgentRunID]
		mu.Unlock()

		_ = host.Call(ctx, v1.MethodEventPublish, v1.PublishEventParams{
			AgentRunID: req.AgentRunID,
			SessionID:  sessionID,
			Type:       "agent.raw",
			Protocol:   "test",
			Method:     "session/update",
			Payload:    json.RawMessage(`{"text":"working"}`),
		}, nil)

		_ = host.Call(ctx, v1.MethodExecutionReport, v1.ExecutionReportParams{
			AgentRunID: req.AgentRunID,
			Generation: req.Generation,
			State:      "terminal",
			Reason:     "end_turn",
		}, nil)

		return map[string]any{"stopReason": "end_turn"}, nil
	})

	host.Handle(v1.MethodExecutionCancel, func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"cancelled": true}, nil
	})

	host.Handle(v1.MethodExecutionConfig, func(_ context.Context, params json.RawMessage) (any, error) {
		var req v1.ExecutionConfigParams
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, v1.InvalidParams("invalid execution.config request")
		}

		current := "m1"
		if req.Value != "" {
			current = req.Value
		}
		return v1.ExecutionConfigResult{Options: []v1.SessionConfigOption{{
			ID:           "model",
			Name:         "Model",
			Category:     v1.ConfigCategoryModel,
			Type:         "select",
			CurrentValue: json.RawMessage(`"` + current + `"`),
			Options: []v1.SessionConfigOptionValue{
				{Value: "m1", Name: "Model 1"},
				{Value: "m2", Name: "Model 2"},
			},
		}}}, nil
	})

	host.Handle(v1.MethodPermissionRespond, func(_ context.Context, params json.RawMessage) (any, error) {
		var req v1.PermissionRespondParams
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, v1.InvalidParams("invalid permission.respond request")
		}
		mu.Lock()
		responded[req.AgentRequestID] = req.Approved
		mu.Unlock()
		return map[string]any{"ok": true}, nil
	})

	// Optional: publish one event immediately, which lets a test exercise the
	// node's buffer without a coordinator to trigger the agent.
	if sessionID := os.Getenv("HIVE_TEST_AGENT_PUBLISH_ON_START"); sessionID != "" {
		_ = host.Call(ctx, v1.MethodEventPublish, v1.PublishEventParams{
			AgentRunID: testRun,
			SessionID:  sessionID,
			Type:       "agent.raw",
			Protocol:   "test",
			Method:     "session/update",
			Payload:    json.RawMessage(`{"text":"at startup"}`),
		}, nil)
	}

	_ = host.Run(ctx)
}

func testAgentSpec() plugin.Spec {
	return testAgentSpecWithEnv()
}

func testAgentSpecWithEnv(extra ...string) plugin.Spec {
	env := append([]string{"HIVE_TEST_AGENT=1"}, extra...)
	return plugin.Spec{
		ID:           testAgent,
		Type:         v1.PluginTypeAgent,
		Version:      "test",
		Command:      []string{os.Args[0]},
		Env:          env,
		MaxRestarts:  1,
		RestartDelay: 50 * time.Millisecond,
	}
}

func openCoordinatorStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatalf("open coordinator store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openNodeStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.OpenNode(context.Background(), filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatalf("open node store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestCoordinatorNodeAgentEndToEnd runs the whole stack in process: a session
// and an AgentRun on the coordinator, an execution started on a node, an agent
// plugin producing an event, and the event becoming durable with an assigned
// stream sequence.
func TestCoordinatorNodeAgentEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	coordinatorStore := openCoordinatorStore(t)

	cert, err := node.LoadOrCreateCertificate(t.TempDir())
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}

	coordinator, err := daemon.NewCoordinator(daemon.CoordinatorOptions{
		Store:       coordinatorStore,
		Listen:      "127.0.0.1:0",
		Certificate: cert,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	if err := coordinator.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer coordinator.Close()

	// A session and a run assigned to the node.
	sess := session.New(testSession)
	if err := coordinatorStore.WriteTx(ctx, func(tx storage.Execer) error {
		return coordinatorStore.InsertSession(ctx, tx, sess)
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	run := agent.New(testRun, testSession, testAgent, "acp")
	run.NodeID = testNodeID
	if err := coordinatorStore.WriteTx(ctx, func(tx storage.Execer) error {
		return coordinatorStore.InsertAgentRun(ctx, tx, run)
	}); err != nil {
		t.Fatalf("InsertAgentRun: %v", err)
	}

	// The node, with its agent plugin.
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
	defer stopNode()
	go func() { _ = executionNode.Run(nodeCtx) }()

	waitFor(t, "the node to connect", func() bool {
		n, ok := coordinator.NodeServer().Node(testNodeID)
		return ok && n.Connected()
	})

	nc, _ := coordinator.NodeServer().Node(testNodeID)

	// Start the execution on the node.
	var started map[string]any
	if err := nc.Call(ctx, v1.MethodExecutionStart, v1.ExecutionStartParams{
		AgentRunID:    testRun,
		SessionID:     testSession,
		Generation:    1,
		WorkspacePath: "/tmp/ws",
	}, &started); err != nil {
		t.Fatalf("execution.start: %v", err)
	}
	if started["runtimeSessionId"] != "agent-session-test-agent" {
		t.Fatalf("started = %v", started)
	}

	// The agent's own report must have reached the coordinator and moved the run.
	waitFor(t, "the run to record its runtime session", func() bool {
		r, err := coordinatorStore.GetAgentRun(ctx, testRun)
		return err == nil && r.RuntimeSessionID == "agent-session-test-agent" && r.State == agent.StateStarting
	})

	// The prompt is acknowledged; the turn outcome arrives separately as an
	// execution report, because the node reports what actually happened rather
	// than what was requested.
	var prompted map[string]any
	if err := nc.Call(ctx, v1.MethodExecutionPrompt, v1.ExecutionPromptParams{
		AgentRunID: testRun,
		Generation: 1,
		Text:       "do the thing",
	}, &prompted); err != nil {
		t.Fatalf("execution.prompt: %v", err)
	}
	if prompted["ok"] != true {
		t.Fatalf("prompted = %v", prompted)
	}

	// The agent's event must be durable, attributed, and sequence-numbered by
	// the coordinator rather than by the node.
	waitFor(t, "the agent event to become durable", func() bool {
		n, err := coordinatorStore.EventCount(ctx, testSession)
		return err == nil && n == 1
	})

	events, err := coordinatorStore.ReadEvents(ctx, testSession, 0, 10)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	got := events[0]
	if got.Sequence != 1 {
		t.Errorf("sequence = %d, want 1 (the coordinator assigns it)", got.Sequence)
	}
	if got.Type != "agent.raw" || got.Method != "session/update" {
		t.Errorf("event = %+v", got)
	}
	if got.OriginNode != testNodeID {
		t.Errorf("origin node = %q, want %q", got.OriginNode, testNodeID)
	}
	if got.RunID != testRun {
		t.Errorf("run = %q, want %q", got.RunID, testRun)
	}

	// The node buffer is cleared once the coordinator has the event.
	waitFor(t, "the node buffer to be cleared", func() bool {
		n, err := nodeStore.BufferedCount(ctx)
		return err == nil && n == 0
	})

	// The run reached a terminal state.
	waitFor(t, "the run to finish", func() bool {
		r, err := coordinatorStore.GetAgentRun(ctx, testRun)
		return err == nil && r.State == agent.StateCompleted
	})

	// The session outlived the run.
	reloaded, err := coordinatorStore.GetSession(ctx, testSession)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if reloaded.State != session.StateActive {
		t.Errorf("session state = %q, want active", reloaded.State)
	}
}

// A node that cannot reach the coordinator must still buffer events durably
// rather than lose them.
func TestNodeBuffersEventsWhileDisconnected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cert, err := node.LoadOrCreateCertificate(t.TempDir())
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}

	nodeStore := openNodeStore(t)
	executionNode, err := daemon.NewNode(daemon.NodeOptions{
		Store:          nodeStore,
		NodeID:         testNodeID,
		Version:        "test",
		LeaseSeconds:   30,
		CoordinatorURL: "wss://127.0.0.1:1/node",
		TLSConfig:      node.ClientTLSConfig(cert),
		AgentPlugins:   []plugin.Spec{testAgentSpecWithEnv("HIVE_TEST_AGENT_PUBLISH_ON_START=" + testSession)},
		AgentPluginID:  testAgent,
	})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	nodeCtx, stopNode := context.WithCancel(ctx)
	defer stopNode()
	go func() { _ = executionNode.Run(nodeCtx) }()

	waitFor(t, "the event to be buffered", func() bool {
		n, err := nodeStore.BufferedCount(ctx)
		return err == nil && n == 1
	})

	buffered, err := nodeStore.BufferedEvents(ctx, 10)
	if err != nil {
		t.Fatalf("BufferedEvents: %v", err)
	}
	ev := buffered[0].Event
	if ev.SessionID != testSession || ev.RunID != testRun {
		t.Errorf("buffered event = %+v", ev)
	}
	if ev.ID == "" {
		t.Error("the node must assign a stable event id before buffering")
	}
	if buffered[0].LocalSequence != 1 {
		t.Errorf("local sequence = %d, want 1", buffered[0].LocalSequence)
	}
}

// A node cannot invent durable state: an event for a session the coordinator has
// never seen is refused, and the node keeps it buffered instead of dropping it.
func TestEventForAnUnknownSessionStaysBuffered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const unknownSession = "sess_unknown"

	coordinatorStore := openCoordinatorStore(t)
	cert, err := node.LoadOrCreateCertificate(t.TempDir())
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}

	coordinator, err := daemon.NewCoordinator(daemon.CoordinatorOptions{
		Store:       coordinatorStore,
		Listen:      "127.0.0.1:0",
		Certificate: cert,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	if err := coordinator.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer coordinator.Close()

	nodeStore := openNodeStore(t)
	executionNode, err := daemon.NewNode(daemon.NodeOptions{
		Store:          nodeStore,
		NodeID:         testNodeID,
		Version:        "test",
		LeaseSeconds:   30,
		CoordinatorURL: coordinator.URL(),
		TLSConfig:      node.ClientTLSConfig(cert),
		AgentPlugins:   []plugin.Spec{testAgentSpecWithEnv("HIVE_TEST_AGENT_PUBLISH_ON_START=" + unknownSession)},
		AgentPluginID:  testAgent,
	})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	nodeCtx, stopNode := context.WithCancel(ctx)
	defer stopNode()
	go func() { _ = executionNode.Run(nodeCtx) }()

	waitFor(t, "the node to connect", func() bool {
		n, ok := coordinator.NodeServer().Node(testNodeID)
		return ok && n.Connected()
	})

	// The upload is attempted and refused, so the event must stay local. Poll
	// rather than sleep once, so a late success would still be caught.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		events, err := coordinatorStore.EventCount(ctx, unknownSession)
		if err != nil {
			t.Fatalf("EventCount: %v", err)
		}
		if events != 0 {
			t.Fatalf("coordinator accepted %d events for an unknown session", events)
		}

		buffered, err := nodeStore.BufferedCount(ctx)
		if err != nil {
			t.Fatalf("BufferedCount: %v", err)
		}
		if buffered != 1 {
			t.Fatalf("buffered = %d, want the refused event to stay buffered", buffered)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
