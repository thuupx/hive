package acp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAgent is a scripted ACP agent on the other end of an in-memory pipe.
type fakeAgent struct {
	t    *testing.T
	conn net.Conn
	enc  *json.Encoder

	mu      sync.Mutex
	handler func(rpcMessage)

	recv chan rpcMessage
	wg   sync.WaitGroup
}

func newFakeAgent(t *testing.T, conn net.Conn) *fakeAgent {
	t.Helper()
	a := &fakeAgent{
		t:    t,
		conn: conn,
		enc:  json.NewEncoder(conn),
		recv: make(chan rpcMessage, 64),
	}
	a.wg.Add(1)
	go a.loop()
	return a
}

func (a *fakeAgent) loop() {
	defer a.wg.Done()
	dec := json.NewDecoder(a.conn)
	for {
		var msg rpcMessage
		if err := dec.Decode(&msg); err != nil {
			return
		}
		select {
		case a.recv <- msg:
		default:
		}

		a.mu.Lock()
		h := a.handler
		a.mu.Unlock()
		if h != nil {
			h(msg)
		}
	}
}

func (a *fakeAgent) setHandler(h func(rpcMessage)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handler = h
}

func (a *fakeAgent) send(msg rpcMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enc.Encode(msg); err != nil {
		a.t.Logf("fake agent write: %v", err)
	}
}

func (a *fakeAgent) reply(id json.RawMessage, result any) {
	a.send(rpcMessage{JSONRPC: jsonrpcVersion, ID: id, Result: encode(result)})
}

func (a *fakeAgent) replyError(id json.RawMessage, code int, message string) {
	a.send(rpcMessage{JSONRPC: jsonrpcVersion, ID: id, Error: &rpcError{Code: code, Message: message}})
}

func (a *fakeAgent) notify(method string, params any) {
	a.send(rpcMessage{JSONRPC: jsonrpcVersion, Method: method, Params: encode(params)})
}

func (a *fakeAgent) request(id int64, method string, params any) {
	a.send(rpcMessage{JSONRPC: jsonrpcVersion, ID: encode(id), Method: method, Params: encode(params)})
}

func (a *fakeAgent) next() rpcMessage {
	a.t.Helper()
	select {
	case msg := <-a.recv:
		return msg
	case <-time.After(2 * time.Second):
		a.t.Fatal("timed out waiting for a message from the client")
		return rpcMessage{}
	}
}

func (a *fakeAgent) close() {
	_ = a.conn.Close()
	a.wg.Wait()
}

func newPair(t *testing.T, opts Options) (*Client, *fakeAgent) {
	t.Helper()
	clientConn, agentConn := net.Pipe()
	client := NewClient(clientConn, opts)
	agent := newFakeAgent(t, agentConn)
	t.Cleanup(func() {
		_ = client.Close()
		agent.close()
	})
	return client, agent
}

func replyInitialize(a *fakeAgent, version int, loadSession bool) {
	a.setHandler(func(msg rpcMessage) {
		if msg.Method != methodInitialize {
			return
		}
		a.reply(msg.ID, initializeResponse{
			ProtocolVersion: version,
			AgentCapabilities: agentCapabilities{
				LoadSession:        loadSession,
				PromptCapabilities: promptCapabilities{Image: true},
			},
			AgentInfo: &Implementation{Name: "fake-agent", Version: "9.9.9"},
		})
	})
}

