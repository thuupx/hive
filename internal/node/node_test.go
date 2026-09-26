package node_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thuupx/hive/internal/event"
	"github.com/thuupx/hive/internal/node"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

type fakeExecutor struct {
	mu        sync.Mutex
	started   []v1.ExecutionStartParams
	prompts   []v1.ExecutionPromptParams
	cancels   []v1.ExecutionCancelParams
	responses []v1.PermissionRespondParams
	configs   []v1.ExecutionConfigParams
}

func (f *fakeExecutor) Start(_ context.Context, req v1.ExecutionStartParams) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, req)
	return "runtime-" + req.AgentRunID, nil
}

func (f *fakeExecutor) Prompt(_ context.Context, req v1.ExecutionPromptParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, req)
	return nil
}

func (f *fakeExecutor) Cancel(_ context.Context, req v1.ExecutionCancelParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, req)
	return nil
}

func (f *fakeExecutor) Respond(_ context.Context, req v1.PermissionRespondParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses = append(f.responses, req)
	return nil
}

func (f *fakeExecutor) Live(_ context.Context, req v1.ExecutionLiveParams) (*v1.ExecutionLiveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &v1.ExecutionLiveResult{Live: true}, nil
}

func (f *fakeExecutor) Config(_ context.Context, req v1.ExecutionConfigParams) (*v1.ExecutionConfigResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs = append(f.configs, req)

	return &v1.ExecutionConfigResult{Options: []v1.SessionConfigOption{{
		ID:           "model",
		Name:         "Model",
		Category:     "model",
		Type:         "select",
		CurrentValue: json.RawMessage(`"m1"`),
		Options:      []v1.SessionConfigOptionValue{{Value: "m1", Name: "Model 1"}},
	}}}, nil
}

func (f *fakeExecutor) startedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

type fakeStore struct {
	mu      sync.Mutex
	queried []string
	live    map[string]int64
	durable map[string]bool
}

func (f *fakeStore) LiveExecutions(context.Context, string) (map[string]int64, error) {
	return f.live, nil
}

func (f *fakeStore) HasEvent(_ context.Context, eventID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queried = append(f.queried, eventID)
	return f.durable[eventID], nil
}

func (f *fakeStore) queriedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queried...)
}

func newCoordinator(t *testing.T) (*node.Server, string, *tls.Config) {
	t.Helper()

	cert, err := node.LoadOrCreateCertificate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCertificate: %v", err)
	}

	srv := node.NewServer(context.Background(), nil)
	httpSrv := httptest.NewUnstartedServer(srv)
	httpSrv.TLS = node.ServerTLSConfig(cert)
	httpSrv.StartTLS()
	t.Cleanup(httpSrv.Close)

	url := "wss" + strings.TrimPrefix(httpSrv.URL, "https")
	return srv, url, node.ClientTLSConfig(cert)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func startNode(t *testing.T, url string, tlsConfig *tls.Config, opts node.Options) (*node.Client, context.CancelFunc) {
	t.Helper()
	c := node.New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(ctx, url, tlsConfig)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return c, cancel
}

func TestHandshakeAssignsIncreasingGenerations(t *testing.T) {
	srv, url, tlsConfig := newCoordinator(t)

	first, stopFirst := startNode(t, url, tlsConfig, node.Options{NodeID: "node_a", Version: "test", LeaseSeconds: 5})
	waitFor(t, "the first connection", func() bool { return first.ConnectionGeneration() == 1 })

	oldNode, ok := srv.Node("node_a")
	if !ok {
		t.Fatal("the coordinator did not record the node")
	}

	stopFirst()

	second, _ := startNode(t, url, tlsConfig, node.Options{NodeID: "node_a", Version: "test", LeaseSeconds: 5})
	waitFor(t, "the second connection", func() bool { return second.ConnectionGeneration() == 2 })

	current, ok := srv.Node("node_a")
	if !ok {
		t.Fatal("the coordinator lost the node")
	}
	if current.ConnectionGeneration != 2 {
		t.Fatalf("generation = %d, want 2", current.ConnectionGeneration)
	}

	// The superseded connection is no longer a current node link.
	waitFor(t, "the superseded connection to be marked disconnected", func() bool {
		return !oldNode.Connected()
	})
	if err := oldNode.Call(context.Background(), v1.MethodExecutionPrompt, nil, nil); err == nil {
		t.Fatal("a superseded connection must not accept commands")
	}
}

