package acpbridge_test

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thupham/hive/plugins/acp"
	"github.com/thupham/hive/plugins/acpbridge"
	"github.com/thupham/hive/plugins/sdk"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// --- fake core -------------------------------------------------------------

// core is the Hive side of the plugin link: it answers the handshake and
// records what the bridge reports.
type core struct {
	host *sdk.Host
	peer *v1.Peer

	mu        sync.Mutex
	reports   []v1.ExecutionReportParams
	published []v1.PublishEventParams
	requests  []v1.PermissionRequestParams
}

func newCore(t *testing.T) *core {
	t.Helper()
	pluginConn, coreConn := net.Pipe()

	c := &core{peer: v1.NewPeer(v1.NewStream(coreConn, coreConn, coreConn))}
	c.peer.Start()
	go c.serve()

	host, err := sdk.Connect(context.Background(), v1.NewStream(pluginConn, pluginConn, pluginConn), sdk.Options{
		ID:      "acp",
		Type:    v1.PluginTypeAgent,
		Version: "test",
		Capabilities: []string{
			v1.CapabilityEventWrite,
			v1.CapabilityPermissionWrite,
			v1.CapabilityExecutionWrite,
		},
	})
	if err != nil {
		t.Fatalf("sdk.Connect: %v", err)
	}
	c.host = host

	t.Cleanup(func() {
		_ = host.Close()
		_ = c.peer.Close()
	})
	return c
}

func (c *core) serve() {
	for {
		select {
		case <-c.peer.Done():
			return
		case req, ok := <-c.peer.Requests():
			if !ok {
				return
			}
			c.handle(req)
		}
	}
}

func (c *core) handle(req *v1.Message) {
	switch req.Method {
	case v1.MethodPluginHello:
		_ = c.peer.Respond(req.RequestID(), v1.HelloResponse{
			InstanceID:           "inst_1",
			ConnectionGeneration: 1,
			Capabilities: []string{
				v1.CapabilityEventWrite,
				v1.CapabilityPermissionWrite,
				v1.CapabilityExecutionWrite,
			},
		})
	case v1.MethodExecutionReport:
		var p v1.ExecutionReportParams
		_ = json.Unmarshal(req.Params, &p)
		c.mu.Lock()
		c.reports = append(c.reports, p)
		c.mu.Unlock()
		_ = c.peer.Respond(req.RequestID(), map[string]any{"ok": true})
	case v1.MethodEventPublish:
		var p v1.PublishEventParams
		_ = json.Unmarshal(req.Params, &p)
		c.mu.Lock()
		c.published = append(c.published, p)
		c.mu.Unlock()
		_ = c.peer.Respond(req.RequestID(), map[string]any{"ok": true})
	case v1.MethodPermissionRequest:
		var p v1.PermissionRequestParams
		_ = json.Unmarshal(req.Params, &p)
		c.mu.Lock()
		c.requests = append(c.requests, p)
		c.mu.Unlock()
		_ = c.peer.Respond(req.RequestID(), map[string]any{"ok": true})
	default:
		_ = c.peer.RespondError(req.RequestID(), v1.MethodNotFound(req.Method))
	}
}

func (c *core) reportsSnapshot() []v1.ExecutionReportParams {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]v1.ExecutionReportParams(nil), c.reports...)
}

func (c *core) publishedSnapshot() []v1.PublishEventParams {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]v1.PublishEventParams(nil), c.published...)
}

func (c *core) requestsSnapshot() []v1.PermissionRequestParams {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]v1.PermissionRequestParams(nil), c.requests...)
}

// --- fake ACP agent --------------------------------------------------------

type fakeAgent struct {
	t    *testing.T
	conn net.Conn
	enc  *json.Encoder

	mu       sync.Mutex
	onMethod func(method string, id json.RawMessage, params json.RawMessage)
	outcomes []acp.PermissionOutcome
	loaded   []string
	setOpts  []string
	auths    []string
	wg       sync.WaitGroup
}

// droppedUpdates is how many notifications the client could not take.
func (a *fakeAgent) droppedUpdates() int64 { return 0 }

// authCount is how many times the agent was asked to authenticate.
func (a *fakeAgent) authCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.auths)
}

