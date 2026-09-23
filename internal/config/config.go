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
	"sort"
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
	DataDir string `toml:"data_dir"`

	// WorkspaceDir is where a run works when its workspace names no location for
	// the node. Empty uses the node process's working directory.
	//
	// A run must have a working directory: an agent that writes files needs to
	// know where, and ACP requires one to create a session.
	WorkspaceDir string                     `toml:"workspace_dir"`
	Cluster      ClusterConfig              `toml:"cluster"`
	Log          LogConfig                  `toml:"log"`
	EventStore   EventStoreConfig           `toml:"event_store"`
	Security     SecurityConfig             `toml:"security"`
	Agents       map[string]AgentConfig     `toml:"agents"`
	Transports   map[string]TransportConfig `toml:"transport"`

	// DefaultAgent names the agent used when a session does not choose one.
	// Empty falls back to the only configured agent, or the first in name
	// order when several exist.
	DefaultAgent string `toml:"default_agent"`
}

// ClusterConfig holds cluster role configuration.
//
// Coordinator failover is not part of v1, so election priority and lease
// settings are intentionally absent.
type ClusterConfig struct {
	Role Role `toml:"role"`

	// NodeID is the stable node identity. Empty uses the hostname.
	NodeID string `toml:"node_id"`

	// Listen is the coordinator's node link address, for example
	// "127.0.0.1:0". Empty uses a loopback default on an ephemeral port.
	Listen string `toml:"listen"`

	// CoordinatorURL is the coordinator's node link URL, for example
	// "wss://127.0.0.1:7433". A node uses it to connect.
	CoordinatorURL string `toml:"coordinator_url"`

	// SpawnNode makes a coordinator start a node child process. It defaults to
	// true, which is the single-machine installation: one command runs the
	// whole stack.
	SpawnNode *bool `toml:"spawn_node"`

	// LeaseSeconds is the node liveness claim. Empty uses the built-in
	// default.
	LeaseSeconds int `toml:"lease_seconds"`
}

// SpawnsNode reports whether a coordinator should start a node child process.
func (c ClusterConfig) SpawnsNode() bool {
	return c.SpawnNode == nil || *c.SpawnNode
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

	// AuthMethod selects which of the agent's advertised ACP auth methods to
	// use. Empty uses the first one the agent offers.
	//
	// An agent that advertises auth methods refuses to create a session until the
	// client authenticates, and Hive does not own agent credentials.
	AuthMethod string `toml:"auth_method"`

	// APIKeyEnv names an environment variable holding an API key, passed to the
	// agent as _meta.api_key. It is an environment variable so the key never
	// appears in this file.
	APIKeyEnv string `toml:"api_key_env"`
}

// TransportConfig holds per-transport presentation configuration.
type TransportConfig struct {
	// Enabled starts this transport as a plugin process.
	Enabled bool `toml:"enabled"`

	// Options are passed to the transport plugin as opaque key/value arguments,
	// so Hive core holds no vendor-specific configuration fields.
	Options map[string]string `toml:"options"`

	Acknowledgement AcknowledgementConfig `toml:"acknowledgement"`
}

// AcknowledgementConfig configures message acknowledgement. It is
// transport/plugin configuration, not Session or Command state.
//
// Acknowledgement means "Hive received this" and nothing more. It is not "the
// agent started" and not "the agent finished", and its failure never fails the
// operation.
type AcknowledgementConfig struct {
	// Enabled is a pointer so that unset means on. A user should not have to
	// discover that the received signal exists before it works.
	Enabled  *bool  `toml:"enabled"`
	Mode     string `toml:"mode"`
	Reaction string `toml:"reaction"`
}

// EnabledOr reports whether acknowledgement is on. Unset means on.
func (a AcknowledgementConfig) EnabledOr() bool {
	return a.Enabled == nil || *a.Enabled
}

// ReactionOr is the reaction to add, with the default when unset.
func (a AcknowledgementConfig) ReactionOr() string {
	if a.Reaction == "" {
		return "eyes"
	}
	return a.Reaction
}

