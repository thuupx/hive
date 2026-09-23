// Package config loads and validates the Hive configuration.
//
// The configuration is TOML. Unknown keys are rejected so a typo fails
// loudly instead of being silently ignored.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// DefaultDirName is the Hive home directory under the user's home.
const DefaultDirName = ".hive"

// Role selects the cluster role of a process.
type Role string

const (
	// RoleAuto lets the process decide its role. This is the default.
	RoleAuto Role = "auto"
	// RoleCoordinator forces the control-plane role.
	RoleCoordinator Role = "coordinator"
	// RoleNode forces the execution-plane role.
	RoleNode Role = "node"
)

// Config is the root Hive configuration.
type Config struct {
	// DataDir is the data directory. Empty means ~/.hive/data.
	DataDir    string                     `toml:"data_dir"`
	Cluster    ClusterConfig              `toml:"cluster"`
	Log        LogConfig                  `toml:"log"`
	EventStore EventStoreConfig           `toml:"event_store"`
	Security   SecurityConfig             `toml:"security"`
	Agents     map[string]AgentConfig     `toml:"agents"`
	Transports map[string]TransportConfig `toml:"transport"`
}

// ClusterConfig holds cluster role configuration.
//
// Coordinator failover is not part of v1, so election priority and lease
// settings are intentionally absent.
type ClusterConfig struct {
	Role Role `toml:"role"`
}

// LogConfig holds logging configuration.
type LogConfig struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
}

// EventStoreConfig holds event retention configuration. Event retention and
// session retention are independent.
type EventStoreConfig struct {
	RetentionDays int `toml:"retention_days"`
}

// SecurityConfig holds the owner-principal allow lists. Unknown access is
// denied by default.
type SecurityConfig struct {
	AllowedUsers    []string `toml:"allowed_users"`
	AllowedChannels []string `toml:"allowed_channels"`
}

// AgentConfig describes one agent target. Hive core holds no vendor
// knowledge: an agent is a protocol plus either a local command or a remote
// endpoint.
type AgentConfig struct {
	Protocol string   `toml:"protocol"`
	Command  []string `toml:"command"`
	Endpoint string   `toml:"endpoint"`
}

// TransportConfig holds per-transport presentation configuration.
type TransportConfig struct {
	Acknowledgement AcknowledgementConfig `toml:"acknowledgement"`
}

// AcknowledgementConfig configures optional message acknowledgement. It is
// transport/plugin configuration, not Session or Command state.
type AcknowledgementConfig struct {
	Enabled  bool   `toml:"enabled"`
	Mode     string `toml:"mode"`
	Reaction string `toml:"reaction"`
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		Cluster:    ClusterConfig{Role: RoleAuto},
		Log:        LogConfig{Level: "info", Format: "text"},
		EventStore: EventStoreConfig{RetentionDays: 30},
		Agents:     map[string]AgentConfig{},
		Transports: map[string]TransportConfig{},
	}
}

// HomeDir returns the Hive home directory.
func HomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, DefaultDirName), nil
}

// DefaultPath returns the default configuration file path.
func DefaultPath() (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.toml"), nil
}

// Load reads the configuration at path. A missing file is not an error: the
// built-in defaults are returned.
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	dec := toml.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate reports whether the configuration is usable.
func (c Config) Validate() error {
	switch c.Cluster.Role {
	case RoleAuto, RoleCoordinator, RoleNode:
	default:
		return fmt.Errorf("cluster.role: unknown role %q", c.Cluster.Role)
	}

	switch c.Log.Format {
	case "text", "json":
	default:
		return fmt.Errorf("log.format: unknown format %q, want text or json", c.Log.Format)
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("log.level: unknown level %q", c.Log.Level)
	}

	if c.EventStore.RetentionDays < 0 {
		return errors.New("event_store.retention_days: must not be negative")
	}

	for name, a := range c.Agents {
		if name == "" {
			return errors.New("agents: empty agent name")
		}
		if a.Protocol == "" {
			return fmt.Errorf("agents.%s.protocol: required", name)
		}
		switch {
		case len(a.Command) == 0 && a.Endpoint == "":
			return fmt.Errorf("agents.%s: either command or endpoint is required", name)
		case len(a.Command) > 0 && a.Endpoint != "":
			return fmt.Errorf("agents.%s: command and endpoint are mutually exclusive", name)
		}
	}

	for name, t := range c.Transports {
		if !t.Acknowledgement.Enabled {
			continue
		}
		switch t.Acknowledgement.Mode {
		case "reaction", "visual", "none":
		default:
			return fmt.Errorf("transport.%s.acknowledgement.mode: unknown mode %q, want reaction, visual, or none", name, t.Acknowledgement.Mode)
		}
	}

	return nil
}

// EffectiveDataDir resolves the data directory, creating nothing.
func (c Config) EffectiveDataDir() (string, error) {
	if c.DataDir != "" {
		return expandHome(c.DataDir)
	}
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "data"), nil
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}
