package plugin_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thuupx/hive/internal/event"
	"github.com/thuupx/hive/internal/eventbus"
	"github.com/thuupx/hive/internal/plugin"
	"github.com/thuupx/hive/plugins/sdk"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// The test binary doubles as a plugin process. Re-executing it keeps the test
// hermetic: no separate build step and no fixture binary to keep in sync.
func TestMain(m *testing.M) {
	if os.Getenv("HIVE_TEST_PLUGIN") != "" {
		runHelperPlugin()
		return
	}
	os.Exit(m.Run())
}

func runHelperPlugin() {
	mode := os.Getenv("HIVE_TEST_PLUGIN")
	stream := sdk.Stdio()
	ctx := context.Background()

	switch mode {
	case "silent":
		// Never say hello.
		time.Sleep(30 * time.Second)
		return

	case "bad-version":
		req, _ := v1.NewRequest(v1.NumberID(1), v1.MethodPluginHello, v1.HelloRequest{
			PluginID:        "test-plugin",
			Type:            v1.PluginTypeTransport,
			Version:         "test",
			ProtocolVersion: v1.ProtocolVersion{Family: v1.ProtocolFamily, Major: 99},
		})
		_ = stream.Write(req)
		time.Sleep(30 * time.Second)
		return
	}

	id := "test-plugin"
	if mode == "bad-identity" {
		id = "someone-else"
	}

	host, err := sdk.Connect(ctx, stream, sdk.Options{
		ID:      id,
		Type:    v1.PluginTypeTransport,
		Version: "test",
		Capabilities: []string{
			v1.CapabilitySessionRead,
			v1.CapabilitySessionWrite,
			v1.CapabilityEventRead,
			v1.CapabilityPermissionWrite,
		},
	})
	if err != nil {
		os.Exit(2)
	}

	if mode == "crash" {
		os.Exit(3)
	}

	host.Handle(v1.MethodPluginHealth, func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"status": "ok"}, nil
	})

	if mode == "caller" {
		var out map[string]any
		_ = host.Call(ctx, v1.MethodSessionList, nil, &out)
		_ = host.Close()
		return
	}

	if mode == "unauthorized" {
		var out map[string]any
		err := host.Call(ctx, v1.MethodExecutionReport, nil, &out)
		writeResult(err)
		_ = host.Close()
		return
	}

	if mode == "subscriber" {
		resp, err := host.Subscribe(ctx, v1.SubscribeRequest{SessionID: "sess_1"})
		if err != nil {
			os.Exit(4)
		}
		go func() {
			for ev := range host.Events() {
				_ = host.Ack(ctx, resp.SubscriptionID, ev.Event.Sequence)
			}
		}()
	}

	_ = host.Run(ctx)
}

// writeResult reports an outcome back to the test through a file, because the
// plugin process cannot otherwise be observed.
func writeResult(err error) {
	path := os.Getenv("HIVE_TEST_OUT")
	if path == "" {
		return
	}
	msg := "ok"
	if err != nil {
		msg = err.Error()
	}
	_ = os.WriteFile(path, []byte(msg), 0o600)
}

func helperSpec(mode string, maxRestarts int) plugin.Spec {
	return plugin.Spec{
		ID:           "test-plugin",
		Type:         v1.PluginTypeTransport,
		Version:      "test",
		Command:      []string{os.Args[0]},
		Env:          []string{"HIVE_TEST_PLUGIN=" + mode},
		MaxRestarts:  maxRestarts,
		RestartDelay: 20 * time.Millisecond,
	}
}

func newSupervisor(t *testing.T) *plugin.Supervisor {
	t.Helper()
	s := plugin.NewSupervisor(nil)
	s.HandshakeTimeout = 2 * time.Second
	t.Cleanup(s.StopAll)
	return s
}

func TestStartCompletesHandshake(t *testing.T) {
	s := newSupervisor(t)

	inst, err := s.Start(context.Background(), helperSpec("ok", 0))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if inst.PluginID != "test-plugin" {
		t.Errorf("plugin id = %q", inst.PluginID)
	}
	if inst.ConnectionGeneration != 1 {
		t.Errorf("generation = %d, want 1", inst.ConnectionGeneration)
	}
	if inst.ID == "" {
		t.Error("instance id is required")
	}

	// Only capabilities the plugin type may hold are granted.
	if !hasCapability(inst.Capabilities, v1.CapabilitySessionWrite) {
		t.Errorf("capabilities = %v, want session.write", inst.Capabilities)
	}
	if hasCapability(inst.Capabilities, v1.CapabilityExecutionWrite) {
		t.Errorf("capabilities = %v, a transport plugin must not hold execution.write", inst.Capabilities)
	}

	// The plugin can be invoked on the core-to-plugin direction.
	var health map[string]any
	if err := inst.Call(context.Background(), v1.MethodPluginHealth, nil, &health); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if health["status"] != "ok" {
		t.Errorf("health = %v", health)
	}
}

