// Command hive-plugin-acp exposes an ACP agent as a Hive agent plugin.
//
// It is a separate process by design: a plugin does not link the core, so an
// agent protocol change is contained to this adapter.
package main

import (
	"context"
	"flag"
	"fmt"
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

	bridge := acpbridge.New(host, acpbridge.CommandLauncher{
		Command:       strings.Fields(*agentCommand),
		ClientVersion: *version,
	})
	bridge.Register()

	return bridge.Run(ctx)
}