// setOptions is how many options the agent was asked to set.
func (a *fakeAgent) setOptions() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.setOpts)
}

// loadedSnapshot is what the agent was asked to restore.
func (a *fakeAgent) loadedSnapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.loaded...)
}

func newFakeAgent(t *testing.T, conn net.Conn) *fakeAgent {
	t.Helper()
	a := &fakeAgent{t: t, conn: conn, enc: json.NewEncoder(conn)}
	a.wg.Add(1)
	go a.loop()
	return a
}

func (a *fakeAgent) loop() {
	defer a.wg.Done()
	dec := json.NewDecoder(a.conn)
	for {
		var msg struct {
			ID     json.RawMessage `json:"id,omitempty"`
			Method string          `json:"method,omitempty"`
			Params json.RawMessage `json:"params,omitempty"`
			Result json.RawMessage `json:"result,omitempty"`
		}
		if err := dec.Decode(&msg); err != nil {
			return
		}
		if msg.Method == "" {
			// The decision is nested, as ACP defines it: a flat outcome is what an
			// agent rejects.
			var response acp.PermissionResponse
			if err := json.Unmarshal(msg.Result, &response); err == nil {
				a.mu.Lock()
				a.outcomes = append(a.outcomes, response.Outcome)
				a.mu.Unlock()
			}
			continue
		}
		a.mu.Lock()
		h := a.onMethod
		a.mu.Unlock()
		if h != nil {
			h(msg.Method, msg.ID, msg.Params)
		}
	}
}

func (a *fakeAgent) setHandler(h func(method string, id json.RawMessage, params json.RawMessage)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onMethod = h
}

func (a *fakeAgent) send(v any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enc.Encode(v); err != nil {
		a.t.Logf("fake agent write: %v", err)
	}
}

func (a *fakeAgent) reply(id json.RawMessage, result any) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// replyError answers with a JSON-RPC error, as an agent refusing a request does.
func (a *fakeAgent) replyError(id json.RawMessage, code int, message string) {
	a.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

func (a *fakeAgent) notify(method string, params any) {
	a.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// request sends an agent request. The id is any JSON value, because ACP lets an
// agent use a number or a string, and a real one uses a string.
func (a *fakeAgent) request(id any, method string, params any) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

// baseHandler answers the ACP methods the bridge needs.
func (a *fakeAgent) baseHandler(method string, id json.RawMessage, params json.RawMessage) {
	switch method {
	case "initialize":
		a.reply(id, map[string]any{
			"protocolVersion":   acp.ProtocolVersion,
			"agentCapabilities": map[string]any{"loadSession": true},
			"authMethods":       []map[string]any{{"id": "browser", "name": "Browser"}},
		})
	case "session/new":
		a.reply(id, map[string]any{"sessionId": "agent-sess-1"})
	case "session/set_config_option":
		// A real agent refuses a value it does not offer, and an empty value is
		// not one of them.
		var req struct {
			Value any `json:"value"`
		}
		_ = json.Unmarshal(params, &req)
		if value, ok := req.Value.(string); ok && value == "" {
			a.replyError(id, -32602, "Invalid params")
			return
		}
		a.mu.Lock()
		a.setOpts = append(a.setOpts, string(params))
		a.mu.Unlock()
		a.reply(id, map[string]any{})
	case "authenticate":
		a.mu.Lock()
		a.auths = append(a.auths, string(params))
		a.mu.Unlock()
		a.reply(id, map[string]any{})
	case "session/load":
		a.mu.Lock()
		a.loaded = append(a.loaded, string(params))
		a.mu.Unlock()
		a.reply(id, map[string]any{})
	case "session/prompt":
		a.reply(id, map[string]any{"stopReason": "end_turn"})
	}
}

func (a *fakeAgent) outcomeSnapshot() []acp.PermissionOutcome {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]acp.PermissionOutcome(nil), a.outcomes...)
}

type fakeLauncher struct {
	t     *testing.T
	agent *fakeAgent
	fail  bool

	// onReady scripts the agent before the bridge uses it.
	onReady func(*fakeAgent)
}

