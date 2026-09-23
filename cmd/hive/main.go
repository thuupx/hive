// Command hive is the Hive daemon and CLI entry point.
//
// A single binary serves every cluster role. In the single-machine default, one
// `hive serve` runs the coordinator and starts a node child process, so the whole
// stack comes up with one command.
package main

import (
	"context"
	"errors"
	"fmt"

	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/thupham/hive/internal/config"
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
  hive [flags] <command>

Commands:
  version        print build and protocol version
  config         validate the configuration and print effective values
  serve          run the Hive daemon

Flags:
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
	command        string
}

func run(args []string) error {
	f, err := parseArgs(args)
	if err != nil {
		return err
	}
	if f.command == "" || f.command == "help" {
		fmt.Print(usage)
		return nil
	}
	if f.command == "version" {
		printVersion()
		return nil
	}

	cfgPath := f.configPath
	if cfgPath == "" {
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

	switch f.command {
	case "config":
		return printConfig(cfgPath, cfg)
	case "serve":
		return serve(cfgPath, cfg)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", f.command, usage)
	}
}

func parseArgs(args []string) (flags, error) {
	var f flags

	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-h" || a == "--help" || a == "help":
			f.command = "help"
			return f, nil
		case a == "-config" || a == "--config":
			i++
			if i >= len(args) {
				return f, errors.New("-config requires a path")
			}
			f.configPath = args[i]
		case a == "-role" || a == "--role":
			i++
			if i >= len(args) {
				return f, errors.New("-role requires a value")
			}
			f.role = args[i]
		case a == "-coordinator-url" || a == "--coordinator-url":
			i++
			if i >= len(args) {
				return f, errors.New("-coordinator-url requires a value")
			}
			f.coordinatorURL = args[i]
		case a == "-node-id" || a == "--node-id":
			i++
			if i >= len(args) {
				return f, errors.New("-node-id requires a value")
			}
			f.nodeID = args[i]
		case strings.HasPrefix(a, "-"):
			return f, fmt.Errorf("unknown flag %q", a)
		case f.command == "":
			f.command = a
		default:
			return f, fmt.Errorf("unexpected argument %q", a)
		}
	}
	return f, nil
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

func printConfig(path string, cfg config.Config) error {
	dataDir, err := cfg.EffectiveDataDir()
	if err != nil {
		return err
	}

	state := "loaded"
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		state = "not found, using defaults"
	}
	fmt.Printf("config file: %s (%s)\n", path, state)
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

func serve(cfgPath string, cfg config.Config) error {
	log := logging.New(os.Stderr, cfg.Log.Level, cfg.Log.Format)

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

	coordinator, err := daemon.NewCoordinator(daemon.CoordinatorOptions{
		Store:       store,
		Log:         log,
		Listen:      cfg.Cluster.Listen,
		Certificate: cert,
	})
	if err != nil {
		return err
	}
	if err := coordinator.Start(ctx); err != nil {
		return err
	}
	defer coordinator.Close()

	log.Info("coordinator ready", "url", coordinator.URL())

	if cfg.Cluster.SpawnsNode() {
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
		"serve",
		"-role", string(config.RoleNode),
		"-coordinator-url", coordinatorURL,
		"-config", cfgPath,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

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
