package main

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"syscall"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"
)

// newRootCommand builds the command tree.
//
// Every command is a cobra command, so help, completions, and the manpage are
// generated from one definition. The prose a user reads is not generated: each
// command's Long and Example come from helpTable, which is curated and tested.
func newRootCommand(g *flags) *cobra.Command {
	root := &cobra.Command{
		Use:   "hive",
		Short: "personal agent gateway",
		Long: "Hive is a self-hosted gateway between where you talk and the agents\n" +
			"that do the work. A single binary serves every cluster role and is also\n" +
			"the client of the Hive Control API.",
		SilenceUsage: true,
		// Cobra suggests a command for a typo; a wrong guess is worse than none.
		DisableSuggestions: true,
	}

	// Global flags. They are persistent, so `hive -config x status` and
	// `hive status -config x` both work — which is what the old
	// parseArgsAndFlags existed to guarantee.
	root.PersistentFlags().StringVar(&g.configPath, "config", "", "configuration file (default ~/.hive/config.toml)")
	root.PersistentFlags().StringVar(&g.role, "role", "", "override cluster.role (coordinator, node, auto)")
	root.PersistentFlags().StringVar(&g.coordinatorURL, "coordinator-url", "", "override cluster.coordinator_url")
	root.PersistentFlags().StringVar(&g.nodeID, "node-id", "", "override cluster.node_id")

	root.AddCommand(
		initCommand(g),
		versionCommand(),
		configCommand(g),
		serveCommand(g),
		serviceCommand(g),
		updateCommand(g),
		uninstallCommand(g),
		tuiCommandGroup(g),
		sessionCommandGroup(g),
		workspaceCommandGroup(g),
		permissionCommandGroup(g),
		agentCommandGroup(g),
		nodeCommandGroup(g),
		commandCommandGroup(g),
		doctorCommand(g),
		logsCommand(g),
	)

	// `hive help <command>` is how a user asks about one command, so the help
	// command is part of the tree rather than a flag on the root.
	root.InitDefaultHelpCmd()

	applyHelpTable(root)
	return root
}

// globalFlagNames are the long flags the root defines.
var globalFlagNames = []string{"config", "role", "coordinator-url", "node-id", "help", "version"}

// normalizeGlobalFlags rewrites "-config x" as "--config x".
//
// Cobra decides which command is being run before it parses flags, and it only
// recognises a single-dash long flag once the command is known. The CLI has
// always accepted both spellings — the node child is started with them, and so is
// every example in the documentation — so they are normalised once, here, rather
// than in every invocation ever written down.
func normalizeGlobalFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") || arg == "-" {
			out = append(out, arg)
			continue
		}

		name := strings.TrimPrefix(arg, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		if slices.Contains(globalFlagNames, name) {
			out = append(out, "-"+arg)
			continue
		}
		out = append(out, arg)
	}
	return out
}

// execute runs the tree through fang, which styles help and errors, adds
// --version, and generates completions and a manpage.
func execute(ctx context.Context, args []string) error {
	var g flags
	root := newRootCommand(&g)
	root.SetArgs(normalizeGlobalFlags(args))

	// fang prints the error itself, so the caller only has to exit non-zero.
	return fang.Execute(ctx, root,
		fang.WithVersion(Version),
		fang.WithNotifySignal(os.Interrupt, syscall.SIGTERM),
	)
}

// applyHelpTable gives every command the curated prose a flag set cannot know.
//
// The table is the source of truth for what a user reads; a test fails if a
// command has no entry, or an entry has no command.
func applyHelpTable(root *cobra.Command) {
	for _, entry := range helpTable {
		cmd, ok := findCommand(root, entry.path)
		if !ok {
			continue
		}
		// The table wins: it is the curated text, and a test fails when a command
		// has no entry.
		cmd.Short = entry.summary
		if len(entry.details) > 0 {
			cmd.Long = strings.Join(entry.details, "\n")
		}
		if len(entry.examples) > 0 {
			cmd.Example = strings.Join(entry.examples, "\n")
		}
	}
}

// findCommand resolves "session create" to the command it names.
func findCommand(root *cobra.Command, path string) (*cobra.Command, bool) {
	cmd := root
	for _, name := range strings.Fields(path) {
		var next *cobra.Command
		for _, child := range cmd.Commands() {
			if child.Name() == name {
				next = child
				break
			}
		}
		if next == nil {
			return nil, false
		}
		cmd = next
	}
	return cmd, true
}

// exactArgs is cobra.ExactArgs with the message the command already used.
//
// "accepts 1 arg(s), received 0" is not an answer to "why did nothing happen".
func exactArgs(n int, message string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return errors.New(message)
		}
		return nil
	}
}

