// Command plugin-echo is a minimal Hive plugin.
//
// It is a template for plugin authors: handshake, capability registration, a
// handler for core-invoked methods, and a call back into the core.
//
// A plugin is a separate process that depends on the public protocol surface. It
// imports no internal Hive package, so an integration cannot break when the core
// is refactored.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/thuupx/hive/plugins/sdk"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// capabilities are what this plugin asks for. The core intersects the request with
// what the plugin's type may ever hold, so asking for more does not grant more.
var capabilities = []string{
	v1.CapabilityAgentRead,
	v1.CapabilitySessionRead,
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "plugin-echo:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	host, err := sdk.Connect(ctx, sdk.Stdio(), sdk.Options{
		ID:           "echo",
		Type:         v1.PluginTypeUI,
		Version:      "0.1.0",
		Capabilities: capabilities,
	})
	if err != nil {
		return err
	}
	defer host.Close()

	// A capability the core did not grant is a capability this plugin must not
	// assume it has.
	if !host.Granted(v1.CapabilitySessionRead) {
		return fmt.Errorf("the core did not grant %s", v1.CapabilitySessionRead)
	}

	// A core-invoked method. The handler returns a result, which becomes the
	// response to the core's request.
	host.Handle("echo.ping", func(_ context.Context, params json.RawMessage) (any, error) {
		var request struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, v1.InvalidParams("invalid echo.ping request")
		}
		return map[string]any{"text": request.Text}, nil
	})

	log.Info("echo plugin ready", "instance", host.InstanceID(), "generation", host.ConnectionGeneration())

	// A call into the core, using a granted capability.
	var sessions v1.SessionListResult
	if err := host.Call(ctx, v1.MethodSessionList, v1.SessionListParams{Limit: 5}, &sessions); err != nil {
		log.Warn("could not list sessions", "error", err)
	} else {
		log.Info("sessions visible to this plugin", "count", len(sessions.Sessions))
	}

	// Serve until the core goes away or the process is asked to stop.
	return host.Run(ctx)
}