func TestHeartbeatRenewsTheLease(t *testing.T) {
	srv, url, tlsConfig := newCoordinator(t)

	startNode(t, url, tlsConfig, node.Options{NodeID: "node_a", Version: "test", LeaseSeconds: 1})
	waitFor(t, "the node connection", func() bool {
		n, ok := srv.Node("node_a")
		return ok && n.Connected()
	})

	n, _ := srv.Node("node_a")
	first := n.LastSeen()

	waitFor(t, "the lease to be renewed", func() bool { return n.LastSeen().After(first) })
	if n.LeaseExpired(time.Now().UTC()) {
		t.Fatal("a heartbeating node must not be considered expired")
	}
}

// Lease expiry classifies a run as interrupted. It must never authorize a
// replacement execution, because the original execution may still be alive.
func TestLeaseExpiryIsReportedWithoutReplacingExecution(t *testing.T) {
	srv, url, tlsConfig := newCoordinator(t)

	executor := &fakeExecutor{}
	client, stop := startNode(t, url, tlsConfig, node.Options{
		NodeID:       "node_a",
		Version:      "test",
		LeaseSeconds: 1,
		Executor:     executor,
	})

	waitFor(t, "the node connection", func() bool {
		n, ok := srv.Node("node_a")
		return ok && n.Connected()
	})

	// The coordinator starts an execution on the node.
	n, _ := srv.Node("node_a")
	var started map[string]any
	err := n.Call(context.Background(), v1.MethodExecutionStart, v1.ExecutionStartParams{
		AgentRunID:    "run_1",
		SessionID:     "sess_1",
		Generation:    1,
		WorkspacePath: "/tmp/ws",
	}, &started)
	if err != nil {
		t.Fatalf("execution.start: %v", err)
	}
	if started["runtimeSessionId"] != "runtime-run_1" {
		t.Fatalf("result = %v", started)
	}

	// Let the node report the execution in a heartbeat.
	waitFor(t, "the node to report its execution", func() bool {
		return len(n.Executions()) == 1
	})

	// The link goes away and the lease expires.
	stop()

	expired := make(chan v1.ExecutionRef, 8)
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	go func() {
		_ = srv.WatchLeases(watchCtx, 50*time.Millisecond, func(_ string, ref v1.ExecutionRef) {
			select {
			case expired <- ref:
			default:
			}
		})
	}()

	select {
	case ref := <-expired:
		if ref.AgentRunID != "run_1" || ref.Generation != 1 {
			t.Fatalf("ref = %+v", ref)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the expired lease was never reported")
	}

	// Expiry must not have started anything.
	if got := executor.startedCount(); got != 1 {
		t.Fatalf("executor started %d executions, want 1", got)
	}
	_ = client
}

func TestReconnectSyncRequestsMissingEvents(t *testing.T) {
	srv, url, tlsConfig := newCoordinator(t)
	srv.Store = &fakeStore{durable: map[string]bool{}}

	client := node.New(node.Options{NodeID: "node_a", Version: "test", LeaseSeconds: 5})

	// Two events buffered while the coordinator was unreachable. One is
	// already durable, so only the other should be requested.
	client.Buffer(&event.Event{ID: "ev_1", SessionID: "sess_1", Type: event.TypeMessage, Version: 1})
	client.Buffer(&event.Event{ID: "ev_2", SessionID: "sess_1", Type: event.TypeMessage, Version: 1})
	srv.Store.(*fakeStore).durable["ev_1"] = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx, url, tlsConfig) }()

	waitFor(t, "the reconnect synchronization", func() bool {
		return len(srv.Store.(*fakeStore).queriedIDs()) == 2
	})

	// The store is queried before the client records the request, so the store is
	// not a sufficient signal. Poll the client's own state; reading clears it, so
	// the first non-empty read is the answer.
	var requested []*event.Event
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if requested = client.RequestedEvents(); len(requested) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if len(requested) != 1 || requested[0].ID != "ev_2" {
		t.Fatalf("requested = %+v, want only ev_2", requested)
	}

	// Acknowledging the replay clears the local buffer.
	client.AckBuffered("ev_1", "ev_2")
	if got := client.Pending(); got != 0 {
		t.Fatalf("pending = %d, want 0", got)
	}
}

