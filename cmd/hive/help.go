package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// commandHelp is what a user needs to use one command.
//
// It is written out rather than generated from the flag set, because a flag set
// knows a flag's name and nothing about what the command is for. `-h` on a flag
// set prints `-agent string` and stops; this says what the command does, what it
// prints, and what a failure means.
type commandHelp struct {
	// path is how the command is invoked, without the binary name.
	path string

	// summary is one line, used in the list.
	summary string

	// usage is the invocation shape.
	usage string

	// details are the paragraphs a user needs, one per line.
	details []string

	// flags are the options, in the order they matter.
	flags []flagHelp

	// examples are invocations that work.
	examples []string
}

// flagHelp is one option.
type flagHelp struct {
	flag    string
	meaning string
}

// helpTable is every command, in the order the usage lists them.
var helpTable = []commandHelp{
	{
		path:    "init",
		summary: "write a starter configuration file",
		usage:   "hive init [-force] [-agent <name>]",
		details: []string{
			"Hive cannot create a session without an agent, so this is the first",
			"command to run. It looks for ACP agents on PATH and writes a",
			"configuration naming the one you choose.",
		},
		flags: []flagHelp{
			{"-force", "overwrite an existing configuration"},
			{"-agent", "choose the default agent without being asked"},
		},
		examples: []string{"hive init", "hive init -agent devin"},
	},
	{
		path:    "serve",
		summary: "run the Hive daemon",
		usage:   "hive serve [-role <role>] [-coordinator-url <url>]",
		details: []string{
			"The coordinator owns durable state and the Control API; the node runs",
			"the agents. A single-machine installation runs both, and `serve`",
			"starts the node as a child process.",
		},
		flags: []flagHelp{
			{"-role", "coordinator, node, or auto (the default)"},
			{"-coordinator-url", "override cluster.coordinator_url"},
		},
		examples: []string{"hive serve"},
	},
	{
		path:    "service",
		summary: "run the daemon in the background at login",
		usage:   "hive service <install|uninstall|status>",
		details: []string{
			"`install` writes a launchd agent on macOS and a systemd user unit on",
			"Linux, copies the binaries next to the data directory, and writes the",
			"secrets the configuration needs to a file only its owner can read.",
			"Installing again over a running service restarts it, which is how a new",
			"build is picked up.",
		},
		examples: []string{"hive service install", "hive service status"},
	},
	{
		path:    "session create",
		summary: "create a session and its first run",
		usage:   "hive session create [-agent <name>] [-workspace <name>]",
		details: []string{
			"A session is a conversation. A run is one execution of it by one agent,",
			"so a session outlives its runs and can be handed to another agent.",
		},
		flags: []flagHelp{
			{"-agent", "agent to run (default: the configured default)"},
			{"-workspace", "workspace the run works in"},
			{"-command-id", "idempotency key (default: a fresh one)"},
		},
		examples: []string{"hive session create", "hive session create -agent devin"},
	},
	{
		path:    "session prompt",
		summary: "prompt a session",
		usage:   "hive session prompt <id> <text> [-no-wait]",
		details: []string{
			"The turn runs in the background, because a command's lifetime is not a",
			"request's lifetime: a turn can take minutes, and holding the request",
			"open would tie it to a connection that may go away. The command is",
			"followed through its durable record instead.",
			"",
			"With -no-wait the command id is printed and nothing is followed.",
		},
		flags: []flagHelp{
			{"-no-wait", "dispatch and return without waiting for the answer"},
			{"-command-id", "idempotency key (default: a fresh one)"},
		},
		examples: []string{
			`hive session prompt sess_123 "fix the failing test"`,
			`hive session prompt sess_123 "run the tests" -no-wait`,
		},
	},
	{
		path:    "session status",
		summary: "show a session and its runs",
		usage:   "hive session status <id>",
		details: []string{
			"Every run is listed with its agent, node, state, and runtime session.",
			"The runtime session belongs to the agent; a run is what Hive owns.",
		},
		examples: []string{"hive session status sess_123"},
	},
	{
		path:    "session events",
		summary: "replay a session's event stream",
		usage:   "hive session events <id> [-limit <n>] [-all] [-type <type>] [-json]",
		details: []string{
			"Events are the durable record of what happened. The agent's own stream",
			"is kept as `agent.raw` and is hidden by default, because one answer can",
			"be hundreds of raw chunks and the readable answer is one `message`",
			"event.",
		},
		flags: []flagHelp{
			{"-limit", "how many events to show"},
			{"-all", "include the agent's raw stream"},
			{"-type", "only this event type"},
			{"-json", "one JSON object per line"},
		},
		examples: []string{"hive session events sess_123", "hive session events sess_123 -all"},
	},
	{
		path:    "session config",
		summary: "read or change the agent's settings",
		usage:   "hive session config <id> [selector] [value]",
		details: []string{
			"The settings are the agent's own: it declares what it offers, and Hive",
			"renders whatever it declared. A model is a selector whose category is",
			"model, so a new selector needs no Hive change.",
			"",
			"A change is recorded on the session, so the next run continues with it",
			"rather than reverting. If the agent session is gone, it is brought up",
			"first, because asking for the settings is a request to have the agent",
			"there.",
		},
		flags: []flagHelp{
			{"-run", "the run whose agent session to ask (default: the current one)"},
		},
		examples: []string{
			"hive session config sess_123",
			"hive session config sess_123 model swe-2-fast",
		},
	},
	{
		path:     "session cancel",
		summary:  "cancel the run that is working",
		usage:    "hive session cancel <id>",
		examples: []string{"hive session cancel sess_123"},
	},
	{
		path:    "session handoff",
		summary: "hand the session to another agent",
		usage:   "hive session handoff <id> <agent> [-summary <text>]",
		details: []string{
			"The conversation continues under a different agent, carrying the",
			"context. A run that is still working is refused, because its turn would",
			"be abandoned: a handoff happens between turns, which is exactly when a",
			"run has finished.",
			"",
			"A handoff that fails leaves the session usable and does not report a",
			"transfer that did not happen.",
		},
		flags: []flagHelp{
			{"-summary", "what the source agent was doing, for the target"},
			{"-workspace", "workspace the target works in"},
		},
		examples: []string{"hive session handoff sess_123 hermes"},
	},
	{
		path:     "agent list",
		summary:  "list the configured agents",
		usage:    "hive agent list",
		examples: []string{"hive agent list"},
	},
	{
		path:     "node list",
		summary:  "list the nodes the coordinator has heard from",
		usage:    "hive node list",
		examples: []string{"hive node list"},
	},
	{
		path:    "permission list",
		summary: "list pending permission requests",
		usage:   "hive permission list",
		details: []string{
			"An agent may ask before running a tool. A request fails closed: it never",
			"becomes an implicit approval, including when the connection is lost.",
		},
		examples: []string{"hive permission list"},
	},
	{
		path:    "permission respond",
		summary: "answer a pending request",
		usage:   "hive permission respond <agent-request-id> -allow|-deny",
		flags: []flagHelp{
			{"-allow", "approve the request"},
			{"-deny", "refuse it"},
		},
		examples: []string{"hive permission respond perm_123 -allow"},
	},
	{
		path:    "workspace create",
		summary: "register a workspace",
		usage:   "hive workspace create <name> [-node <id>] [-path <path>]",
		details: []string{
			"A workspace is where an agent works, and a location is where it is on",
			"one node. Two runs sharing a location is reported rather than locked:",
			"they can corrupt each other's work, and the user is the one who knows",
			"whether that is intended.",
		},
		flags: []flagHelp{
			{"-node", "the node this location is on (default: this machine)"},
			{"-path", "the directory on that node"},
		},
		examples: []string{"hive workspace create piceta -path /tmp/piceta"},
	},
	{
		path:     "workspace list",
		summary:  "list workspaces and shared-location warnings",
		usage:    "hive workspace list",
		examples: []string{"hive workspace list"},
	},
	{
		path:    "command get",
		summary: "show a command's durable status",
		usage:   "hive command get <id>",
		details: []string{
			"A command is how an operation is followed. `session prompt` prints the",
			"command id when it does not wait, and this is how its outcome is read.",
		},
		examples: []string{"hive command get cmd_123"},
	},
	{
		path:    "config",
		summary: "validate the configuration and print effective values",
		usage:   "hive config",
		details: []string{
			"Prints what Hive will actually use, after defaults and after resolving",
			"paths. Unknown keys are refused rather than ignored, so a typo is",
			"reported instead of silently doing nothing.",
		},
		examples: []string{"hive config"},
	},
	{
		path:    "help",
		summary: "what a command does",
		usage:   "hive help [command]",
		details: []string{
			"`hive help` lists the commands. `hive help <command>` describes one,",
			"including what it prints and what a failure means.",
		},
		examples: []string{"hive help", "hive help session handoff"},
	},
	{
		path:    "doctor",
		summary: "check the installation and say what is wrong",
		usage:   "hive doctor",
		details: []string{
			"Checks the configuration, the agents, the daemon, the node, and the",
			"transport, and reports what it finds with what to do about it.",
		},
		examples: []string{"hive doctor"},
	},
	{
		path:    "tui",
		summary: "show the management plane",
		usage:   "hive tui [-interval <duration>]",
		flags: []flagHelp{
			{"-interval", "how often to refresh (default: 2s)"},
		},
		examples: []string{"hive tui", "hive tui -interval 5s"},
	},
	{
		path:     "version",
		summary:  "print the build and protocol version",
		usage:    "hive version",
		examples: []string{"hive version"},
	},
}

