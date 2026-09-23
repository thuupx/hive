// Command hive is the Hive daemon and CLI entry point.
//
// A single binary serves every cluster role and is also the client of the Hive
// Control API. In the single-machine default, one `hive serve` runs the
// coordinator and starts a node child process, so the whole stack comes up with
// one command.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/term"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/thupham/hive/internal/config"
	"github.com/thupham/hive/internal/control"
	"github.com/thupham/hive/internal/daemon"
	"github.com/thupham/hive/internal/logging"
	"github.com/thupham/hive/internal/node"
	"github.com/thupham/hive/internal/plugin"
	"github.com/thupham/hive/internal/storage"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// Version is the Hive build version.
const Version = "0.1.0-dev"

// ChildStopTimeout bounds how long the coordinator waits for its node child.
const ChildStopTimeout = 5 * time.Second

const usage = `hive - personal agent gateway

Usage:
  hive [global flags] <command> [arguments]

Commands:
  init                       write a starter configuration file
  version                    print build and protocol version
  config                     validate the configuration and print effective values
  serve                      run the Hive daemon
  tui [-interval 2s]         show the management plane
  session create             create a session
  session list               list sessions
  session status <id>        show a session and its runs
  session prompt <id> <text> prompt a session
  session cancel <id>        cancel the current run
  session handoff <id> <agent>  hand the session to another agent
  session events <id>        replay a session event stream
  workspace create <name>    register a workspace
  workspace list             list workspaces and shared-location warnings
  agent list                 list configured agents
  node list                  list nodes
  command get <id>           show a command status resource

Global flags:
  -config <path>           configuration file (default ~/.hive/config.toml)
  -role <role>             override cluster.role (coordinator, node, auto)
  -coordinator-url <url>   override cluster.coordinator_url
  -node-id <id>            override cluster.node_id
  -h, --help               show this help
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "hive:", err)
		os.Exit(1)
	}
}

type flags struct {
	configPath     string
	role           string
	coordinatorURL string
	nodeID         string
	help           bool
}

func run(args []string) error {
	f, rest, err := parseArgs(args)
	if err != nil {
		return err
	}
	if f.help || len(rest) == 0 {
		fmt.Print(usage)
		return nil
	}

	switch rest[0] {
	case "version":
		printVersion()
		return nil
	case "init":
		return runInit(f, rest[1:])
	case "config":
		return runConfig(f)
	case "serve":
		merged, err := parseServeArgs(f, rest[1:])
		if err != nil {
			return err
		}
		return runServe(merged)
	case "session":
		return sessionCommand(f, rest[1:])
	case "agent":
		return agentCommand(f, rest[1:])
	case "node":
		return nodeCommand(f, rest[1:])
	case "command":
		return commandCommand(f, rest[1:])
	case "workspace":
		return workspaceCommand(f, rest[1:])
	case "tui":
		return tuiCommand(f, rest[1:])
	default:
		return fmt.Errorf("unknown command %q\n\n%s", rest[0], usage)
	}
}

// parseServeArgs applies the global flags that may also follow `serve`.
//
// A spawned node child is started with the role and the coordinator url as flags,
// so dropping them here would silently turn it into a second coordinator.
func parseServeArgs(f flags, args []string) (flags, error) {
	extra, rest, err := parseArgs(args)
	if err != nil {
		return f, err
	}
	if len(rest) > 0 {
		return f, fmt.Errorf("unexpected argument %q after serve", rest[0])
	}

	merged := f
	if extra.configPath != "" {
		merged.configPath = extra.configPath
	}
	if extra.role != "" {
		merged.role = extra.role
	}
	if extra.coordinatorURL != "" {
		merged.coordinatorURL = extra.coordinatorURL
	}
	if extra.nodeID != "" {
		merged.nodeID = extra.nodeID
	}
	return merged, nil
}

// nodeChildEnv marks a process that was started as a node child.
//
// A coordinator refuses to spawn a node child when it is itself one, so a
// configuration mistake cannot turn into an unbounded chain of processes.
const nodeChildEnv = "HIVE_NODE_CHILD"

// parseArgs extracts the global flags and returns the remaining arguments.
func parseArgs(args []string) (flags, []string, error) {
	var (
		f    flags
		rest []string
	)

	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-h" || a == "--help" || a == "help":
			f.help = true
		case a == "-config" || a == "--config":
			i++
			if i >= len(args) {
				return f, nil, errors.New("-config requires a path")
			}
			f.configPath = args[i]
		case a == "-role" || a == "--role":
			i++
			if i >= len(args) {
				return f, nil, errors.New("-role requires a value")
			}
			f.role = args[i]
		case a == "-coordinator-url" || a == "--coordinator-url":
			i++
			if i >= len(args) {
				return f, nil, errors.New("-coordinator-url requires a value")
			}
			f.coordinatorURL = args[i]
		case a == "-node-id" || a == "--node-id":
			i++
			if i >= len(args) {
				return f, nil, errors.New("-node-id requires a value")
			}
			f.nodeID = args[i]
		default:
			// Everything from the first positional argument onwards belongs to
			// the subcommand, which parses its own flags.
			rest = append(rest, args[i:]...)
			return f, rest, nil
		}
	}
	return f, rest, nil
}

func applyOverrides(cfg *config.Config, f flags) {
	if f.role != "" {
		cfg.Cluster.Role = config.Role(f.role)
	}
	if f.coordinatorURL != "" {
		cfg.Cluster.CoordinatorURL = f.coordinatorURL
	}
	if f.nodeID != "" {
		cfg.Cluster.NodeID = f.nodeID
	}
}

func printVersion() {
	fmt.Printf("hive %s\n", Version)
	fmt.Printf("hive protocol %s\n", v1.Current())
	if bi, ok := debug.ReadBuildInfo(); ok {
		fmt.Printf("go %s\n", bi.GoVersion)
	}
}

// runInit writes a starter configuration.
//
// It discovers the ACP agents on this machine and asks which one should be the
// default when there is more than one, so a first run starts from what is
// actually installed rather than from a placeholder the user has to replace.
func runInit(f flags, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	agentFlag := fs.String("agent", "", "the default agent, chosen without prompting")
	force := fs.Bool("force", false, "overwrite an existing configuration file")
	if err := parseArgsAndFlags(fs, args); err != nil {
		return err
	}

	cfgPath := f.configPath
	if cfgPath == "" {
		var err error
		if cfgPath, err = config.DefaultPath(); err != nil {
			return err
		}
	}

	// Refuse to overwrite unless asked: a configuration file is not ours to
	// replace.
	if _, err := os.Stat(cfgPath); err == nil && !*force {
		return fmt.Errorf("%s already exists; edit it, remove it, or pass -force", cfgPath)
	}

	agents := config.DiscoverAgents()
	reportDiscovery(agents)

	defaultAgent, err := chooseDefaultAgent(agents, *agentFlag)
	if err != nil {
		return err
	}

	content := config.RenderStarter(config.StarterOptions{
		Agents:       agents,
		DefaultAgent: defaultAgent,
	})

	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(cfgPath), err)
	}
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}

	fmt.Printf("\nwrote %s\n", cfgPath)
	if defaultAgent != "" {
		fmt.Printf("default agent: %s\n", defaultAgent)
	}
	fmt.Println("\nNext: `hive serve`, then `hive agent list` in another terminal.")
	return nil
}

// reportDiscovery prints what was found, and is explicit about what is a guess.
func reportDiscovery(agents []config.DiscoveredAgent) {
	if len(agents) == 0 {
		fmt.Println("No ACP agent was found on this machine's PATH.")
		fmt.Println("Hive will start and serve status, but it cannot create a session")
		fmt.Println("until one is installed and named in the configuration.")
		return
	}

	fmt.Printf("Found %d ACP agent(s) on PATH:\n\n", len(agents))
	for _, agent := range agents {
		note := ""
		if agent.NeedsVerification() {
			note = "  (ACP invocation is a convention: verify it)"
		}
		fmt.Printf("  %-16s %s%s\n", agent.Name, strings.Join(agent.Command, " "), note)
	}
}

// chooseDefaultAgent picks which agent a session uses by default.
func chooseDefaultAgent(agents []config.DiscoveredAgent, requested string) (string, error) {
	if requested != "" {
		for _, agent := range agents {
			if agent.Name == requested {
				return requested, nil
			}
		}
		if len(agents) == 0 {
			return "", fmt.Errorf("agent %q was requested but no ACP agent was found on PATH", requested)
		}
		return "", fmt.Errorf("agent %q was not found on PATH; found: %s", requested, agentNames(agents))
	}

	switch {
	case len(agents) == 0:
		return "", nil
	case len(agents) == 1:
		return agents[0].Name, nil
	case !isInteractive():
		// Nothing can be asked, so name every agent and let the user choose by
		// editing default_agent.
		fmt.Printf("\nSeveral agents were found. default_agent is set to %s; edit it to change.\n", agents[0].Name)
		return agents[0].Name, nil
	}

	return promptForAgent(agents)
}

// promptForAgent asks which agent should be the default.
func promptForAgent(agents []config.DiscoveredAgent) (string, error) {
	fmt.Println("\nWhich agent should be the default for new sessions?")
	fmt.Println()
	for i, agent := range agents {
		fmt.Printf("  %d) %s\n", i+1, agent.Name)
	}
	fmt.Printf("\nChoose 1-%d, or a name (default: %s): ", len(agents), agents[0].Name)

	reader := bufio.NewReader(os.Stdin)
	for attempt := 0; attempt < 3; attempt++ {
		line, err := reader.ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			// End of input: take the default rather than failing.
			return agents[0].Name, nil
		}

		choice := strings.TrimSpace(line)
		if choice == "" {
			return agents[0].Name, nil
		}

		if n, err := strconv.Atoi(choice); err == nil {
			if n >= 1 && n <= len(agents) {
				return agents[n-1].Name, nil
			}
		} else {
			for _, agent := range agents {
				if agent.Name == choice {
					return choice, nil
				}
			}
		}

		fmt.Printf("Enter 1-%d, or one of the names above: ", len(agents))
	}
	return "", errors.New("no valid choice after three attempts")
}

// isInteractive reports whether stdin is a terminal.
//
// A character device is not enough: /dev/null is one. Asking the terminal itself
// is what distinguishes a prompt from noise in a script.
func isInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

func agentNames(agents []config.DiscoveredAgent) string {
	names := make([]string, 0, len(agents))
	for _, agent := range agents {
		names = append(names, agent.Name)
	}
	return strings.Join(names, ", ")
}

func runConfig(f flags) error {
	cfgPath := f.configPath
	if cfgPath == "" {
		var err error
		if cfgPath, err = config.DefaultPath(); err != nil {
			return err
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	applyOverrides(&cfg, f)
	if err := cfg.Validate(); err != nil {
		return err
	}

	dataDir, err := cfg.EffectiveDataDir()
	if err != nil {
		return err
	}

	state := "loaded"
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		state = "not found, using defaults"
	}
	fmt.Printf("config file: %s (%s)\n", cfgPath, state)
	fmt.Printf("data dir:    %s\n", dataDir)
	fmt.Printf("role:        %s\n", cfg.EffectiveRole())

	if nodeID, err := cfg.EffectiveNodeID(); err == nil {
		fmt.Printf("node id:     %s\n", nodeID)
	}
	if agent, err := cfg.EffectiveDefaultAgent(); err == nil {
		fmt.Printf("agent:       %s\n", agent)
	}
	fmt.Println()

	effective := cfg
	effective.DataDir = dataDir
	b, err := toml.Marshal(effective)
	if err != nil {
		return fmt.Errorf("encode effective config: %w", err)
	}
	fmt.Print("effective configuration:\n\n")
	_, err = os.Stdout.Write(b)
	return err
}

func runServe(f flags) error {
	cfgPath := f.configPath
	if cfgPath == "" {
		var err error
		if cfgPath, err = config.DefaultPath(); err != nil {
			return err
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	applyOverrides(&cfg, f)
	if err := cfg.Validate(); err != nil {
		return err
	}

	log := logging.New(os.Stderr, cfg.Log.Level, cfg.Log.Format)

	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		log.Warn("no configuration file; Hive cannot run an agent",
			"path", cfgPath, "create_it_with", "hive init")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cfg.EffectiveRole() {
	case config.RoleCoordinator:
		return serveCoordinator(ctx, cfgPath, cfg, log)
	case config.RoleNode:
		return serveNode(ctx, cfg, log)
	default:
		return fmt.Errorf("unsupported role %q", cfg.EffectiveRole())
	}
}

func serveCoordinator(ctx context.Context, cfgPath string, cfg config.Config, log *slog.Logger) error {
	dataDir, err := cfg.EffectiveDataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	store, err := storage.Open(ctx, filepath.Join(dataDir, "coordinator.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	cert, err := node.LoadOrCreateCertificate(dataDir)
	if err != nil {
		return err
	}

	agentNames := cfg.AgentNames()
	defaultAgent, err := cfg.EffectiveDefaultAgent()
	if err != nil {
		// A coordinator with no agents can still serve status, listing, and
		// replay. It simply cannot create sessions, which is a better outcome
		// than refusing to start.
		log.Warn("no agents are configured; sessions cannot be created", "error", err)
		defaultAgent = ""
	}

	coordinator, err := daemon.NewCoordinator(daemon.CoordinatorOptions{
		Store:            store,
		Log:              log,
		Listen:           cfg.Cluster.Listen,
		Certificate:      cert,
		Agents:           agentNames,
		DefaultAgent:     defaultAgent,
		AllowedUsers:     cfg.Security.AllowedUsers,
		ControlSocket:    filepath.Join(dataDir, control.SocketFile),
		TransportPlugins: transportPluginSpecs(cfg),
	})
	if err != nil {
		return err
	}
	if err := coordinator.Start(ctx); err != nil {
		return err
	}
	defer coordinator.Close()

	log.Info("coordinator ready",
		"url", coordinator.URL(),
		"agents", len(agentNames),
		"control", coordinator.ControlSocket(),
	)

	switch {
	case !cfg.Cluster.SpawnsNode():
		log.Info("no node child: cluster.spawn_node is false")
	case os.Getenv(nodeChildEnv) != "":
		// This process is already a node child.
	case len(agentNames) == 0:
		// A node with nothing to run would start, find no agent, and exit. Say
		// what to fix instead of spawning a child that dies.
		log.Warn("no node child started: no agents are configured",
			"fix", "add an [agents.<name>] section to "+cfgPath, "see", "README.md")
	default:
		child, err := spawnNodeChild(cfgPath, coordinator.URL(), log)
		if err != nil {
			return err
		}
		defer stopNodeChild(child, log)
	}

	<-ctx.Done()
	log.Info("hive stopping")
	return nil
}

func spawnNodeChild(cfgPath, coordinatorURL string, log *slog.Logger) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve the hive executable: %w", err)
	}

	cmd := exec.Command(self,
		"-config", cfgPath,
		"serve",
		"-role", string(config.RoleNode),
		"-coordinator-url", coordinatorURL,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), nodeChildEnv+"=1")

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the node child process: %w", err)
	}
	log.Info("node child started", "pid", cmd.Process.Pid)
	return cmd, nil
}

func stopNodeChild(cmd *exec.Cmd, log *slog.Logger) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(ChildStopTimeout):
		log.Warn("node child did not stop in time; killing it", "pid", cmd.Process.Pid)
		_ = cmd.Process.Kill()
		<-done
	}
}

func serveNode(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	dataDir, err := cfg.EffectiveDataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	store, err := storage.OpenNode(ctx, filepath.Join(dataDir, "node.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	// v1 runs the node on the same machine, so it shares the locally generated
	// certificate. Multi-machine enrollment is deferred; see the plan.
	cert, err := node.LoadOrCreateCertificate(dataDir)
	if err != nil {
		return err
	}

	nodeID, err := cfg.EffectiveNodeID()
	if err != nil {
		return err
	}
	agentName, err := cfg.EffectiveDefaultAgent()
	if err != nil {
		return err
	}
	specs, err := agentPluginSpecs(cfg)
	if err != nil {
		return err
	}

	n, err := daemon.NewNode(daemon.NodeOptions{
		Store:          store,
		Log:            log,
		NodeID:         nodeID,
		Version:        Version,
		LeaseSeconds:   cfg.Cluster.LeaseSeconds,
		CoordinatorURL: cfg.Cluster.CoordinatorURL,
		TLSConfig:      node.ClientTLSConfig(cert),
		AgentPlugins:   specs,
		AgentPluginID:  agentName,
	})
	if err != nil {
		return err
	}

	log.Info("node starting",
		"node", nodeID,
		"coordinator", cfg.Cluster.CoordinatorURL,
		"agent", agentName,
		"plugins", len(specs),
	)
	return n.Run(ctx)
}

// agentPluginSpecs turns the configured agents into plugin process specs.
//
// Hive core holds no vendor knowledge: an agent is a protocol plus a command,
// and every ACP agent is served by the same adapter.
func agentPluginSpecs(cfg config.Config) ([]plugin.Spec, error) {
	dir := pluginDir()
	specs := make([]plugin.Spec, 0, len(cfg.Agents))

	for _, name := range cfg.AgentNames() {
		agentCfg := cfg.Agents[name]

		switch {
		case agentCfg.Protocol != "acp":
			return nil, fmt.Errorf("agents.%s: unsupported protocol %q, v1 supports acp", name, agentCfg.Protocol)
		case agentCfg.Endpoint != "":
			return nil, fmt.Errorf("agents.%s: remote agents are not implemented in v1", name)
		case len(agentCfg.Command) == 0:
			return nil, fmt.Errorf("agents.%s: command is required", name)
		}

		specs = append(specs, plugin.Spec{
			ID:      name,
			Type:    v1.PluginTypeAgent,
			Version: Version,
			Command: []string{
				filepath.Join(dir, "hive-plugin-acp"),
				"-id", name,
				"-version", Version,
				"-agent-command", strings.Join(agentCfg.Command, " "),
			},
			MaxRestarts:  3,
			RestartDelay: time.Second,
		})
	}
	return specs, nil
}

// transportPluginSpecs turns the enabled transports into plugin process specs.
//
// Options are passed through opaquely, so Hive core holds no vendor-specific
// configuration fields: a transport plugin decides what its own options mean.
func transportPluginSpecs(cfg config.Config) []plugin.Spec {
	dir := pluginDir()

	names := make([]string, 0, len(cfg.Transports))
	for name, transport := range cfg.Transports {
		if transport.Enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	specs := make([]plugin.Spec, 0, len(names))
	for _, name := range names {
		transport := cfg.Transports[name]

		options := make(map[string]string, len(transport.Options)+2)
		for key, value := range transport.Options {
			options[key] = value
		}
		if transport.Acknowledgement.Enabled {
			options["acknowledgement"] = "true"
			options["acknowledgement_reaction"] = transport.Acknowledgement.Reaction
		}

		command := []string{
			filepath.Join(dir, "hive-plugin-"+name),
			"-id", name,
			"-version", Version,
		}
		for _, key := range sortedKeys(options) {
			if value := options[key]; value != "" {
				command = append(command, "-option", key+"="+value)
			}
		}

		specs = append(specs, plugin.Spec{
			ID:           name,
			Type:         v1.PluginTypeTransport,
			Version:      Version,
			Command:      command,
			MaxRestarts:  3,
			RestartDelay: time.Second,
		})
	}
	return specs
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// pluginDir resolves where the plugin binaries live.
//
// They ship next to the hive binary. HIVE_PLUGIN_DIR overrides it, which is what
// makes `go run` and the tests workable.
func pluginDir() string {
	if dir := os.Getenv("HIVE_PLUGIN_DIR"); dir != "" {
		return dir
	}
	self, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(self)
}