func TestInitializeNegotiatesVersionAndCapabilities(t *testing.T) {
	client, agent := newPair(t, Options{ClientVersion: "test"})
	replyInitialize(agent, ProtocolVersion, true)

	caps, err := client.Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if caps.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocol version = %d", caps.ProtocolVersion)
	}
	if !caps.LoadSession {
		t.Error("loadSession capability was not carried through")
	}
	if !caps.PromptImages {
		t.Error("prompt image capability was not carried through")
	}
	if caps.AgentName != "fake-agent" || caps.AgentVersion != "9.9.9" {
		t.Errorf("agent info = %q %q", caps.AgentName, caps.AgentVersion)
	}

	// The request must advertise the version Hive supports and no capability
	// Hive does not implement.
	sent := agent.next()
	if sent.Method != methodInitialize {
		t.Fatalf("method = %q", sent.Method)
	}
	var req initializeRequest
	if err := json.Unmarshal(sent.Params, &req); err != nil {
		t.Fatalf("decode initialize params: %v", err)
	}
	if req.ProtocolVersion != ProtocolVersion {
		t.Errorf("advertised version = %d, want %d", req.ProtocolVersion, ProtocolVersion)
	}
	if req.ClientCapabilities.Terminal || req.ClientCapabilities.FS.ReadTextFile {
		t.Error("Hive must not advertise capabilities it does not implement")
	}
}

// An unsupported protocol version must fail the handshake explicitly rather
// than be interpreted with the wrong semantics.
func TestInitializeRejectsUnsupportedVersion(t *testing.T) {
	client, agent := newPair(t, Options{})
	replyInitialize(agent, ProtocolVersion+1, true)

	_, err := client.Initialize(context.Background())
	if err == nil {
		t.Fatal("expected the handshake to fail")
	}
	if !contains(err.Error(), "protocol version") {
		t.Fatalf("error = %v, want it to name the protocol version", err)
	}

	select {
	case <-client.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("an incompatible version should close the connection")
	}
}

func TestNewSessionRequiresCwd(t *testing.T) {
	client, _ := newPair(t, Options{})
	if _, err := client.NewSession(context.Background(), NewSessionRequest{}); err == nil {
		t.Fatal("expected a missing cwd to be rejected")
	}
}

