package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The generated file must load and validate, or `hive init` produces something
// Hive itself rejects.
func TestRenderStarterRoundTrips(t *testing.T) {
	agents := []DiscoveredAgent{
		{Name: "alpha", Command: []string{"alpha-acp"}, Source: SourceAdapter},
		{Name: "beta", Command: []string{"beta", "acp"}, Source: SourceConvention},
	}

	rendered := RenderStarter(StarterOptions{Agents: agents, DefaultAgent: "alpha"})

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(rendered), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the generated configuration does not load: %v\n%s", err, rendered)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the generated configuration does not validate: %v", err)
	}

	if cfg.DefaultAgent != "alpha" {
		t.Errorf("default_agent = %q, want alpha", cfg.DefaultAgent)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("agents = %+v", cfg.Agents)
	}
	if got := cfg.Agents["beta"].Command; len(got) != 2 || got[1] != "acp" {
		t.Errorf("beta command = %v", got)
	}
	if cfg.Log.Level != "info" || cfg.EventStore.RetentionDays != 30 {
		t.Errorf("defaults did not survive the round trip: %+v", cfg)
	}
	if !cfg.Cluster.SpawnsNode() {
		t.Error("spawn_node should default to true")
	}
}

// Every key is present, so nothing depends on the reader knowing what a zero
// value means.
func TestRenderStarterStatesEveryKey(t *testing.T) {
	rendered := RenderStarter(StarterOptions{})

	for _, key := range []string{
		"data_dir", "default_agent",
		"role", "node_id", "listen", "coordinator_url", "spawn_node", "lease_seconds",
		"level", "format", "retention_days",
		"allowed_users", "allowed_channels",
	} {
		if !strings.Contains(rendered, key+" =") {
			t.Errorf("the generated file does not state %q", key)
		}
	}
}

// The effective lease is stated explicitly, rather than "0 means something".
func TestRenderStarterStatesTheEffectiveLease(t *testing.T) {
	rendered := RenderStarter(StarterOptions{})

	if !strings.Contains(rendered, "lease_seconds = 30") {
		t.Fatalf("the generated file does not state the effective lease:\n%s", rendered)
	}
}

// With nothing found, the file says what to do rather than leaving a placeholder
// that looks configured.
func TestRenderStarterWithoutAgents(t *testing.T) {
	rendered := RenderStarter(StarterOptions{})

	// A commented example is expected; an active section is not.
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[agents.") {
			t.Errorf("no agent was found, so no active agent section should be written: %s", line)
		}
	}
	if !strings.Contains(rendered, "No ACP agent was found") {
		t.Error("the file should say that no agent was found")
	}
	if !strings.Contains(rendered, "# [agents.example]") {
		t.Error("the file should show a commented example to fill in")
	}

	// It still loads: an installation with no agent is a valid configuration.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(rendered), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(cfg.Agents) != 0 {
		t.Errorf("agents = %+v, want none", cfg.Agents)
	}
}

// A convention command is marked, so the user knows what to check.
func TestRenderStarterMarksConventionCommands(t *testing.T) {
	rendered := RenderStarter(StarterOptions{
		Agents:       []DiscoveredAgent{{Name: "beta", Command: []string{"beta", "acp"}, Source: SourceConvention}},
		DefaultAgent: "beta",
	})

	if !strings.Contains(rendered, "verify it") {
		t.Fatalf("a convention command should be marked:\n%s", rendered)
	}
}

func TestRenderStarterIsStable(t *testing.T) {
	opts := StarterOptions{
		Agents:       []DiscoveredAgent{{Name: "alpha", Command: []string{"alpha-acp"}, Source: SourceAdapter}},
		DefaultAgent: "alpha",
	}

	if RenderStarter(opts) != RenderStarter(opts) {
		t.Fatal("rendering is not deterministic")
	}
}
