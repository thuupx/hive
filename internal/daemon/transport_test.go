package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thupham/hive/internal/daemon"
	"github.com/thupham/hive/internal/node"
	"github.com/thupham/hive/internal/plugin"
	"github.com/thupham/hive/internal/storage"
	"github.com/thupham/hive/plugins/sdk"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// runTestTransport delivers one normalized inbound delivery and reports the
// outcome, so the transport boundary is exercised through the real plugin link.
func runTestTransport() {
	ctx := context.Background()

	host, err := sdk.Connect(ctx, sdk.Stdio(), sdk.Options{
		ID:      "slack",
		Type:    v1.PluginTypeTransport,
		Version: "test",
		Capabilities: []string{
			v1.CapabilityTransportInbound,
			v1.CapabilitySessionRead,
			v1.CapabilitySessionWrite,
			v1.CapabilityAgentRead,
			v1.CapabilityEventRead,
		},
	})
	if err != nil {
		os.Exit(2)
	}

	var params v1.TransportInboundParams
	if err := json.Unmarshal([]byte(os.Getenv("HIVE_TEST_TRANSPORT")), &params); err != nil {
		os.Exit(3)
	}

	// When asked, subscribe before delivering so the event the delivery causes is
	// observed, then acknowledge it with the conversation. That is what advances
	// the durable binding cursor.
	//
	// The SDK loop pumps delivered events, so it has to run for Events() to
	// receive anything.
	var subscription v1.SubscribeResponse
	if os.Getenv("HIVE_TEST_TRANSPORT_ACK") != "" {
		runCtx, stopRun := context.WithCancel(ctx)
		defer stopRun()
		go func() { _ = host.Run(runCtx) }()

		var err error
		// The session does not exist yet: it is created by the delivery below. So
		// the helper subscribes to everything and takes the first delivery.
		subscription, err = host.Subscribe(ctx, v1.SubscribeRequest{})
		if err != nil {
			os.Exit(4)
		}
	}

	var outcome v1.TransportOutcome
	callErr := host.Call(ctx, v1.MethodTransportInbound, params, &outcome)

	if subscription.SubscriptionID != "" {
		select {
		case delivered := <-host.Events():
			_ = host.AckFor(ctx, subscription.SubscriptionID, delivered.Event.Sequence, params.ConversationID)
		case <-time.After(10 * time.Second):
			os.Exit(5)
		}
	}

	writeTransportResult(callErr, outcome)
	_ = host.Close()
}

func writeTransportResult(callErr error, outcome v1.TransportOutcome) {
	path := os.Getenv("HIVE_TEST_OUT")
	if path == "" {
		return
	}

	payload := map[string]any{"outcome": outcome}
	if callErr != nil {
		e := v1.AsError(callErr)
		payload["error"] = e.Message
		payload["errorCode"] = e.Code
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, encoded, 0o600)
}

// transportSpec runs the test binary once as a transport plugin.
func transportSpec(t *testing.T, params v1.TransportInboundParams, out string, ack bool) plugin.Spec {
	t.Helper()

	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}

	return plugin.Spec{
		ID:      "slack",
		Type:    v1.PluginTypeTransport,
		Version: "test",
		Command: []string{os.Args[0]},
		Env: []string{
			"HIVE_TEST_TRANSPORT=" + string(encoded),
			"HIVE_TEST_OUT=" + out,
			"HIVE_TEST_TRANSPORT_ACK=" + ackFlag(ack),
		},
		// The scenario is a single delivery, so a restart would repeat it.
		MaxRestarts: 0,
	}
}

// startStackWithTransport brings up a coordinator, a node with the test agent,
// and a transport plugin that delivers one envelope.
// ackFlag renders the acknowledgement switch for the helper process.
func ackFlag(enabled bool) string {
	if enabled {
		return "1"
	}
	return ""
}

func startStackWithTransport(t *testing.T, params v1.TransportInboundParams) (*storage.Store, v1.TransportOutcome, error) {
	return startStackWithTransportAck(t, params, false)
}