func TestNewSessionReturnsRuntimeSessionID(t *testing.T) {
	client, agent := newPair(t, Options{})
	agent.setHandler(func(msg rpcMessage) {
		if msg.Method == methodSessionNew {
			agent.reply(msg.ID, newSessionResponse{SessionID: "agent-sess-1"})
		}
	})

	id, err := client.NewSession(context.Background(), NewSessionRequest{Cwd: "/tmp/ws"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if id != "agent-sess-1" {
		t.Fatalf("session id = %q", id)
	}

	var sent NewSessionRequest
	if err := json.Unmarshal(agent.next().Params, &sent); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if sent.Cwd != "/tmp/ws" {
		t.Errorf("cwd = %q", sent.Cwd)
	}
	if sent.McpServers == nil {
		t.Error("mcpServers must be present, even when empty")
	}
}

func TestLoadSessionCarriesTheRuntimeSessionID(t *testing.T) {
	client, agent := newPair(t, Options{})
	agent.setHandler(func(msg rpcMessage) {
		if msg.Method == methodSessionLoad {
			agent.reply(msg.ID, struct{}{})
		}
	})

	err := client.LoadSession(context.Background(), LoadSessionRequest{
		SessionID: "agent-sess-1",
		Cwd:       "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}

	var sent LoadSessionRequest
	if err := json.Unmarshal(agent.next().Params, &sent); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if sent.SessionID != "agent-sess-1" {
		t.Fatalf("session id = %q", sent.SessionID)
	}
}

func TestPromptReturnsStopReason(t *testing.T) {
	client, agent := newPair(t, Options{})
	agent.setHandler(func(msg rpcMessage) {
		if msg.Method == methodSessionPrompt {
			agent.reply(msg.ID, promptResponse{StopReason: "end_turn"})
		}
	})

	reason, err := client.Prompt(context.Background(), "s1", "fix the bug")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if reason != "end_turn" {
		t.Fatalf("stop reason = %q", reason)
	}

	var sent promptRequest
	if err := json.Unmarshal(agent.next().Params, &sent); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if sent.SessionID != "s1" || len(sent.Prompt) != 1 || sent.Prompt[0].Text != "fix the bug" {
		t.Fatalf("prompt = %+v", sent)
	}
}

func TestCancelSendsNotification(t *testing.T) {
	client, agent := newPair(t, Options{})
	if err := client.Cancel("s1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	sent := agent.next()
	if sent.Method != methodSessionCancel {
		t.Fatalf("method = %q", sent.Method)
	}
	if len(sent.ID) != 0 {
		t.Error("session/cancel must be a notification")
	}
}

// Agent-specific streaming updates stay protocol-native: Hive does not need a
// new core event type when the agent protocol grows.
func TestSessionUpdatesArePreservedAsRawData(t *testing.T) {
	client, agent := newPair(t, Options{})

	agent.notify(methodSessionUpdate, map[string]any{
		"sessionId": "s1",
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": "hello"},
		},
	})

	select {
	case update := <-client.Updates():
		if update.Method != methodSessionUpdate {
			t.Errorf("method = %q", update.Method)
		}
		if update.SessionID != "s1" {
			t.Errorf("session id = %q", update.SessionID)
		}
		var payload map[string]any
		if err := json.Unmarshal(update.Payload, &payload); err != nil {
			t.Fatalf("payload is not valid json: %v", err)
		}
		if payload["sessionUpdate"] != "agent_message_chunk" {
			t.Errorf("payload = %v, want the raw update object", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no update was delivered")
	}
}

func TestUnknownNotificationsSurvive(t *testing.T) {
	client, agent := newPair(t, Options{})

	agent.notify("future/thing", map[string]any{"sessionId": "s1", "novel": true})

	select {
	case update := <-client.Updates():
		if update.Method != "future/thing" {
			t.Errorf("method = %q", update.Method)
		}
		var payload map[string]any
		if err := json.Unmarshal(update.Payload, &payload); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if payload["novel"] != true {
			t.Errorf("payload = %v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an unknown notification was dropped")
	}
}

func TestPermissionRequestIsDeliveredAndAnswered(t *testing.T) {
	client, agent := newPair(t, Options{})

	agent.request(99, methodRequestPermission, map[string]any{
		"sessionId": "s1",
		"toolCall":  map[string]any{"toolCallId": "tc1", "title": "Write file"},
		"options": []PermissionOption{
			{OptionID: "a1", Name: "Allow", Kind: OptionAllow},
			{OptionID: "d1", Name: "Deny", Kind: OptionDeny},
		},
	})

	var req PermissionRequest
	select {
	case req = <-client.Permissions():
	case <-time.After(2 * time.Second):
		t.Fatal("the permission request was not delivered")
	}
	if req.SessionID != "s1" {
		t.Errorf("session id = %q", req.SessionID)
	}
	if len(req.Options) != 2 {
		t.Fatalf("options = %d", len(req.Options))
	}
	if string(req.RequestID) != "99" {
		t.Errorf("request id = %s", req.RequestID)
	}

	outcome := OutcomeForDecision(req.Options, true, "")
	if err := client.RespondToPermission(req.RequestID, outcome); err != nil {
		t.Fatalf("RespondToPermission: %v", err)
	}

	// The agent sees a response to its own request id.
	var seen *rpcMessage
	deadline := time.After(2 * time.Second)
	for seen == nil {
		select {
		case msg := <-agent.recv:
			if msg.Method == "" && string(msg.ID) == "99" {
				m := msg
				seen = &m
			}
		case <-deadline:
			t.Fatal("the agent never received the permission response")
		}
	}
	if seen.Error != nil {
		t.Fatalf("agent received an error: %v", seen.Error)
	}
	var got PermissionOutcome
	if err := json.Unmarshal(seen.Result, &got); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	if got.Outcome != outcomeSelected || got.OptionID != "a1" {
		t.Fatalf("outcome = %+v", got)
	}
}

// Hive must never answer a permission request with an approval it did not
// receive. When it cannot take delivery, it cancels.
func TestPermissionRequestIsCancelledWhenItCannotBeDelivered(t *testing.T) {
	_, agent := newPair(t, Options{PermissionBuffer: 1})

	params := map[string]any{
		"sessionId": "s1",
		"toolCall":  map[string]any{"toolCallId": "tc1"},
		"options":   []PermissionOption{{OptionID: "a1", Kind: OptionAllow}},
	}
	agent.request(1, methodRequestPermission, params)
	agent.request(2, methodRequestPermission, params)

	var cancelled *rpcMessage
	deadline := time.After(2 * time.Second)
	for cancelled == nil {
		select {
		case msg := <-agent.recv:
			if msg.Method == "" && string(msg.ID) == "2" {
				m := msg
				cancelled = &m
			}
		case <-deadline:
			t.Fatal("the undeliverable request was left unanswered")
		}
	}

	var got PermissionOutcome
	if err := json.Unmarshal(cancelled.Result, &got); err != nil {
		t.Fatalf("decode outcome: %v", err)
	}
	if got.Outcome != outcomeCancelled {
		t.Fatalf("outcome = %+v, want cancelled", got)
	}
}

// Hive advertises no filesystem or terminal capabilities, so a conforming
// agent will not call these. If one does, Hive refuses instead of pretending.
func TestUnsupportedInboundMethodIsRefused(t *testing.T) {
	_, agent := newPair(t, Options{})

	agent.request(5, "fs/read_text_file", map[string]any{"path": "/etc/passwd"})

	msg := agent.next()
	if msg.Error == nil {
		t.Fatal("expected an error response")
	}
	if msg.Error.Code != codeMethodNotFound {
		t.Fatalf("code = %d, want %d", msg.Error.Code, codeMethodNotFound)
	}
}

func TestOutcomeForDecision(t *testing.T) {
	options := []PermissionOption{
		{OptionID: "a1", Name: "Allow once", Kind: OptionAllow},
		{OptionID: "d1", Name: "Deny", Kind: OptionDeny},
	}

	if got := OutcomeForDecision(options, true, ""); got.Outcome != outcomeSelected || got.OptionID != "a1" {
		t.Errorf("approve = %+v", got)
	}
	if got := OutcomeForDecision(options, false, ""); got.Outcome != outcomeSelected || got.OptionID != "d1" {
		t.Errorf("deny = %+v", got)
	}

	// A transport that rendered the agent's own options can name one directly.
	if got := OutcomeForDecision(options, false, "a1"); got.OptionID != "a1" {
		t.Errorf("explicit option = %+v", got)
	}

	// An option the agent never offered must not be sent back.
	if got := OutcomeForDecision(options, true, "nope"); got.Outcome != outcomeCancelled {
		t.Errorf("unknown option = %+v, want cancelled", got)
	}

	// No option of the requested kind: cancel rather than invent an approval.
	if got := OutcomeForDecision(nil, true, ""); got.Outcome != outcomeCancelled {
		t.Errorf("no options = %+v, want cancelled", got)
	}
}

func TestCloseFailsPendingCalls(t *testing.T) {
	client, agent := newPair(t, Options{})

	received := make(chan struct{})
	agent.setHandler(func(msg rpcMessage) {
		if msg.Method == methodSessionNew {
			close(received)
		}
	})

	errCh := make(chan error, 1)
	go func() {
		_, err := client.NewSession(context.Background(), NewSessionRequest{Cwd: "/tmp"})
		errCh <- err
	}()

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("the agent never saw the request")
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("a pending call must fail when the connection closes")
		}
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the pending call was never released")
	}
}

func TestAgentExitSurfacesError(t *testing.T) {
	client, agent := newPair(t, Options{})

	agent.close()

	select {
	case <-client.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the client did not notice the agent exiting")
	}
	if client.Err() == nil {
		t.Fatal("Err() should report why the connection ended")
	}

	if _, err := client.NewSession(context.Background(), NewSessionRequest{Cwd: "/tmp"}); err == nil {
		t.Fatal("a call after the agent exits must fail")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