func (l *fakeLauncher) Launch(ctx context.Context) (*acp.Client, *acp.Capabilities, error) {
	if l.fail {
		return nil, nil, context.DeadlineExceeded
	}

	clientConn, agentConn := net.Pipe()
	agent := newFakeAgent(l.t, agentConn)
	agent.setHandler(agent.baseHandler)
	l.agent = agent
	if l.onReady != nil {
		l.onReady(agent)
	}

	client := acp.NewClient(clientConn, acp.Options{ClientVersion: "test"})
	caps, err := client.Initialize(ctx)
	if err != nil {
		return nil, nil, err
	}
	return client, caps, nil
}

// --- harness ---------------------------------------------------------------

func newBridge(t *testing.T) (*acpbridge.Bridge, *core, *fakeLauncher) {
	t.Helper()
	c := newCore(t)
	launcher := &fakeLauncher{t: t}

	bridge := acpbridge.New(c.host, launcher, acpbridge.Options{})
	bridge.Register()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = bridge.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	return bridge, c, launcher
}

func callStart(t *testing.T, c *core, agentRunID string, generation int64) (map[string]any, error) {
	t.Helper()
	var out map[string]any
	err := c.peer.Call(context.Background(), v1.MethodExecutionStart, v1.ExecutionStartParams{
		AgentRunID:    agentRunID,
		SessionID:     "sess_1",
		Generation:    generation,
		WorkspacePath: "/tmp/ws",
	}, &out)
	return out, err
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- tests -----------------------------------------------------------------

func TestStartCreatesAnAgentSessionAndReports(t *testing.T) {
	_, c, _ := newBridge(t)

	out, err := callStart(t, c, "run_1", 1)
	if err != nil {
		t.Fatalf("execution.start: %v", err)
	}
	if out["runtimeSessionId"] != "agent-sess-1" {
		t.Fatalf("result = %v", out)
	}

	reports := c.reportsSnapshot()
	if len(reports) == 0 {
		t.Fatal("no execution report was sent")
	}
	last := reports[len(reports)-1]
	if last.AgentRunID != "run_1" || last.State != "starting" || last.RuntimeSessionID != "agent-sess-1" {
		t.Fatalf("report = %+v", last)
	}
	if last.Generation != 1 {
		t.Errorf("generation = %d, want 1", last.Generation)
	}
}

// A failed start side effect must be reported as absent, so the core can
// reconcile instead of starting a second execution.
func TestStartReportsAbsentWhenTheAgentCannotStart(t *testing.T) {
	c := newCore(t)
	bridge := acpbridge.New(c.host, &fakeLauncher{t: t, fail: true}, acpbridge.Options{})
	bridge.Register()
	go func() { _ = bridge.Run(context.Background()) }()

	if _, err := callStart(t, c, "run_1", 1); err == nil {
		t.Fatal("expected execution.start to fail")
	}

	reports := c.reportsSnapshot()
	if len(reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(reports))
	}
	if reports[0].State != "absent" {
		t.Fatalf("state = %q, want absent", reports[0].State)
	}
}

func TestPromptForwardsUpdatesAndReportsTerminal(t *testing.T) {
	_, c, launcher := newBridge(t)

	if _, err := callStart(t, c, "run_1", 1); err != nil {
		t.Fatalf("execution.start: %v", err)
	}

	launcher.agent.setHandler(func(method string, id json.RawMessage, params json.RawMessage) {
		if method == "session/prompt" {
			launcher.agent.notify("session/update", map[string]any{
				"sessionId": "agent-sess-1",
				"update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": "working"},
				},
			})
		}
		launcher.agent.baseHandler(method, id, params)
	})

	var out map[string]any
	err := c.peer.Call(context.Background(), v1.MethodExecutionPrompt, v1.ExecutionPromptParams{
		AgentRunID: "run_1",
		Generation: 1,
		Text:       "fix the bug",
	}, &out)
	if err != nil {
		t.Fatalf("execution.prompt: %v", err)
	}
	if out["stopReason"] != "end_turn" {
		t.Fatalf("result = %v", out)
	}

	// The raw stream and the turn's answer are both published, and the order
	// between them is not something a test should depend on.
	waitFor(t, "the raw stream and the answer", func() bool {
		var raw, answer bool
		for _, published := range c.publishedSnapshot() {
			switch published.Type {
			case "agent.raw":
				raw = true
			case v1.EventMessage:
				answer = true
			}
		}
		return raw && answer
	})

	var sawRaw, sawAnswer bool
	for _, published := range c.publishedSnapshot() {
		if published.AgentRunID != "run_1" {
			t.Errorf("agent run = %q", published.AgentRunID)
		}
		if published.Protocol != "acp" || published.Method != "session/update" {
			t.Errorf("published = %+v", published)
		}

		switch published.Type {
		case "agent.raw":
			sawRaw = true
			var payload map[string]any
			if err := json.Unmarshal(published.Payload, &payload); err != nil {
				t.Fatalf("payload: %v", err)
			}
			if payload["sessionUpdate"] != "agent_message_chunk" {
				t.Errorf("payload = %v, want the raw ACP update", payload)
			}

		case v1.EventMessage:
			sawAnswer = true
			var payload map[string]any
			if err := json.Unmarshal(published.Payload, &payload); err != nil {
				t.Fatalf("answer payload: %v", err)
			}
			if payload["text"] != "working" {
				t.Errorf("answer = %v, want the assistant text", payload)
			}
		}
	}
	if !sawRaw || !sawAnswer {
		t.Fatalf("raw = %v, answer = %v; both should be published", sawRaw, sawAnswer)
	}

	waitFor(t, "a terminal report", func() bool {
		for _, r := range c.reportsSnapshot() {
			if r.State == "terminal" {
				return true
			}
		}
		return false
	})
}