func TestStartRejectsWrongIdentity(t *testing.T) {
	s := newSupervisor(t)

	if _, err := s.Start(context.Background(), helperSpec("bad-identity", 0)); err == nil {
		t.Fatal("expected a mismatched plugin identity to be rejected")
	}
}

func TestStartRejectsIncompatibleProtocolVersion(t *testing.T) {
	s := newSupervisor(t)

	if _, err := s.Start(context.Background(), helperSpec("bad-version", 0)); err == nil {
		t.Fatal("expected an incompatible protocol version to be rejected")
	}
}

func TestStartTimesOutOnASilentPlugin(t *testing.T) {
	s := newSupervisor(t)
	s.HandshakeTimeout = 200 * time.Millisecond

	start := time.Now()
	if _, err := s.Start(context.Background(), helperSpec("silent", 0)); err == nil {
		t.Fatal("expected the handshake to time out")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("handshake timeout took %v", elapsed)
	}
}

func TestStartRejectsInvalidSpec(t *testing.T) {
	s := newSupervisor(t)

	cases := map[string]plugin.Spec{
		"no id":       {Type: v1.PluginTypeTransport, Command: []string{os.Args[0]}},
		"no command":  {ID: "p", Type: v1.PluginTypeTransport},
		"bad type":    {ID: "p", Type: "wizard", Command: []string{os.Args[0]}},
		"bad restart": {ID: "p", Type: v1.PluginTypeTransport, Command: []string{os.Args[0]}, MaxRestarts: -1},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Start(context.Background(), spec); err == nil {
				t.Fatal("expected the spec to be rejected")
			}
		})
	}
}