func startStackWithTransportAck(t *testing.T, params v1.TransportInboundParams, ack bool) (*storage.Store, v1.TransportOutcome, error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	coordinatorStore := openCoordinatorStore(t)

	cert, err := node.LoadOrCreateCertificate(t.TempDir())
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}

	outcomePath := filepath.Join(t.TempDir(), "transport.json")

	coordinator, err := daemon.NewCoordinator(daemon.CoordinatorOptions{
		Store:        coordinatorStore,
		Listen:       "127.0.0.1:0",
		Certificate:  cert,
		Agents:       []string{testAgent},
		DefaultAgent: testAgent,
		AllowedUsers: []string{"slack:U123"},
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

	// A transport can be started after the coordinator is up, which is also what
	// happens when a transport is enabled later.
	if _, err := coordinator.Supervisor().Start(ctx, transportSpec(t, params, outcomePath, ack)); err != nil {
		t.Fatalf("start the transport plugin: %v", err)
	}

	waitFor(t, "the transport delivery", func() bool {
		_, err := os.Stat(outcomePath)
		return err == nil
	})

	raw, err := os.ReadFile(outcomePath)
	if err != nil {
		t.Fatalf("read the transport outcome: %v", err)
	}

	var result struct {
		Outcome   v1.TransportOutcome `json:"outcome"`
		Error     string              `json:"error"`
		ErrorCode int                 `json:"errorCode"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode the transport outcome: %v", err)
	}
	var callErr error
	if result.Error != "" {
		callErr = v1.NewErrorf(result.ErrorCode, "%s", result.Error)
	}
	return coordinatorStore, result.Outcome, callErr
}

// A transport command becomes a Hive operation: the transport owns the syntax and
// Hive owns the semantics.
func TestTransportCommandCreatesASession(t *testing.T) {
	store, outcome, callErr := startStackWithTransport(t, v1.TransportInboundParams{
		Transport:      "slack",
		ConversationID: "C123",
		Principal:      "slack:U123",
		SourceID:       "event:C123:1700000000.000100",
		Kind:           "command",
		Command:        "new_chat",
		Method:         v1.MethodSessionCreate,
	})

	if callErr != nil {
		t.Fatalf("transport delivery failed: %v", callErr)
	}
	if outcome.Method != v1.MethodSessionCreate {
		t.Fatalf("outcome = %+v", outcome)
	}
	if outcome.SessionID == "" || outcome.RunID == "" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if outcome.Error != nil {
		t.Fatalf("outcome error = %v", outcome.Error)
	}

	// The conversation is bound to the session, so later deliveries reach it.
	ctx := context.Background()
	binding, err := store.GetBinding(ctx, "slack", "C123")
	if err != nil {
		t.Fatalf("GetBinding: %v", err)
	}
	if binding.SessionID != outcome.SessionID {
		t.Fatalf("binding session = %q, want %q", binding.SessionID, outcome.SessionID)
	}
	if binding.Principal != "slack:U123" {
		t.Errorf("binding principal = %q", binding.Principal)
	}

	// The command id derives from the transport delivery, so a redelivery of the
	// same platform event resolves to the same operation.
	want := "slack:event:C123:1700000000.000100:" + v1.MethodSessionCreate
	if outcome.CommandID != want {
		t.Errorf("command id = %q, want %q", outcome.CommandID, want)
	}
}

// A plain message becomes a prompt, and a conversation with no session yet gets
// one, because the first thing a user says has to land somewhere.
func TestTransportMessagePromptsAndCreatesASession(t *testing.T) {
	store, outcome, callErr := startStackWithTransport(t, v1.TransportInboundParams{
		Transport:      "slack",
		ConversationID: "C123",
		Principal:      "slack:U123",
		SourceID:       "event:C123:1700000000.000200",
		Kind:           "message",
		Text:           "fix the login bug",
	})

	if callErr != nil {
		t.Fatalf("transport delivery failed: %v", callErr)
	}
	if outcome.Method != v1.MethodSessionPrompt {
		t.Fatalf("outcome = %+v", outcome)
	}
	if outcome.SessionID == "" || outcome.RunID == "" {
		t.Fatalf("outcome = %+v", outcome)
	}

	ctx := context.Background()

	// The session was created implicitly and bound to the conversation.
	binding, err := store.GetBinding(ctx, "slack", "C123")
	if err != nil {
		t.Fatalf("GetBinding: %v", err)
	}
	if binding.SessionID != outcome.SessionID {
		t.Fatalf("binding session = %q, want %q", binding.SessionID, outcome.SessionID)
	}

	runs, err := store.ListAgentRuns(ctx, outcome.SessionID)
	if err != nil {
		t.Fatalf("ListAgentRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want the implicit session to have exactly one run", len(runs))
	}

	// The agent received the prompt: the test agent publishes an event for it.
	waitFor(t, "the agent event to become durable", func() bool {
		n, err := store.EventCount(ctx, outcome.SessionID)
		return err == nil && n >= 1
	})
}

// A transport may only speak for its own transport, and only assert a principal
// of that transport.
func TestTransportCannotSpeakForAnotherTransport(t *testing.T) {
	_, _, callErr := startStackWithTransport(t, v1.TransportInboundParams{
		Transport:      "telegram",
		ConversationID: "C123",
		Principal:      "telegram:1",
		Kind:           "message",
		Text:           "hello",
	})

	if callErr == nil {
		t.Fatal("expected the delivery to be refused")
	}
	if e := v1.AsError(callErr); e.Code != v1.CodeUnauthorized {
		t.Fatalf("code = %d, want %d", e.Code, v1.CodeUnauthorized)
	}
}

func TestTransportCannotAssertAnotherTransportsPrincipal(t *testing.T) {
	_, _, callErr := startStackWithTransport(t, v1.TransportInboundParams{
		Transport:      "slack",
		ConversationID: "C123",
		Principal:      "telegram:1",
		Kind:           "message",
		Text:           "hello",
	})

	if callErr == nil {
		t.Fatal("expected the assertion to be refused")
	}
	if e := v1.AsError(callErr); e.Code != v1.CodeUnauthorized {
		t.Fatalf("code = %d, want %d", e.Code, v1.CodeUnauthorized)
	}
}

// A recognized command the transport does not expose is reported as a command
// error, not forwarded to the agent as a prompt.
func TestTransportUnknownCommandIsACommandError(t *testing.T) {
	_, outcome, callErr := startStackWithTransport(t, v1.TransportInboundParams{
		Transport:      "slack",
		ConversationID: "C123",
		Principal:      "slack:U123",
		Kind:           "command",
		Command:        "teleport",
		Method:         "",
	})
	if callErr != nil {
		t.Fatalf("a command error is an outcome, not a protocol error: %v", callErr)
	}

	if outcome.Error == nil {
		t.Fatal("expected a command error")
	}
	if outcome.Error.Code != v1.CodeMethodNotFound {
		t.Fatalf("code = %d, want %d", outcome.Error.Code, v1.CodeMethodNotFound)
	}
	if outcome.SessionID != "" {
		t.Fatal("a rejected command must not create a session")
	}
}

// A transport that acknowledges a delivery advances its durable binding cursor, so
// a restarted transport resumes instead of replaying everything.
func TestTransportAcknowledgementAdvancesTheBindingCursor(t *testing.T) {
	ctx := context.Background()

	store, outcome, callErr := startStackWithTransportAck(t, v1.TransportInboundParams{
		Transport:      "slack",
		ConversationID: "C123",
		Principal:      "slack:U123",
		SourceID:       "event:C123:1700000000.000300",
		Kind:           "message",
		Text:           "fix the login bug",
	}, true)
	if callErr != nil {
		t.Fatalf("transport delivery failed: %v", callErr)
	}
	if outcome.SessionID == "" {
		t.Fatalf("outcome = %+v", outcome)
	}

	waitFor(t, "the binding cursor to advance", func() bool {
		binding, err := store.GetBinding(ctx, "slack", "C123")
		return err == nil && binding.EventCursor >= 1
	})

	binding, err := store.GetBinding(ctx, "slack", "C123")
	if err != nil {
		t.Fatalf("GetBinding: %v", err)
	}
	if binding.SessionID != outcome.SessionID {
		t.Errorf("binding session = %q, want %q", binding.SessionID, outcome.SessionID)
	}
}

// A picture sent to the transport reaches the agent as a picture.
//
// The transport fetches it, because reading the platform is its job, and the core
// carries only the bytes.
func TestTransportImageReachesTheAgent(t *testing.T) {
	store, outcome, callErr := startStackWithTransport(t, v1.TransportInboundParams{
		Transport:      "slack",
		ConversationID: "C123",
		Principal:      "slack:U123",
		SourceID:       "event:C123:1700000000.000400",
		Kind:           "message",
		Text:           "what is in this picture?",
		Attachments: []v1.Attachment{{
			Name:     "shot.png",
			MimeType: "image/png",
			Data:     []byte{0x89, 0x50, 0x4e, 0x47},
		}},
	})
	if callErr != nil {
		t.Fatalf("transport delivery failed: %v", callErr)
	}
	if outcome.SessionID == "" {
		t.Fatalf("outcome = %+v", outcome)
	}

	// The agent reports what it received, so the assertion is about the agent and
	// not about the transport.
	waitFor(t, "the agent to report the prompt it received", func() bool {
		_, ok := receivedPrompt(t, store, outcome.SessionID)
		return ok
	})

	received, ok := receivedPrompt(t, store, outcome.SessionID)
	if !ok {
		t.Fatal("the agent never reported the prompt")
	}
	if received["prompt"] != "what is in this picture?" {
		t.Errorf("prompt = %v", received["prompt"])
	}
	if received["images"] != float64(1) {
		t.Errorf("images = %v, want 1", received["images"])
	}
	if received["mime"] != "image/png" {
		t.Errorf("mime = %v", received["mime"])
	}
}

// receivedPrompt decodes what the test agent reported receiving.
func receivedPrompt(t *testing.T, store *storage.Store, sessionID string) (map[string]any, bool) {
	t.Helper()
	ctx := context.Background()

	events, err := store.ReadEvents(ctx, sessionID, 0, 100)
	if err != nil {
		return nil, false
	}
	for _, ev := range events {
		if ev.Type != "agent.raw" {
			continue
		}

		var payload map[string]any
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			continue
		}
		if _, ok := payload["images"]; ok {
			return payload, true
		}
	}
	return nil, false
}