func TestPermissionIsForwardedAndAnswered(t *testing.T) {
	_, c, launcher := newBridge(t)

	if _, err := callStart(t, c, "run_1", 1); err != nil {
		t.Fatalf("execution.start: %v", err)
	}

	launcher.agent.setHandler(func(method string, id json.RawMessage, params json.RawMessage) {
		if method == "session/prompt" {
			launcher.agent.request(77, "session/request_permission", map[string]any{
				"sessionId": "agent-sess-1",
				"toolCall":  map[string]any{"toolCallId": "tc1", "title": "Write file"},
				"options": []map[string]any{
					{"optionId": "a1", "name": "Allow", "kind": "allow"},
					{"optionId": "d1", "name": "Deny", "kind": "deny"},
				},
			})
		}
		launcher.agent.baseHandler(method, id, params)
	})

	var out map[string]any
	if err := c.peer.Call(context.Background(), v1.MethodExecutionPrompt, v1.ExecutionPromptParams{
		AgentRunID: "run_1", Generation: 1, Text: "write it",
	}, &out); err != nil {
		t.Fatalf("execution.prompt: %v", err)
	}

	waitFor(t, "the permission request", func() bool { return len(c.requestsSnapshot()) > 0 })
	req := c.requestsSnapshot()[0]
	if req.AgentRunID != "run_1" {
		t.Errorf("agent run = %q", req.AgentRunID)
	}
	if req.AgentRequestID != "77" {
		t.Errorf("agent request id = %q", req.AgentRequestID)
	}

	// Hive approves; the bridge must express that in the agent's own terms.
	var respondOut map[string]any
	if err := c.peer.Call(context.Background(), v1.MethodPermissionRespond, v1.PermissionRespondParams{
		AgentRunID:     "run_1",
		AgentRequestID: "77",
		Approved:       true,
	}, &respondOut); err != nil {
		t.Fatalf("permission.respond: %v", err)
	}
	if respondOut["outcome"] != "selected" {
		t.Fatalf("outcome = %v", respondOut)
	}

	waitFor(t, "the agent to receive the outcome", func() bool {
		return len(launcher.agent.outcomeSnapshot()) > 0
	})
	got := launcher.agent.outcomeSnapshot()[0]
	if got.Outcome != "selected" || got.OptionID != "a1" {
		t.Fatalf("agent outcome = %+v", got)
	}
}