// minArgs is exactArgs for a command that takes a trailing list.
func minArgs(n int, message string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) < n {
			return errors.New(message)
		}
		return nil
	}
}

// noArgs refuses a positional argument with a message a user can act on.
func noArgs(message string) cobra.PositionalArgs {
	return exactArgs(0, message)
}

// ── root commands ────────────────────────────────────────────────────────────

func initCommand(g *flags) *cobra.Command {
	var (
		agent string
		force bool
	)
	cmd := &cobra.Command{
		Use:  "init",
		Args: noArgs("init takes no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(*g, initOptions{agent: agent, force: force})
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "the default agent, chosen without prompting")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing configuration file")
	return cmd
}

func versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:  "version",
		Args: noArgs("version takes no arguments"),
		RunE: func(*cobra.Command, []string) error { printVersion(); return nil },
	}
}

func configCommand(g *flags) *cobra.Command {
	var update bool
	cmd := &cobra.Command{
		Use:  "config",
		Args: noArgs("config takes no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if update {
				return runConfigUpdate(*g)
			}
			return runConfig(*g)
		},
	}
	cmd.Flags().BoolVar(&update, "update", false, "change the configuration with a guided setup")
	return cmd
}

func serveCommand(g *flags) *cobra.Command {
	return &cobra.Command{
		Use:  "serve",
		Args: noArgs("serve takes no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(*g, cmd.Context())
		},
	}
}

func serviceCommand(g *flags) *cobra.Command {
	service := &cobra.Command{
		Use:  "service",
		Args: noArgs("service requires an action: install, restart, uninstall, status"),
	}
	service.AddCommand(
		&cobra.Command{
			Use:  "install",
			Args: noArgs("service install takes no arguments"),
			RunE: func(*cobra.Command, []string) error { return installService(*g) },
		},
		&cobra.Command{
			Use:  "restart",
			Args: noArgs("service restart takes no arguments"),
			RunE: func(*cobra.Command, []string) error { return restartService(*g) },
		},
		&cobra.Command{
			Use:  "uninstall",
			Args: noArgs("service uninstall takes no arguments"),
			RunE: func(*cobra.Command, []string) error { return uninstallService(*g) },
		},
		&cobra.Command{
			Use:  "status",
			Args: noArgs("service status takes no arguments"),
			RunE: func(*cobra.Command, []string) error { return serviceStatus() },
		},
	)
	return service
}

func updateCommand(g *flags) *cobra.Command {
	var opts updateCommandOptions
	cmd := &cobra.Command{
		Use:  "update",
		Args: noArgs("update takes no arguments"),
		RunE: func(*cobra.Command, []string) error { return runUpdate(*g, opts) },
	}
	cmd.Flags().StringVar(&opts.version, "version", "", "the release to install (default: the latest)")
	cmd.Flags().StringVar(&opts.dir, "dir", "", "where to install (default: the running binary's directory)")
	cmd.Flags().BoolVar(&opts.force, "force", false, "install even when the version is already current")
	return cmd
}

func uninstallCommand(g *flags) *cobra.Command {
	var opts uninstallOptions
	cmd := &cobra.Command{
		Use:  "uninstall",
		Args: noArgs("uninstall takes no arguments"),
		RunE: func(*cobra.Command, []string) error { return runUninstall(*g, opts) },
	}
	cmd.Flags().BoolVar(&opts.yes, "yes", false, "remove without asking for confirmation")
	cmd.Flags().BoolVar(&opts.workspace, "workspace", false, "also remove the agent workspace and everything in it")
	return cmd
}

func tuiCommandGroup(g *flags) *cobra.Command {
	var opts tuiOptions
	cmd := &cobra.Command{
		Use:  "tui",
		Args: noArgs("tui takes no arguments"),
		RunE: func(*cobra.Command, []string) error { return tuiCommand(*g, opts) },
	}
	cmd.Flags().DurationVar(&opts.interval, "interval", 0, "refresh interval (default: render once)")
	return cmd
}

func doctorCommand(g *flags) *cobra.Command {
	return &cobra.Command{
		Use:  "doctor",
		Args: noArgs("doctor takes no arguments"),
		RunE: func(*cobra.Command, []string) error { return runDoctor(*g) },
	}
}

func logsCommand(g *flags) *cobra.Command {
	var opts logsOptions
	cmd := &cobra.Command{
		Use:  "logs",
		Args: noArgs("logs takes no arguments"),
		RunE: func(*cobra.Command, []string) error { return runLogs(*g, opts) },
	}
	cmd.Flags().IntVar(&opts.lines, "lines", 50, "how many lines to show (0 for all)")
	cmd.Flags().BoolVar(&opts.follow, "follow", false, "keep printing as the daemon writes")
	cmd.Flags().StringVar(&opts.level, "level", "", "only lines at this level or above")
	cmd.Flags().StringVar(&opts.grep, "grep", "", "only lines containing this text")
	return cmd
}

