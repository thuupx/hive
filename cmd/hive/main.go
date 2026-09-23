// Command hive is the Hive daemon and CLI entry point.
//
// A single binary serves every cluster role. See the configuration
// cluster.role setting.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/pelletier/go-toml/v2"

	"github.com/thupham/hive/internal/config"
	"github.com/thupham/hive/internal/logging"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// Version is the Hive build version.
const Version = "0.1.0-dev"

const usage = `hive - personal agent gateway

Usage:
  hive [flags] <command>

Commands:
  version        print build and protocol version
  config         validate the configuration and print effective values
  serve          run the Hive daemon

Flags:
  -config <path>  configuration file (default ~/.hive/config.toml)
  -h, --help      show this help
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "hive:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfgPath, cmd, err := parseArgs(args)
	if err != nil {
		return err
	}
	if cmd == "" || cmd == "help" {
		fmt.Print(usage)
		return nil
	}

	if cmd == "version" {
		printVersion()
		return nil
	}

	if cfgPath == "" {
		if cfgPath, err = config.DefaultPath(); err != nil {
			return err
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	switch cmd {
	case "config":
		return printConfig(cfgPath, cfg)
	case "serve":
		return serve(cfgPath, cfg)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

func parseArgs(args []string) (cfgPath, cmd string, err error) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-h" || a == "--help" || a == "help":
			return "", "help", nil
		case a == "-config" || a == "--config":
			i++
			if i >= len(args) {
				return "", "", errors.New("-config requires a path")
			}
			cfgPath = args[i]
		case strings.HasPrefix(a, "-"):
			return "", "", fmt.Errorf("unknown flag %q", a)
		case cmd == "":
			cmd = a
		default:
			return "", "", fmt.Errorf("unexpected argument %q", a)
		}
	}
	return cfgPath, cmd, nil
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
	fmt.Printf("data dir:    %s\n\n", dataDir)

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
	log.Info("hive starting",
		logging.FieldComponent, "daemon",
		logging.FieldRole, string(cfg.Cluster.Role),
		"config", cfgPath,
	)

	dataDir, err := cfg.EffectiveDataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	log.Info("data directory ready", logging.FieldComponent, "storage", "path", dataDir)

	log.Warn("no subsystems are active yet; coordinator, node, plugins, and transports land in M1-M8 of hive-v1-implementation-plan.md",
		logging.FieldComponent, "daemon",
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	log.Info("hive stopping", logging.FieldComponent, "daemon")
	return nil
}