// An agent's request id is a string, and the id Hive is handed back has to be
// the id it stored.
//
// Found in a live conversation: every click was refused with "permission request
// ... is not pending", because the pending request was keyed by the id's raw
// JSON — quotes included — and looked up by the id itself. A number hid it,
// because a number's raw JSON is its text.
func TestPermissionWithAStringRequestIDIsAnswered(t *testing.T) {
	_, c, launcher := newBridge(t)
	const requestID = "e1bf1074-79b2-4a3c-b60f-5ddba8f97e69"

	if _, err := callStart(t, c, "run_1", 1); err != nil {
		t.Fatalf("execution.start: %v", err)
	}

	launcher.agent.setHandler(func(method string, id json.RawMessage, params json.RawMessage) {
		if method == "session/prompt" {
			launcher.agent.request(requestID, "session/request_permission", map[string]any{
				"sessionId": "agent-sess-1",
				"toolCall":  map[string]any{"toolCallId": "tc1", "title": "Run ls"},
				"options": []map[string]any{
					{"optionId": "allow-once", "name": "Allow once", "kind": "allow"},
					{"optionId": "allow-always", "name": "Allow always", "kind": "allow"},
					{"optionId": "deny", "name": "Deny", "kind": "deny"},
				},
			})
		}
		launcher.agent.baseHandler(method, id, params)
	})

	if err := c.peer.Call(context.Background(), v1.MethodExecutionPrompt, v1.ExecutionPromptParams{
		AgentRunID: "run_1", Generation: 1, Text: "run it",
	}, &map[string]any{}); err != nil {
		t.Fatalf("execution.prompt: %v", err)
	}

	waitFor(t, "the permission request", func() bool { return len(c.requestsSnapshot()) > 0 })
	if got := c.requestsSnapshot()[0].AgentRequestID; got != requestID {
		t.Fatalf("agent request id = %q, want %q", got, requestID)
	}

	// The user picks the agent's second option, which is what the agent's own
	// button carries.
	var out map[string]any
	if err := c.peer.Call(context.Background(), v1.MethodPermissionRespond, v1.PermissionRespondParams{
		AgentRunID:     "run_1",
		AgentRequestID: requestID,
		OptionID:       "allow-always",
	}, &out); err != nil {
		t.Fatalf("permission.respond: %v", err)
	}

	waitFor(t, "the agent to receive the outcome", func() bool {
		return len(launcher.agent.outcomeSnapshot()) > 0
	})
	if got := launcher.agent.outcomeSnapshot()[0]; got.OptionID != "allow-always" {
		t.Fatalf("agent outcome = %+v, want the option the user chose", got)
	}
}

func TestPromptBeforeStartIsNotFound(t *testing.T) {
	_, c, _ := newBridge(t)

	var out map[string]any
	err := c.peer.Call(context.Background(), v1.MethodExecutionPrompt, v1.ExecutionPromptParams{
		AgentRunID: "run_missing", Generation: 1, Text: "hello",
	}, &out)
	if err == nil {
		t.Fatal("expected prompting an unknown run to fail")
	}
	if e := v1.AsError(err); e.Code != v1.CodeNotFound {
		t.Fatalf("code = %d, want %d", e.Code, v1.CodeNotFound)
	}
}

func TestCancelReachesTheAgent(t *testing.T) {
	_, c, launcher := newBridge(t)

	if _, err := callStart(t, c, "run_1", 1); err != nil {
		t.Fatalf("execution.start: %v", err)
	}

	cancelled := make(chan struct{}, 1)
	launcher.agent.setHandler(func(method string, id json.RawMessage, params json.RawMessage) {
		if method == "session/cancel" {
			select {
			case cancelled <- struct{}{}:
			default:
			}
		}
		launcher.agent.baseHandler(method, id, params)
	})

	var out map[string]any
	if err := c.peer.Call(context.Background(), v1.MethodExecutionCancel, v1.ExecutionCancelParams{
		AgentRunID: "run_1", Generation: 1,
	}, &out); err != nil {
		t.Fatalf("execution.cancel: %v", err)
	}

	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("the agent never received session/cancel")
	}
}