// ── session ──────────────────────────────────────────────────────────────────

func sessionCommandGroup(g *flags) *cobra.Command {
	session := &cobra.Command{
		Use:  "session",
		Args: noArgs("session requires an action: create, list, status, prompt, cancel, handoff, events, config"),
	}
	session.AddCommand(
		sessionCreateCommand(g),
		sessionListCommand(g),
		sessionStatusCommand(g),
		sessionPromptCommand(g),
		sessionCancelCommand(g),
		sessionHandoffCommand(g),
		sessionEventsCommand(g),
		sessionConfigCommand(g),
	)
	return session
}

func sessionCreateCommand(g *flags) *cobra.Command {
	var opts sessionCreateOptions
	cmd := &cobra.Command{
		Use:  "create",
		Args: noArgs("session create takes no arguments"),
		RunE: func(*cobra.Command, []string) error { return sessionCreate(*g, opts) },
	}
	cmd.Flags().StringVar(&opts.agent, "agent", "", "agent to run (default: the configured default agent)")
	cmd.Flags().StringVar(&opts.workspace, "workspace", "", "workspace the run works in")
	cmd.Flags().StringVar(&opts.commandID, "command-id", "", "idempotency key (default: a fresh one)")
	return cmd
}

func sessionListCommand(g *flags) *cobra.Command {
	var opts sessionListOptions
	cmd := &cobra.Command{
		Use:  "list",
		Args: noArgs("session list takes no arguments"),
		RunE: func(*cobra.Command, []string) error { return sessionList(*g, opts) },
	}
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "maximum sessions to show")
	return cmd
}

func sessionStatusCommand(g *flags) *cobra.Command {
	return &cobra.Command{
		Use:  "status <id>",
		Args: exactArgs(1, "session status requires a session id"),
		RunE: func(_ *cobra.Command, args []string) error { return sessionStatus(*g, args[0]) },
	}
}

func sessionPromptCommand(g *flags) *cobra.Command {
	var opts sessionPromptOptions
	cmd := &cobra.Command{
		Use:  "prompt <id> <text>",
		Args: minArgs(2, "session prompt requires a session id and text"),
		RunE: func(_ *cobra.Command, args []string) error {
			opts.sessionID = args[0]
			opts.text = strings.Join(args[1:], " ")
			return sessionPrompt(*g, opts)
		},
	}
	cmd.Flags().StringVar(&opts.runID, "run", "", "target a specific AgentRun")
	cmd.Flags().StringVar(&opts.commandID, "command-id", "", "idempotency key (default: a fresh one)")
	cmd.Flags().BoolVar(&opts.noWait, "no-wait", false, "return as soon as the turn is accepted")
	return cmd
}

func sessionCancelCommand(g *flags) *cobra.Command {
	var opts sessionCancelOptions
	cmd := &cobra.Command{
		Use:  "cancel <id>",
		Args: exactArgs(1, "session cancel requires a session id"),
		RunE: func(_ *cobra.Command, args []string) error {
			opts.sessionID = args[0]
			return sessionCancel(*g, opts)
		},
	}
	cmd.Flags().StringVar(&opts.runID, "run", "", "cancel a specific AgentRun")
	return cmd
}

func sessionHandoffCommand(g *flags) *cobra.Command {
	var opts sessionHandoffOptions
	cmd := &cobra.Command{
		Use:  "handoff <id> <agent>",
		Args: exactArgs(2, "session handoff requires a session id and a target agent"),
		RunE: func(_ *cobra.Command, args []string) error {
			opts.sessionID = args[0]
			opts.agentID = args[1]
			return sessionHandoff(*g, opts)
		},
	}
	cmd.Flags().StringVar(&opts.summary, "summary", "", "optional source-agent summary")
	cmd.Flags().StringVar(&opts.workspace, "workspace", "", "workspace the target works in")
	cmd.Flags().StringVar(&opts.commandID, "command-id", "", "idempotency key (default: a fresh one)")
	return cmd
}

func sessionEventsCommand(g *flags) *cobra.Command {
	var opts sessionEventsOptions
	cmd := &cobra.Command{
		Use:  "events <id>",
		Args: exactArgs(1, "session events requires a session id"),
		RunE: func(_ *cobra.Command, args []string) error {
			opts.sessionID = args[0]
			return sessionEvents(*g, opts)
		},
	}
	cmd.Flags().Int64Var(&opts.from, "from", 0, "resume after this sequence")
	cmd.Flags().IntVar(&opts.limit, "limit", 50, "maximum events to show")
	cmd.Flags().BoolVar(&opts.asJSON, "json", false, "print raw events")
	cmd.Flags().BoolVar(&opts.all, "all", false, "include agent.raw protocol traffic")
	cmd.Flags().StringVar(&opts.types, "type", "", "only events of this type")
	return cmd
}

