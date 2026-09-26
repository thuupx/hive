package sdk

import (
	"context"
	"os"
	"testing"
	"time"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// A plugin that stops for its own reason must still be able to exit.
//
// Host.Close closes the stream and waits for the read loop, so it can only
// return if closing the stream ends the read. A transport whose own connection
// dropped returns from Run while the core is still connected, so the read is
// still waiting and only closing the plugin's own reader ends it.
//
// Before the standard streams were closers this waited forever: the process
// never exited, and the supervisor restarts a plugin only on exit.
func TestCloseEndsAReadOnAStillConnectedCore(t *testing.T) {
	coreRead, fromPlugin, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	toPlugin, coreWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer coreRead.Close()
	defer coreWrite.Close()

	// The core answers the handshake and then stays connected.
	core := v1.NewPeer(v1.NewStream(coreRead, coreWrite, coreRead, coreWrite))
	core.Start()
	defer core.Close()

	go func() {
		for req := range core.Requests() {
			if req.Method == v1.MethodPluginHello {
				_ = core.Respond(req.RequestID(), v1.HelloResponse{
					InstanceID:           "inst_1",
					ConnectionGeneration: 1,
				})
			}
		}
	}()

	host, err := Connect(context.Background(), stdioStream(toPlugin, fromPlugin), Options{
		ID:      "test",
		Type:    v1.PluginTypeAgent,
		Version: "test",
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- host.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return while the core was still connected")
	}
}