// A run that already had an agent session asks for it back instead of starting
// over.
//
// This is what makes a restart survivable: the agent keeps its own conversation
// history rather than being handed a fresh one.
func TestStartRestoresAnExistingAgentSession(t *testing.T) {
	bridge, c, launcher := newBridge(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bridge.Run(ctx) }()

	// The agent is launched on the first start, so there is nothing to wait for.
	var out map[string]any
	err := c.peer.Call(ctx, v1.MethodExecutionStart, v1.ExecutionStartParams{
		AgentRunID:    "run_1",
		SessionID:     "sess_1",
		Generation:    1,
		WorkspacePath: "/tmp/ws",
		Resume:        "agent-sess-9",
	}, &out)
	if err != nil {
		t.Fatalf("execution.start: %v", err)
	}

	if out["runtimeSessionId"] != "agent-sess-9" {
		t.Errorf("runtime session = %v, want the restored one", out["runtimeSessionId"])
	}
	if out["resumed"] != true {
		t.Errorf("resumed = %v, want true", out["resumed"])
	}

	loaded := launcher.agent.loadedSnapshot()
	if len(loaded) != 1 || !strings.Contains(loaded[0], "agent-sess-9") {
		t.Fatalf("the agent was asked to restore %v", loaded)
	}
}

// A run with no agent session creates one, and does not ask to restore.
func TestStartCreatesWhenThereIsNothingToRestore(t *testing.T) {
	bridge, c, launcher := newBridge(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bridge.Run(ctx) }()

	out, err := callStart(t, c, "run_1", 1)
	if err != nil {
		t.Fatalf("execution.start: %v", err)
	}

	if out["runtimeSessionId"] != "agent-sess-1" {
		t.Errorf("runtime session = %v, want a new one", out["runtimeSessionId"])
	}
	if out["resumed"] != nil {
		t.Errorf("resumed = %v, want absent", out["resumed"])
	}
	if loaded := launcher.agent.loadedSnapshot(); len(loaded) != 0 {
		t.Fatalf("the agent was asked to restore %v", loaded)
	}
}

// What a turn cost is published after the answer it belongs to, so a client can
// show the number without reading the agent's protocol.
func TestPromptPublishesTheTurnsUsage(t *testing.T) {
	_, c, launcher := newBridge(t)
	ctx := context.Background()

	if _, err := callStart(t, c, "run_1", 1); err != nil {
		t.Fatalf("execution.start: %v", err)
	}

	launcher.agent.setHandler(func(method string, id json.RawMessage, params json.RawMessage) {
		if method == "session/prompt" {
			launcher.agent.reply(id, map[string]any{
				"stopReason": "end_turn",
				"usage": map[string]any{
					"inputTokens": 1200, "outputTokens": 34, "totalTokens": 1234,
				},
			})
			return
		}
		launcher.agent.baseHandler(method, id, params)
	})

	var out map[string]any
	if err := c.peer.Call(ctx, v1.MethodExecutionPrompt, v1.ExecutionPromptParams{
		AgentRunID: "run_1", Generation: 1, Text: "hi",
	}, &out); err != nil {
		t.Fatalf("execution.prompt: %v", err)
	}

	var usage *v1.Usage
	for _, published := range c.publishedSnapshot() {
		if published.Type != v1.EventUsage {
			continue
		}
		var report v1.Usage
		if err := json.Unmarshal(published.Payload, &report); err != nil {
			t.Fatalf("usage payload: %v", err)
		}
		usage = &report
	}
	if usage == nil {
		t.Fatal("the turn's usage was not published")
	}
	if usage.TotalTokens != 1234 || usage.InputTokens != 1200 || usage.OutputTokens != 34 {
		t.Fatalf("usage = %+v", usage)
	}
}

// An agent that reports nothing publishes nothing: a usage event of zeroes would
// be a claim about the agent that is not true.
func TestPromptWithoutUsagePublishesNone(t *testing.T) {
	_, c, _ := newBridge(t)
	ctx := context.Background()

	if _, err := callStart(t, c, "run_1", 1); err != nil {
		t.Fatalf("execution.start: %v", err)
	}
	if err := c.peer.Call(ctx, v1.MethodExecutionPrompt, v1.ExecutionPromptParams{
		AgentRunID: "run_1", Generation: 1, Text: "hi",
	}, &map[string]any{}); err != nil {
		t.Fatalf("execution.prompt: %v", err)
	}

	for _, published := range c.publishedSnapshot() {
		if published.Type == v1.EventUsage {
			t.Fatalf("published a usage event with no numbers: %s", published.Payload)
		}
	}
}