func sessionConfigCommand(g *flags) *cobra.Command {
	var opts sessionConfigOptions
	cmd := &cobra.Command{
		Use:  "config <id> [selector] [value]",
		Args: minArgs(1, "session config requires a session id, and optionally a selector and a value"),
		RunE: func(_ *cobra.Command, args []string) error {
			opts.sessionID = args[0]
			if len(args) >= 2 {
				opts.configID = args[1]
			}
			if len(args) >= 3 {
				opts.value = args[2]
			}
			return sessionConfig(*g, opts)
		},
	}
	cmd.Flags().StringVar(&opts.runID, "run", "", "run whose agent session to ask (default: the session's current run)")
	return cmd
}

// ── agent, node, command ─────────────────────────────────────────────────────

func agentCommandGroup(g *flags) *cobra.Command {
	agent := &cobra.Command{
		Use:  "agent",
		Args: noArgs("agent requires the action: list"),
	}
	agent.AddCommand(&cobra.Command{
		Use:  "list",
		Args: noArgs("agent list takes no arguments"),
		RunE: func(*cobra.Command, []string) error { return agentList(*g) },
	})
	return agent
}

func nodeCommandGroup(g *flags) *cobra.Command {
	node := &cobra.Command{
		Use:  "node",
		Args: noArgs("node requires the action: list"),
	}
	node.AddCommand(&cobra.Command{
		Use:  "list",
		Args: noArgs("node list takes no arguments"),
		RunE: func(*cobra.Command, []string) error { return nodeList(*g) },
	})
	return node
}

func commandCommandGroup(g *flags) *cobra.Command {
	command := &cobra.Command{
		Use:  "command",
		Args: noArgs("command requires the action: get"),
	}
	command.AddCommand(&cobra.Command{
		Use:  "get <id>",
		Args: exactArgs(1, "command get requires a command id"),
		RunE: func(_ *cobra.Command, args []string) error { return commandGet(*g, args[0]) },
	})
	return command
}

// ── workspace ────────────────────────────────────────────────────────────────

func workspaceCommandGroup(g *flags) *cobra.Command {
	workspace := &cobra.Command{
		Use:  "workspace",
		Args: noArgs("workspace requires an action: create, list"),
	}
	workspace.AddCommand(workspaceCreateCommand(g), workspaceListCommand(g))
	return workspace
}

func workspaceCreateCommand(g *flags) *cobra.Command {
	var opts workspaceCreateOptions
	cmd := &cobra.Command{
		Use:  "create <name>",
		Args: exactArgs(1, "workspace create requires a name"),
		RunE: func(_ *cobra.Command, args []string) error {
			opts.name = args[0]
			return workspaceCreate(*g, opts)
		},
	}
	cmd.Flags().StringVar(&opts.nodeID, "node", "", "node the location is on")
	cmd.Flags().StringVar(&opts.path, "path", "", "path the workspace lives at on that node")
	cmd.Flags().StringVar(&opts.commandID, "command-id", "", "idempotency key (default: a fresh one)")
	return cmd
}

func workspaceListCommand(g *flags) *cobra.Command {
	return &cobra.Command{
		Use:  "list",
		Args: noArgs("workspace list takes no arguments"),
		RunE: func(*cobra.Command, []string) error { return workspaceList(*g) },
	}
}

// ── permission ───────────────────────────────────────────────────────────────

func permissionCommandGroup(g *flags) *cobra.Command {
	permission := &cobra.Command{
		Use:  "permission",
		Args: noArgs("permission requires an action: list, respond"),
	}
	permission.AddCommand(permissionListCommand(g), permissionRespondCommand(g))
	return permission
}

func permissionListCommand(g *flags) *cobra.Command {
	var opts permissionListOptions
	cmd := &cobra.Command{
		Use:  "list",
		Args: noArgs("permission list takes no arguments"),
		RunE: func(*cobra.Command, []string) error { return permissionList(*g, opts) },
	}
	cmd.Flags().StringVar(&opts.sessionID, "session", "", "only this session")
	return cmd
}

func permissionRespondCommand(g *flags) *cobra.Command {
	var opts permissionRespondOptions
	cmd := &cobra.Command{
		Use:  "respond <id>",
		Args: exactArgs(1, "permission respond requires an agent request id"),
		RunE: func(_ *cobra.Command, args []string) error {
			opts.requestID = args[0]
			return permissionRespond(*g, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.allow, "allow", false, "approve the request")
	cmd.Flags().BoolVar(&opts.deny, "deny", false, "reject the request")
	cmd.Flags().StringVar(&opts.sessionID, "session", "", "the session the request belongs to")
	return cmd
}
