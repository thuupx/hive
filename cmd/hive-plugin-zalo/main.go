// Command hive-plugin-zalo runs the Zalo Bot transport.
//
// It is a separate process by design: a transport plugin does not link the core,
// and a Zalo failure cannot take the coordinator down with it.
//
// Inbound messages arrive by long polling, so the transport needs no public
// endpoint: a personal installation behind NAT works as it is.
//
// Credentials come from the environment so they never appear in configuration:
//
//	ZALO_BOT_TOKEN  the bot token, "<bot id>:<secret>"
//	ZALO_TOKEN      an accepted alias for the same token
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/thuupx/hive/internal/logging"
	"github.com/thuupx/hive/plugins/sdk"
	"github.com/thuupx/hive/plugins/zalo"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
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
		fmt.Fprintln(os.Stderr, "hive-plugin-zalo:", err)
		os.Exit(1)
	}
}

func run() error {
	id := flag.String("id", zalo.Name, "stable plugin identity")
	version := flag.String("version", "0.0.0-dev", "plugin version")
	describe := flag.Bool(sdk.DescribeFlag, false, "print the plugin manifest and exit")
	options := optionFlags{}
	flag.Var(&options, "option", "transport option as key=value; repeatable")
	flag.Parse()

	// The core asks a plugin what it needs without starting it, so the manifest
	// is printed before the connection is attempted.
	if *describe {
		return sdk.Describe(zalo.Manifest(*version))
	}

	// The plugin does not link the core, so it logs with the standard library
	// rather than the core logging package.
	log, closeLog := logging.NewForProcess()
	defer closeLog()

	// A misspelled option is passed through and would otherwise do nothing
	// silently, which reads as a setting that does not work.
	if unknown := sdk.UnknownOptions(zalo.Manifest(*version), options); len(unknown) > 0 {
		log.Warn("ignoring options this transport does not read",
			"options", strings.Join(unknown, ", "))
	}

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

	client, err := zalo.NewHTTPClient(zalo.Config{
		Token: botToken(),
		Log:   log,
	})
	if err != nil {
		return err
	}
	defer client.Close()

	plugin := zalo.New(host, client, zalo.Options{
		BotName:        options.get("bot_name"),
		RequireMention: options.bool("require_mention", false),

		// A transport must not read an unbounded amount of a user's data because
		// they attached something large.
		MaxAttachmentBytes: int64(options.int("max_attachment_mb", 8)) << 20,

		// Zalo has a transient chat action a bot can send, so a turn shows it is
		// working while the agent works.
		TypingIndicator: options.bool("typing_indicator", true),
		Acknowledgement: zalo.Acknowledgement{
			Enabled:  options.bool("acknowledgement", true),
			Mode:     options.get("acknowledgement_mode"),
			Reaction: options.get("acknowledgement_reaction"),
		},
		Log: log,
	})

	log.Info("zalo transport starting", "plugin", *id, "instance", host.InstanceID())
	return plugin.Run(ctx)
}

// botToken reads the bot token from the environment.
//
// ZALO_TOKEN is accepted as well as ZALO_BOT_TOKEN so a value kept in a .env
// file, which is where a personal installation usually keeps it, works as it is.
func botToken() string {
	if token := strings.TrimSpace(os.Getenv("ZALO_BOT_TOKEN")); token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("ZALO_TOKEN"))
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

func (o optionFlags) int(key string, fallback int) int {
	value, ok := o[key]
	if !ok {
		return fallback
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

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