// An empty value is not a choice, so there is nothing to apply.
//
// A session recorded by an older build can carry one, and applying it would fail
// every later run: the agent refuses a value that is not one.
func TestStartSkipsAnEmptyConfigValue(t *testing.T) {
	bridge, c, launcher := newBridge(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bridge.Run(ctx) }()

	var out map[string]any
	err := c.peer.Call(ctx, v1.MethodExecutionStart, v1.ExecutionStartParams{
		AgentRunID:    "run_1",
		SessionID:     "sess_1",
		Generation:    1,
		WorkspacePath: "/tmp/ws",
		Config:        map[string]string{"model": "", "mode": "  "},
	}, &out)
	if err != nil {
		t.Fatalf("execution.start: %v", err)
	}
	if out["runtimeSessionId"] != "agent-sess-1" {
		t.Errorf("the run should start anyway: %v", out)
	}
	if got := launcher.agent.setOptions(); got != 0 {
		t.Fatalf("the agent was asked to set %d option(s), want none", got)
	}
}

// A read of a selector is not a change to it.
//
// Found in a live conversation: "/model" with no argument carried an empty
// value, the bridge forwarded it as a change, and the agent refused the whole
// request with "Invalid params". The empty value is not one the agent offers.
func TestConfigReadWithNoValueDoesNotReachTheAgent(t *testing.T) {
	_, c, launcher := newBridge(t)
	ctx := context.Background()

	if _, err := callStart(t, c, "run_1", 1); err != nil {
		t.Fatalf("execution.start: %v", err)
	}

	var out map[string]any
	err := c.peer.Call(ctx, v1.MethodExecutionConfig, v1.ExecutionConfigParams{
		AgentRunID: "run_1",
		ConfigID:   "model",
	}, &out)
	if err != nil {
		t.Fatalf("a read must not fail: %v", err)
	}
	if got := launcher.agent.setOptions(); got != 0 {
		t.Fatalf("the agent was asked to set %d option(s), want none", got)
	}
}

// An agent that already has credentials is not sent through its auth flow again.
//
// Found by watching a browser window open per agent launch: the bridge
// authenticated preemptively, and an agent whose auth method opens a browser ran
// that whole flow every time, even though it had credentials on disk.
//
// The protocol lets an agent refuse a session when it needs authentication, so the
// client waits to be told.
func TestStartDoesNotAuthenticateAnAgentThatDoesNotAsk(t *testing.T) {
	bridge, c, launcher := newBridge(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bridge.Run(ctx) }()

	if _, err := callStart(t, c, "run_1", 1); err != nil {
		t.Fatalf("execution.start: %v", err)
	}

	if got := launcher.agent.authCount(); got != 0 {
		t.Fatalf("the agent was asked to authenticate %d time(s), want none", got)
	}
}

// When the agent does refuse the session, it is authenticated once and retried.
func TestStartAuthenticatesWhenTheAgentRefuses(t *testing.T) {
	bridge, c, launcher := newBridge(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bridge.Run(ctx) }()

	// The agent refuses the first session, as one that needs authentication does.
	var refused atomic.Bool
	launcher.onReady = func(agent *fakeAgent) {
		agent.setHandler(func(method string, id json.RawMessage, params json.RawMessage) {
			if method == "session/new" && refused.CompareAndSwap(false, true) {
				agent.replyError(id, -32000, "authentication required")
				return
			}
			agent.baseHandler(method, id, params)
		})
	}

	if _, err := callStart(t, c, "run_1", 1); err != nil {
		t.Fatalf("execution.start: %v", err)
	}

	if got := launcher.agent.authCount(); got != 1 {
		t.Fatalf("the agent was asked to authenticate %d time(s), want once", got)
	}
}

// The agent ends with the plugin.
//
// An agent left running is an orphan: it holds a session, its memory, and
// whatever authentication state it has, and nothing will ever collect it. Found
// by finding one alive after the daemon had been restarted several times.
func TestRunEndsTheAgent(t *testing.T) {
	bridge, c, launcher := newBridge(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = bridge.Run(ctx) }()

	if _, err := callStart(t, c, "run_1", 1); err != nil {
		t.Fatalf("execution.start: %v", err)
	}
	if launcher.agent == nil {
		t.Fatal("the agent should have been started")
	}

	// Stopping the bridge is what a plugin shutdown does.
	cancel()
	waitFor(t, "the agent to be closed", func() bool {
		return bridge.HasAgent() == false
	})
}
