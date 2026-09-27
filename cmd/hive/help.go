package main

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

// helpTable is the prose every command shows.
//
// The tree owns the commands and their flags; this owns what a user reads. A
// test fails when the two disagree, in either direction.
var helpTable = []commandHelp{
	{
		path:    "init",
		summary: "write a starter configuration file",
		usage:   "hive init [-force] [-agent <name>]",
		details: []string{
			"Hive cannot create a session without an agent, so this is the first",
			"command to run. In a terminal it walks you through the setup: which",
			"agents to configure, which transports to enable, the credentials each",
			"one needs, and where runs should work.",
			"",
			"The credentials are written to a file only you can read, never to the",
			"configuration. With -agent, or without a terminal, the flags decide and",
			"nothing is asked, so a script still works.",
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
		usage:   "hive service <install|restart|uninstall|status>",
		details: []string{
			"`install` writes a launchd agent on macOS and a systemd user unit on",
			"Linux, copies the binaries next to the data directory, and writes the",
			"secrets the configuration needs to a file only its owner can read.",
			"Installing again over a running service restarts it, which is how a new",
			"build is picked up.",
		},
		examples: []string{
			"hive service install",
			"hive service restart",
			"hive service status",
		},
	},
	{
		path:    "update",
		summary: "install a release over this one",
		usage:   "hive update [-version <x.y.z>] [-dir <path>] [-force]",
		details: []string{
			"Downloads the release for this platform, verifies its SHA-256 against",
			"the release's checksums.txt, and replaces the binaries next to the",
			"running one — hive and its plugins together, because the daemon",
			"resolves the plugins from that directory.",
			"",
			"A running service is restarted, so the new build is the one running.",
			"Without a service, restart `hive serve` yourself.",
		},
		flags: []flagHelp{
			{"-version", "the release to install (default: the latest)"},
			{"-dir", "where to install (default: the running binary's directory)"},
			{"-force", "install even when the version is already current"},
		},
		examples: []string{"hive update", "hive update -version 1.1.0", "hive update -force"},
	},
	{
		path:    "uninstall",
		summary: "remove the installation",
		usage:   "hive uninstall [-yes] [-workspace]",
		details: []string{
			"Stops and removes the background service, then removes the installed",
			"binaries, the data directory, and the configuration file. It asks before",
			"removing anything, and a bare newline means no.",
			"",
			"The workspace holds your files, so it is kept. Pass -workspace to remove",
			"it too, which only works when it is inside Hive's own home: a workspace",
			"pointed at a project is never deleted by an uninstall.",
		},
		flags: []flagHelp{
			{"-yes", "remove without asking (required when there is no terminal)"},
			{"-workspace", "also remove the agent workspace and everything in it"},
		},
		examples: []string{"hive uninstall", "hive uninstall -workspace", "hive uninstall -yes"},
	},
	{
		path:    "service install",
		summary: "start the daemon at login",
		usage:   "hive service install",
		details: []string{
			"Writes the platform's service definition and copies the binaries next",
			"to the data directory, so the daemon does not depend on a checkout.",
		},
		examples: []string{"hive service install"},
	},
	{
		path:    "service restart",
		summary: "restart the daemon, keeping its secrets",
		usage:   "hive service restart",
		details: []string{
			"Refreshes the installed binaries and rewrites the definition, so a new",
			"build and a changed role both take effect.",
		},
		examples: []string{"hive service restart"},
	},
	{
		path:     "service status",
		summary:  "report whether the daemon is running",
		usage:    "hive service status",
		examples: []string{"hive service status"},
	},
	{
		path:    "service uninstall",
		summary: "stop the daemon and remove its definition",
		usage:   "hive service uninstall",
		details: []string{
			"The secrets file goes with it, because it exists for the service. To",
			"remove the installation as well, use `hive uninstall`.",
		},
		examples: []string{"hive service uninstall"},
	},
	{
		path:    "session",
		summary: "create and drive sessions",
		usage:   "hive session <action>",
		details: []string{
			"A session is a conversation. A run is one execution of it by one agent,",
			"so a session outlives its runs and can be handed to another agent.",
		},
		examples: []string{"hive session create", "hive session list"},
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
		path:     "session list",
		summary:  "list sessions",
		usage:    "hive session list [-limit <n>]",
		flags:    []flagHelp{{"-limit", "maximum sessions to show"}},
		examples: []string{"hive session list"},
	},
	{
		path:     "agent",
		summary:  "inspect the configured agents",
		usage:    "hive agent <action>",
		examples: []string{"hive agent list"},
	},
	{
		path:     "agent list",
		summary:  "list the configured agents",
		usage:    "hive agent list",
		examples: []string{"hive agent list"},
	},
	{
		path:     "node",
		summary:  "inspect the nodes the coordinator has heard from",
		usage:    "hive node <action>",
		examples: []string{"hive node list"},
	},
	{
		path:     "node list",
		summary:  "list the nodes the coordinator has heard from",
		usage:    "hive node list",
		examples: []string{"hive node list"},
	},
	{
		path:     "command",
		summary:  "follow a durable command",
		usage:    "hive command <action>",
		examples: []string{"hive command get cmd_123"},
	},
	{
		path:    "permission",
		summary: "see and answer permission requests",
		usage:   "hive permission <action>",
		details: []string{
			"An agent may ask before running a tool. A request fails closed: it never",
			"becomes an implicit approval, including when the connection is lost.",
		},
		examples: []string{"hive permission list", "hive permission respond perm_123 -allow"},
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
		path:     "workspace",
		summary:  "register and list workspaces",
		usage:    "hive workspace <action>",
		examples: []string{"hive workspace list"},
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
		usage:   "hive config [--update]",
		details: []string{
			"Prints what Hive will actually use, after defaults and after resolving",
			"paths. Unknown keys are refused rather than ignored, so a typo is",
			"reported instead of silently doing nothing.",
			"",
			"With --update it runs the same guided setup as `hive init`, prefilled",
			"with what is configured now, and writes the result back.",
		},
		flags: []flagHelp{
			{"--update", "change the configuration with a guided setup"},
		},
		examples: []string{"hive config", "hive config --update"},
	},
	{
		path:    "logs",
		summary: "show what the daemon has been doing",
		usage:   "hive logs [-lines <n>] [-follow] [-level <level>] [-grep <text>]",
		details: []string{
			"The daemon writes a log file as well as the terminal, because a daemon",
			"is not watched: without a file there is no answer to \"what happened an",
			"hour ago\", and a service started by the system has no terminal at all.",
			"",
			"The file rotates at 8 MB and keeps three, so a process meant to run for",
			"months cannot fill the disk.",
		},
		flags: []flagHelp{
			{"-lines", "how many lines to show (0 for all)"},
			{"-follow", "keep printing as the daemon writes"},
			{"-level", "only lines at this level or above"},
			{"-grep", "only lines containing this text"},
		},
		examples: []string{
			"hive logs",
			"hive logs -level warn -lines 20",
			"hive logs -follow",
		},
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
