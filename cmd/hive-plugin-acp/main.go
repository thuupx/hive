// Command hive-plugin-acp exposes an ACP agent as a Hive agent plugin.
//
// It is a separate process by design: a plugin does not link the core, so an
// agent protocol change is contained to this adapter.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/thupham/hive/plugins/acpbridge"
	"github.com/thupham/hive/plugins/sdk"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hive-plugin-acp:", err)
		os.Exit(1)
	}
}

func run() error {
	id := flag.String("id", "acp", "stable plugin identity")
	agentCommand := flag.String("agent-command", "", "agent invocation, space separated, for example \"devin acp\"")
	version := flag.String("version", "0.1.0-dev", "plugin version")
	authMethod := flag.String("auth-method", "",
		"which of the agent's advertised ACP auth methods to use; empty uses the first")
	apiKeyEnv := flag.String("api-key-env", "",
		"environment variable holding an API key, passed to the agent as _meta.api_key")
	flag.Parse()

	if strings.TrimSpace(*agentCommand) == "" {
		return fmt.Errorf("-agent-command is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	host, err := sdk.Connect(ctx, sdk.Stdio(), sdk.Options{
		ID:      *id,
		Type:    v1.PluginTypeAgent,
		Version: *version,
		Capabilities: []string{
			v1.CapabilityEventWrite,
			v1.CapabilityPermissionWrite,
			v1.CapabilityExecutionWrite,
		},
	})
	if err != nil {
		return err
	}
	defer host.Close()

	// The API key is read from the environment, so it never appears in
	// configuration and never reaches a log.
	var apiKey string
	if *apiKeyEnv != "" {
		apiKey = os.Getenv(*apiKeyEnv)
	}

	// The logger is what makes a restore that did not work visible. Without it the
	// bridge discarded its own warnings, including the one that explains why an
	// agent answered as if it had never spoken to the user before.
	bridge := acpbridge.New(host, acpbridge.CommandLauncher{
		Command:       strings.Fields(*agentCommand),
		ClientVersion: *version,
	}, acpbridge.Options{
		AuthMethod: *authMethod,
		APIKey:     apiKey,
		Log:        slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	bridge.Register()

	return bridge.Run(ctx)
}