func TestDuplicateStartIsRejected(t *testing.T) {
	s := newSupervisor(t)
	ctx := context.Background()

	if _, err := s.Start(ctx, helperSpec("ok", 0)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := s.Start(ctx, helperSpec("ok", 0)); err == nil {
		t.Fatal("a plugin identity must not be supervised twice")
	}
}

// A plugin crash must not terminate the coordinator or unrelated work, and the
// supervisor restarts it according to policy.
func TestCrashIsContainedAndRestarted(t *testing.T) {
	s := newSupervisor(t)
	ctx := context.Background()

	first, err := s.Start(ctx, helperSpec("crash", 2))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The supervisor must stay usable while the plugin dies and restarts.
	deadline := time.Now().Add(5 * time.Second)
	var second *plugin.Instance
	for time.Now().Before(deadline) {
		if inst, ok := s.Instance("test-plugin"); ok && inst.ID != first.ID {
			second = inst
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if second == nil {
		t.Fatal("the plugin was not restarted")
	}
	if second.ConnectionGeneration != 2 {
		t.Fatalf("generation = %d, want 2", second.ConnectionGeneration)
	}
	if s.Restarts("test-plugin") == 0 {
		t.Fatal("Restarts should report the restart")
	}
}

// A superseded connection must not keep invoking capabilities after a restart.
func TestSupersededInstanceCannotInvokeCapabilities(t *testing.T) {
	s := newSupervisor(t)
	ctx := context.Background()

	first, err := s.Start(ctx, helperSpec("crash", 1))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Authorize(first, v1.MethodSessionList); err != nil {
		t.Fatalf("the current instance should be authorized: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if inst, ok := s.Instance("test-plugin"); ok && inst.ID != first.ID {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := s.Authorize(first, v1.MethodSessionList); err == nil {
		t.Fatal("a superseded instance must not be authorized")
	}
	select {
	case <-first.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the superseded connection should be closed")
	}
}

func TestCapabilityAuthorization(t *testing.T) {
	s := newSupervisor(t)

	inst, err := s.Start(context.Background(), helperSpec("ok", 0))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Granted by the transport grant table.
	if err := s.Authorize(inst, v1.MethodSessionPrompt); err != nil {
		t.Errorf("session.prompt should be authorized: %v", err)
	}
	// A method whose capability the plugin never asked for.
	if err := s.Authorize(inst, v1.MethodExecutionReport); err == nil {
		t.Error("execution.report should be denied")
	}
	// A method outside the plugin API entirely.
	if err := s.Authorize(inst, "cluster.elect"); err == nil {
		t.Error("an unknown method should be denied")
	}
	if err := s.Authorize(nil, v1.MethodSessionList); err == nil {
		t.Error("a nil instance should be denied")
	}
}

func TestGrantIntersectsWithPluginType(t *testing.T) {
	got := plugin.Grant(v1.PluginTypeAgent, []string{
		v1.CapabilityEventWrite,
		v1.CapabilitySessionWrite, // not grantable to an agent plugin
		v1.CapabilityExecutionWrite,
		v1.CapabilityEventWrite, // duplicate
		"invented.capability",   // unknown
	})
	want := []string{v1.CapabilityEventWrite, v1.CapabilityExecutionWrite}
	if len(got) != len(want) {
		t.Fatalf("grant = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("grant = %v, want %v", got, want)
		}
	}
}

func TestPluginCanInvokeAnAuthorizedCoreMethod(t *testing.T) {
	s := newSupervisor(t)

	called := make(chan struct{}, 1)
	s.Handle(v1.MethodSessionList, func(context.Context, plugin.Call) (any, error) {
		called <- struct{}{}
		return map[string]any{"sessions": []string{}}, nil
	})

	if _, err := s.Start(context.Background(), helperSpec("caller", 0)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("the core handler was not invoked")
	}
}

// A plugin may only invoke methods its granted capabilities permit.
func TestUnauthorizedPluginCallIsRefused(t *testing.T) {
	s := newSupervisor(t)

	reached := make(chan struct{}, 1)
	s.Handle(v1.MethodExecutionReport, func(context.Context, plugin.Call) (any, error) {
		reached <- struct{}{}
		return nil, nil
	})

	out := filepath.Join(t.TempDir(), "result.txt")
	spec := helperSpec("unauthorized", 0)
	spec.Env = append(spec.Env, "HIVE_TEST_OUT="+out)

	if _, err := s.Start(context.Background(), spec); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(out); err == nil {
			if !strings.Contains(string(b), strconv.Itoa(v1.CodeUnauthorized)) {
				t.Fatalf("plugin saw %q, want an unauthorized error", b)
			}
			select {
			case <-reached:
				t.Fatal("an unauthorized call reached the handler")
			default:
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the plugin never reported its result")
}

func TestEventSubscriptionDeliversAndAcknowledges(t *testing.T) {
	bus := eventbus.NewBus()
	defer bus.Close()

	s := newSupervisor(t)
	events := plugin.NewEvents(bus, s, nil)
	events.Register(s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = events.Run(ctx) }()

	inst, err := s.Start(ctx, helperSpec("subscriber", 0))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the plugin to subscribe.
	deadline := time.Now().Add(5 * time.Second)
	for events.SubscriptionCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if events.SubscriptionCount() == 0 {
		t.Fatal("the plugin never subscribed")
	}

	for i := int64(1); i <= 3; i++ {
		bus.Publish(&event.Event{
			ID:        "ev_" + string(rune('0'+i)),
			Sequence:  i,
			SessionID: "sess_1",
			Type:      event.TypeMessage,
			Version:   1,
			Payload:   []byte(`{}`),
		})
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if events.AckedThrough(inst) == 3 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("acked through %d, want 3", events.AckedThrough(inst))
}

func TestEventSubscriptionScopesToSession(t *testing.T) {
	bus := eventbus.NewBus()
	defer bus.Close()

	s := newSupervisor(t)
	events := plugin.NewEvents(bus, s, nil)
	events.Register(s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = events.Run(ctx) }()

	inst, err := s.Start(ctx, helperSpec("subscriber", 0))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for events.SubscriptionCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// The plugin subscribed to sess_1 only.
	bus.Publish(&event.Event{
		ID: "other", Sequence: 1, SessionID: "sess_other",
		Type: event.TypeMessage, Version: 1, Payload: []byte(`{}`),
	})

	time.Sleep(200 * time.Millisecond)
	if got := events.AckedThrough(inst); got != 0 {
		t.Fatalf("acked through %d, want 0 for another session", got)
	}
}

func hasCapability(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}
