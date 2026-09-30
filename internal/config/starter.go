package config

import (
	"fmt"
	"strings"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// DefaultLeaseSeconds is the node liveness claim a rendered configuration states
// explicitly, so the file says what actually happens rather than "0 means
// something".
//
// internal/node keeps its own fallback for a node that declares nothing, which is
// the case only when a node is configured by hand.
const DefaultLeaseSeconds = 30

// StarterOptions describes the configuration `hive init` should write.
type StarterOptions struct {
	// Agents are the ACP agents found on this machine.
	Agents []DiscoveredAgent

	// DefaultAgent is the agent a session uses when it does not choose one.
	DefaultAgent string

	// WorkspaceDir is where a run works when its workspace names no location. It
	// is written into the file because a run with no workspace is refused, and a
	// starter that cannot run an agent is not a starter.
	WorkspaceDir string

	// Transports are the transport plugins found on this machine, each describing
	// its own section. The starter renders what the plugins declare rather than
	// knowing any of them, so a new transport is configurable without a core
	// change.
	Transports []v1.PluginManifest

	// EnabledTransports are the transports a guided setup turned on, with the
	// values the user gave. They are written as active sections, and are not
	// repeated among the commented examples.
	EnabledTransports []EnabledTransport

	// AllowedUsers are the principals the security section permits, as
	// "<transport>:<user id>". Nil keeps the configuration default, which is
	// empty: unknown access is denied.
	AllowedUsers []string

	// AllowedChannels are the channel allow list the security section holds.
	// Nil keeps the configuration default.
	AllowedChannels []string
}

// EnabledTransport is one transport a user chose, and what they answered.
type EnabledTransport struct {
	Manifest v1.PluginManifest

	// Options are the values to write, by option name. An option the user did
	// not answer keeps the value the plugin declared.
	Options map[string]string
}

// RenderStarter renders a starter configuration.
//
// Every key is present with its effective default, so nothing has to be guessed
// and nothing depends on the reader knowing what a zero value means. The values
// come from Default, so the file cannot drift from what the code uses.
func RenderStarter(opts StarterOptions) string {
	cfg := Default()

	var b strings.Builder

	b.WriteString("# Hive configuration.\n")
	b.WriteString("#\n")
	b.WriteString("# Written by `hive init`. Every key is present with its effective default.\n")
	b.WriteString("# Edit and restart `hive serve` to change behaviour.\n\n")

	fmt.Fprintf(&b, "data_dir = %q\n", cfg.DataDir)
	b.WriteString("#   ^ empty uses ~/.hive/data\n\n")

	fmt.Fprintf(&b, "workspace_dir = %q\n", opts.WorkspaceDir)
	b.WriteString("#   ^ where a run works when its workspace names no location for the node.\n")
	b.WriteString("#     A run must have one: an agent that writes files needs to know where.\n")
	b.WriteString("#     Empty uses ~/.hive/workspace, a directory Hive owns, so an installation\n")
	b.WriteString("#     never works in the daemon's own directory — which for a service is the\n")
	b.WriteString("#     filesystem root.\n\n")

	fmt.Fprintf(&b, "default_agent = %q\n", opts.DefaultAgent)
	b.WriteString("#   ^ the agent a session uses when it does not choose one\n\n")

	b.WriteString("[cluster]\n")
	fmt.Fprintf(&b, "role = %q\n", cfg.Cluster.Role)
	b.WriteString("#   ^ auto | coordinator | node\n")
	b.WriteString("#     auto is a node when coordinator_url is set, otherwise a coordinator\n")
	fmt.Fprintf(&b, "node_id = %q\n", cfg.Cluster.NodeID)
	b.WriteString("#   ^ empty uses the hostname\n")
	fmt.Fprintf(&b, "listen = %q\n", cfg.Cluster.Listen)
	b.WriteString("#   ^ the coordinator node link; empty binds 127.0.0.1 on a free port\n")
	fmt.Fprintf(&b, "coordinator_url = %q\n", cfg.Cluster.CoordinatorURL)
	b.WriteString("#   ^ required when role is node\n")
	fmt.Fprintf(&b, "spawn_node = %t\n", cfg.Cluster.SpawnsNode())
	b.WriteString("#   ^ a coordinator starts a node child process, so one command runs the\n")
	b.WriteString("#     whole stack. Set false to run the planes separately.\n")
	fmt.Fprintf(&b, "lease_seconds = %d\n", DefaultLeaseSeconds)
	b.WriteString("#   ^ the node liveness claim. Expiry classifies a run as interrupted and\n")
	b.WriteString("#     never starts a replacement execution.\n\n")

	b.WriteString("[log]\n")
	fmt.Fprintf(&b, "level = %q\n", cfg.Log.Level)
	b.WriteString("#   ^ debug | info | warn | error\n")
	fmt.Fprintf(&b, "format = %q\n", cfg.Log.Format)
	b.WriteString("#   ^ text | json\n\n")

	b.WriteString("[event_store]\n")
	fmt.Fprintf(&b, "retention_days = %d\n", cfg.EventStore.RetentionDays)
	b.WriteString("#   ^ prunes durable events that have already been published. A client whose\n")
	b.WriteString("#     cursor was pruned is told so, rather than silently losing history.\n\n")

	allowedUsers := cfg.Security.AllowedUsers
	if opts.AllowedUsers != nil {
		allowedUsers = opts.AllowedUsers
	}
	allowedChannels := cfg.Security.AllowedChannels
	if opts.AllowedChannels != nil {
		allowedChannels = opts.AllowedChannels
	}
	b.WriteString("[security]\n")
	fmt.Fprintf(&b, "allowed_users = %s\n", renderStrings(allowedUsers))
	b.WriteString("#   ^ principals a transport may assert, as \"<transport>:<user id>\", for\n")
	b.WriteString("#     example \"slack:U123\" or \"zalo:<user id>\". Unknown access is denied by\n")
	b.WriteString("#     default, so a transport message is refused until its sender is named\n")
	b.WriteString("#     here. A refused message reports the principal to add.\n")
	fmt.Fprintf(&b, "allowed_channels = %s\n", renderStrings(allowedChannels))
	b.WriteString("#   ^ optional channel allow list, for a transport that has channels.\n\n")

	renderAgents(&b, opts)

	renderTransports(&b, opts)

	return b.String()
}

func renderAgents(b *strings.Builder, opts StarterOptions) {
	b.WriteString("# Agents.\n")
	b.WriteString("#\n")
	b.WriteString("# An agent is a protocol plus a command. Hive core holds no vendor knowledge:\n")
	b.WriteString("# it speaks ACP to whatever the command launches.\n")

	if len(opts.Agents) == 0 {
		b.WriteString("#\n")
		b.WriteString("# No ACP agent was found on this machine. Install one, then add a section:\n")
		b.WriteString("#\n")
		b.WriteString("# [agents.example]\n")
		b.WriteString("# protocol = \"acp\"\n")
		b.WriteString("# command = [\"your-agent\", \"acp\"]\n")
		b.WriteString("#\n")
		b.WriteString("# Until then Hive serves status but cannot create a session.\n\n")
		return
	}

	b.WriteString("#\n")
	b.WriteString("# The agents below were found on this machine's PATH.\n\n")

	for _, agent := range opts.Agents {
		fmt.Fprintf(b, "[agents.%s]\n", agent.Name)
		fmt.Fprintf(b, "protocol = %q\n", "acp")
		fmt.Fprintf(b, "command = %s\n", renderStrings(agent.Command))
		if agent.NeedsVerification() {
			b.WriteString("#   ^ the ACP invocation is a convention, not a fact: verify it against\n")
			b.WriteString("#     this tool's documentation.\n")
		}
		fmt.Fprintf(b, "endpoint = %q\n", "")
		b.WriteString("#   ^ a remote agent endpoint. Remote agents are not implemented in v1.\n\n")
	}
}

// renderTransports renders each transport's section from the plugin's own
// declaration.
//
// The core knows no transport by name: a plugin describes the section it reads,
// so the starter can document a transport it has never heard of.
func renderTransports(b *strings.Builder, opts StarterOptions) {
	b.WriteString("# Transports.\n")
	b.WriteString("#\n")
	b.WriteString("# A transport normalizes a platform into Hive operations. Credentials come from\n")
	b.WriteString("# the environment, never from this file. Each section below was described by the\n")
	b.WriteString("# transport plugin itself, so it is the plugin's own contract.\n")

	if len(opts.Transports) == 0 && len(opts.EnabledTransports) == 0 {
		b.WriteString("#\n")
		b.WriteString("# No transport plugin was found next to the hive binary. Install one, or add a\n")
		b.WriteString("# section by hand:\n")
		b.WriteString("#\n")
		b.WriteString("# [transport.<name>]\n")
		b.WriteString("# enabled = true\n")
		b.WriteString("# [transport.<name>.options]\n")
		return
	}

	// The ones that were chosen are active; the rest stay as examples to uncomment.
	enabled := make(map[string]bool, len(opts.EnabledTransports))
	for _, selection := range opts.EnabledTransports {
		renderEnabledTransport(b, selection)
		enabled[selection.Manifest.ID] = true
	}
	for _, manifest := range opts.Transports {
		if enabled[manifest.ID] {
			continue
		}
		renderTransport(b, manifest)
	}
}

// renderEnabledTransport writes a transport that was turned on, with the answers
// the user gave.
func renderEnabledTransport(b *strings.Builder, selection EnabledTransport) {
	declared := selection.Manifest.Config
	if declared == nil || declared.Section == "" {
		return
	}

	b.WriteString("\n")
	if declared.Summary != "" {
		fmt.Fprintf(b, "# %s\n", declared.Summary)
	}
	fmt.Fprintf(b, "[%s]\n", declared.Section)
	b.WriteString("enabled = true\n")

	if len(declared.Options) > 0 {
		fmt.Fprintf(b, "[%s.options]\n", declared.Section)
		for _, option := range declared.Options {
			value := option.Default
			if chosen, ok := selection.Options[option.Name]; ok {
				value = chosen
			}
			fmt.Fprintf(b, "%s = %q", option.Name, value)
			if option.Description != "" {
				fmt.Fprintf(b, "   # %s", option.Description)
			}
			b.WriteString("\n")
		}
	}

	if ack := declared.Acknowledgement; ack != nil {
		fmt.Fprintf(b, "[%s.acknowledgement]\n", declared.Section)
		fmt.Fprintf(b, "enabled = %t\n", ack.Enabled)
		if ack.Mode != "" {
			fmt.Fprintf(b, "mode = %q\n", ack.Mode)
		}
		if ack.Reaction != "" {
			fmt.Fprintf(b, "reaction = %q\n", ack.Reaction)
		}
	}
}

// renderTransport renders one transport's commented section.
func renderTransport(b *strings.Builder, manifest v1.PluginManifest) {
	declared := manifest.Config
	if declared == nil || declared.Section == "" {
		return
	}

	b.WriteString("\n")
	if declared.Summary != "" {
		fmt.Fprintf(b, "# %s\n", declared.Summary)
	}
	fmt.Fprintf(b, "# [%s]\n", declared.Section)
	fmt.Fprintf(b, "# enabled = %t\n", declared.Enabled)

	if len(declared.Options) > 0 {
		fmt.Fprintf(b, "# [%s.options]\n", declared.Section)
		for _, option := range declared.Options {
			fmt.Fprintf(b, "# %s = %q", option.Name, option.Default)
			if option.Description != "" {
				fmt.Fprintf(b, "   # %s", option.Description)
			}
			b.WriteString("\n")
		}
	}

	if ack := declared.Acknowledgement; ack != nil {
		b.WriteString("#\n")
		b.WriteString("# Acknowledgement means \"received\", never \"started\" or \"finished\".\n")
		b.WriteString("# It is on by default and its failure never fails the operation.\n")
		fmt.Fprintf(b, "# [%s.acknowledgement]\n", declared.Section)
		fmt.Fprintf(b, "# enabled = %t\n", ack.Enabled)
		if ack.Mode != "" {
			fmt.Fprintf(b, "# mode = %q\n", ack.Mode)
		}
		if ack.Reaction != "" {
			fmt.Fprintf(b, "# reaction = %q\n", ack.Reaction)
		}
	}

	if len(manifest.Secrets) > 0 {
		b.WriteString("#\n")
		b.WriteString("# Environment variables this transport needs:\n")
		for _, secret := range manifest.Secrets {
			// A secret may be named by more than one variable; any one is enough.
			fmt.Fprintf(b, "#   %s", strings.Join(secret.Any, " or "))
			if secret.Description != "" {
				fmt.Fprintf(b, "   # %s", secret.Description)
			}
			b.WriteString("\n")
		}
	}
}

func renderStrings(values []string) string {
	if len(values) == 0 {
		return "[]"
	}

	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, fmt.Sprintf("%q", value))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
