package config

import (
	"fmt"
	"strings"
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

	fmt.Fprintf(&b, "workspace_dir = %q\n", cfg.WorkspaceDir)
	b.WriteString("#   ^ where a run works when its workspace names no location for the node.\n")
	b.WriteString("#     Empty uses the node's working directory. A run must have one: an\n")
	b.WriteString("#     agent that writes files needs to know where.\n\n")

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

	b.WriteString("[security]\n")
	fmt.Fprintf(&b, "allowed_users = %s\n", renderStrings(cfg.Security.AllowedUsers))
	b.WriteString("#   ^ principals a transport may assert, for example \"slack:U123\".\n")
	b.WriteString("#     Unknown access is denied by default.\n")
	fmt.Fprintf(&b, "allowed_channels = %s\n", renderStrings(cfg.Security.AllowedChannels))
	b.WriteString("#   ^ optional channel allow list, for a transport that has channels.\n\n")

	renderAgents(&b, opts)

	renderTransports(&b, cfg)

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

func renderTransports(b *strings.Builder, cfg Config) {
	b.WriteString("# Transports.\n")
	b.WriteString("#\n")
	b.WriteString("# A transport normalizes a platform into Hive operations. Credentials come from\n")
	b.WriteString("# the environment, never from this file.\n")
	b.WriteString("#\n")
	b.WriteString("# [transport.slack]\n")
	fmt.Fprintf(b, "# enabled = %t\n", false)
	b.WriteString("# [transport.slack.options]\n")
	b.WriteString("# bot_user_id = \"U0XXXXXXX\"   # the bot's own user id, for mention resolution\n")
	b.WriteString("# require_mention = \"true\"    # ignore messages that do not address the bot\n")
	b.WriteString("#\n")
	b.WriteString("# The Slack transport needs two environment variables:\n")
	b.WriteString("#   SLACK_APP_TOKEN=xapp-...   Socket Mode connection\n")
	b.WriteString("#   SLACK_BOT_TOKEN=xoxb-...   Web API calls\n")
	b.WriteString("#\n")
	b.WriteString("# Acknowledgement means \"received\", never \"started\" or \"finished\". It is\n")
	b.WriteString("# on by default and its failure never fails the operation.\n")
	b.WriteString("#\n")
	b.WriteString("# [transport.slack.acknowledgement]\n")
	fmt.Fprintf(b, "# enabled = %t\n", cfg.Transports["slack"].Acknowledgement.EnabledOr())
	fmt.Fprintf(b, "# mode = %q\n", "reaction")
	fmt.Fprintf(b, "# reaction = %q\n", cfg.Transports["slack"].Acknowledgement.ReactionOr())
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