// runHelp prints help for the binary or for one command.
func runHelp(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		fmt.Print("\nRun `hive help <command>` for what a command does.\n")
		return nil
	}

	// The longest match wins, so `hive help session create` finds that command and
	// `hive help session` lists the session commands.
	query := strings.Join(args, " ")
	if entry, ok := findHelp(query); ok {
		printCommandHelp(os.Stdout, entry)
		return nil
	}

	matches := helpPrefix(query)
	if len(matches) == 0 {
		return fmt.Errorf("no such command %q; run `hive help` for the list", query)
	}
	if len(matches) == 1 {
		printCommandHelp(os.Stdout, matches[0])
		return nil
	}

	fmt.Printf("Commands under %q:\n\n", query)
	for _, entry := range matches {
		fmt.Printf("  %-22s %s\n", entry.path, entry.summary)
	}
	return nil
}

// findHelp is the command whose path is exactly query.
func findHelp(query string) (commandHelp, bool) {
	for _, entry := range helpTable {
		if entry.path == query {
			return entry, true
		}
	}
	return commandHelp{}, false
}

// helpPrefix is the commands whose path starts with query.
func helpPrefix(query string) []commandHelp {
	var out []commandHelp
	for _, entry := range helpTable {
		if strings.HasPrefix(entry.path, query+" ") {
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// printCommandHelp writes one command's help.
func printCommandHelp(w io.Writer, entry commandHelp) {
	fmt.Fprintf(w, "%s\n\n", entry.summary)
	fmt.Fprintf(w, "Usage:\n  %s\n", entry.usage)

	if len(entry.details) > 0 {
		fmt.Fprintln(w)
		for _, line := range entry.details {
			fmt.Fprintln(w, line)
		}
	}

	if len(entry.flags) > 0 {
		fmt.Fprintln(w, "\nFlags:")
		for _, flag := range entry.flags {
			fmt.Fprintf(w, "  %-14s %s\n", flag.flag, flag.meaning)
		}
	}

	if len(entry.examples) > 0 {
		fmt.Fprintln(w, "\nExamples:")
		for _, example := range entry.examples {
			fmt.Fprintf(w, "  %s\n", example)
		}
	}
}
