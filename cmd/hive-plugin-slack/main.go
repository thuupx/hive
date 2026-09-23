// Command hive-plugin-slack runs the Slack transport.
//
// It is a separate process by design: a transport plugin does not link the core,
// and a Slack failure cannot take the coordinator down with it.
//
// Credentials come from the environment so they never appear in configuration:
//
//	SLACK_APP_TOKEN  authenticates the Socket Mode connection
//	SLACK_BOT_TOKEN  authenticates Web API calls
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

	"github.com/thupham/hive/plugins/sdk"
	"github.com/thupham/hive/plugins/slack"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// Capabilities the transport needs. It cannot grant itself more: the core
// intersects this with what a transport may ever hold.
var capabilities = []string{
	v1.CapabilityTransportInbound,
	v1.CapabilitySessionRead,
	v1.CapabilitySessionWrite,
	v1.CapabilityAgentRead,
	v1.CapabilityEventRead,
	v1.CapabilityPermissionRead,
	v1.CapabilityPermissionWrite,
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hive-plugin-slack:", err)
		os.Exit(1)
	}
}

func run() error {
	id := flag.String("id", slack.Name, "stable plugin identity")
	version := flag.String("version", "0.1.0-dev", "plugin version")
	options := optionFlags{}
	flag.Var(&options, "option", "transport option as key=value; repeatable")
	flag.Parse()

	// The plugin does not link the core, so it logs with the standard library
	// rather than the core logging package.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	host, err := sdk.Connect(ctx, sdk.Stdio(), sdk.Options{
		ID:           *id,
		Type:         v1.PluginTypeTransport,
		Version:      *version,
		Capabilities: capabilities,
	})
	if err != nil {
		return err
	}
	defer host.Close()

	// A capability the core did not grant is a capability this transport must not
	// assume it has.
	if !host.Granted(v1.CapabilityTransportInbound) {
		return fmt.Errorf("the core did not grant %s", v1.CapabilityTransportInbound)
	}

	client, err := slack.NewSocketClient(slack.Config{
		AppToken: os.Getenv("SLACK_APP_TOKEN"),
		BotToken: os.Getenv("SLACK_BOT_TOKEN"),
	})
	if err != nil {
		return err
	}
	defer client.Close()

	plugin := slack.New(host, client, slack.Options{
		BotUserID:      options.get("bot_user_id"),
		RequireMention: options.bool("require_mention", true),
		Acknowledgement: slack.Acknowledgement{
			Enabled:  options.bool("acknowledgement", false),
			Reaction: options.get("acknowledgement_reaction"),
		},
		Log: log,
	})

	log.Info("slack transport starting", "plugin", *id, "instance", host.InstanceID())
	return plugin.Run(ctx)
}

// optionFlags collects repeated -option key=value arguments.
//
// Options are opaque to the core: it passes them through, so Hive holds no
// vendor-specific configuration fields.
type optionFlags map[string]string

func (o *optionFlags) String() string {
	if o == nil {
		return ""
	}
	parts := make([]string, 0, len(*o))
	for key, value := range *o {
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, ",")
}

func (o *optionFlags) Set(value string) error {
	key, val, ok := strings.Cut(value, "=")
	if !ok {
		return fmt.Errorf("option %q must be key=value", value)
	}
	if *o == nil {
		*o = make(optionFlags)
	}
	(*o)[key] = val
	return nil
}

func (o optionFlags) get(key string) string { return o[key] }

func (o optionFlags) bool(key string, fallback bool) bool {
	value, ok := o[key]
	if !ok {
		return fallback
	}
	switch strings.ToLower(value) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return fallback
	}
}