func TestExecutionDispatch(t *testing.T) {
	srv, url, tlsConfig := newCoordinator(t)

	executor := &fakeExecutor{}
	startNode(t, url, tlsConfig, node.Options{
		NodeID:       "node_a",
		Version:      "test",
		LeaseSeconds: 5,
		Executor:     executor,
	})

	waitFor(t, "the node connection", func() bool {
		n, ok := srv.Node("node_a")
		return ok && n.Connected()
	})
	n, _ := srv.Node("node_a")

	var started map[string]any
	if err := n.Call(context.Background(), v1.MethodExecutionStart, v1.ExecutionStartParams{
		AgentRunID: "run_1", Generation: 1, WorkspacePath: "/tmp/ws",
	}, &started); err != nil {
		t.Fatalf("execution.start: %v", err)
	}
	if started["runtimeSessionId"] != "runtime-run_1" {
		t.Fatalf("result = %v", started)
	}

	var prompted map[string]any
	if err := n.Call(context.Background(), v1.MethodExecutionPrompt, v1.ExecutionPromptParams{
		AgentRunID: "run_1", Generation: 1, Text: "fix the bug",
	}, &prompted); err != nil {
		t.Fatalf("execution.prompt: %v", err)
	}

	var cancelled map[string]any
	if err := n.Call(context.Background(), v1.MethodExecutionCancel, v1.ExecutionCancelParams{
		AgentRunID: "run_1", Generation: 1,
	}, &cancelled); err != nil {
		t.Fatalf("execution.cancel: %v", err)
	}

	executor.mu.Lock()
	defer executor.mu.Unlock()
	if len(executor.started) != 1 || len(executor.prompts) != 1 || len(executor.cancels) != 1 {
		t.Fatalf("executor saw start=%d prompt=%d cancel=%d",
			len(executor.started), len(executor.prompts), len(executor.cancels))
	}
	if executor.prompts[0].Text != "fix the bug" {
		t.Errorf("prompt text = %q", executor.prompts[0].Text)
	}
}

func TestNodeWithoutExecutorRefusesExecution(t *testing.T) {
	srv, url, tlsConfig := newCoordinator(t)

	startNode(t, url, tlsConfig, node.Options{NodeID: "node_a", Version: "test", LeaseSeconds: 5})
	waitFor(t, "the node connection", func() bool {
		n, ok := srv.Node("node_a")
		return ok && n.Connected()
	})
	n, _ := srv.Node("node_a")

	err := n.Call(context.Background(), v1.MethodExecutionStart, v1.ExecutionStartParams{
		AgentRunID: "run_1", Generation: 1,
	}, nil)
	if err == nil {
		t.Fatal("expected the node to refuse execution work")
	}
	if e := v1.AsError(err); e.Code != v1.CodeUnavailable {
		t.Fatalf("code = %d, want %d", e.Code, v1.CodeUnavailable)
	}
}

func TestUnknownNodeMethodIsRefused(t *testing.T) {
	srv, url, tlsConfig := newCoordinator(t)

	startNode(t, url, tlsConfig, node.Options{NodeID: "node_a", Version: "test", LeaseSeconds: 5})
	waitFor(t, "the node connection", func() bool {
		n, ok := srv.Node("node_a")
		return ok && n.Connected()
	})
	n, _ := srv.Node("node_a")

	err := n.Call(context.Background(), "cluster.elect", nil, nil)
	if err == nil {
		t.Fatal("expected an unknown method to be refused")
	}
	if e := v1.AsError(err); e.Code != v1.CodeMethodNotFound {
		t.Fatalf("code = %d, want %d", e.Code, v1.CodeMethodNotFound)
	}
}