// ModeOr is how the acknowledgement is shown, with the default when unset.
//
// The default has to be a valid mode, or a configuration that leaves it out would
// fail validation — which is exactly what happened when acknowledgement became on
// by default.
func (a AcknowledgementConfig) ModeOr() string {
	if a.Mode == "" {
		return AcknowledgementReaction
	}
	return a.Mode
}

// Acknowledgement modes.
const (
	AcknowledgementReaction = "reaction"
	AcknowledgementVisual   = "visual"
	AcknowledgementNone     = "none"
)

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
		// The strict-mode error does not name the offending key, so name the
		// unrecognized top-level keys instead. A typo in a config file is a common
		// mistake and the message is where the user finds out.
		if unknown := unknownTopLevelKeys(b); len(unknown) > 0 {
			return Config{}, fmt.Errorf("config %s: unknown keys: %s", path, strings.Join(unknown, ", "))
		}
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// unknownTopLevelKeys lists the top-level keys in a document that the Config
// struct does not define.
func unknownTopLevelKeys(body []byte) []string {
	var document map[string]any
	if err := toml.Unmarshal(body, &document); err != nil {
		return nil
	}

	known := map[string]bool{
		"data_dir": true, "default_agent": true, "cluster": true, "log": true,
		"event_store": true, "security": true, "agents": true, "transport": true,
	}

	var unknown []string
	for key := range document {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// Validate reports whether the configuration is usable.
func (c Config) Validate() error {
	switch c.Cluster.Role {
	case RoleAuto, RoleCoordinator, RoleNode:
	default:
		return fmt.Errorf("cluster.role: unknown role %q", c.Cluster.Role)
	}

	if c.Cluster.LeaseSeconds < 0 {
		return errors.New("cluster.lease_seconds: must not be negative")
	}

	// A node has nowhere to go without a coordinator, so fail before starting
	// rather than reconnecting in a loop.
	if c.Cluster.Role == RoleNode && c.Cluster.CoordinatorURL == "" {
		return errors.New("cluster.coordinator_url: required when cluster.role is node")
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
		if !t.Acknowledgement.EnabledOr() {
			continue
		}
		switch t.Acknowledgement.ModeOr() {
		case AcknowledgementReaction, AcknowledgementVisual, AcknowledgementNone:
		default:
			return fmt.Errorf("transport.%s.acknowledgement.mode: unknown mode %q, want reaction, visual, or none", name, t.Acknowledgement.Mode)
		}
	}

	return nil
}

// EffectiveNodeID resolves the node identity, falling back to the hostname.
func (c Config) EffectiveNodeID() (string, error) {
	if c.Cluster.NodeID != "" {
		return c.Cluster.NodeID, nil
	}
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("resolve hostname for the node id: %w", err)
	}
	return host, nil
}

// EffectiveRole resolves "auto" into a concrete role.
//
// A node needs a coordinator to connect to, so the presence of a coordinator
// URL decides: with one, this process is a node; without one, it is the
// coordinator.
func (c Config) EffectiveRole() Role {
	if c.Cluster.Role != RoleAuto {
		return c.Cluster.Role
	}
	if c.Cluster.CoordinatorURL != "" {
		return RoleNode
	}
	return RoleCoordinator
}

// EffectiveDefaultAgent resolves which agent a session uses by default.
func (c Config) EffectiveDefaultAgent() (string, error) {
	if c.DefaultAgent != "" {
		if _, ok := c.Agents[c.DefaultAgent]; !ok {
			return "", fmt.Errorf("default_agent: %q is not a configured agent", c.DefaultAgent)
		}
		return c.DefaultAgent, nil
	}

	switch len(c.Agents) {
	case 0:
		return "", errors.New("no agents are configured")
	case 1:
		for name := range c.Agents {
			return name, nil
		}
	}

	// Several agents and no explicit default: pick deterministically rather
	// than by map iteration order.
	names := make([]string, 0, len(c.Agents))
	for name := range c.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names[0], nil
}

// AgentNames returns the configured agent names in a stable order.
func (c Config) AgentNames() []string {
	names := make([]string, 0, len(c.Agents))
	for name := range c.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
