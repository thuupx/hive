package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thupham/hive/internal/config"
)

func TestDefaultIsValid(t *testing.T) {
	if err := config.Default().Validate(); err != nil {
		t.Fatalf("default config is invalid: %v", err)
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Cluster.Role != config.RoleAuto {
		t.Errorf("role = %q, want auto", cfg.Cluster.Role)
	}
	if cfg.EventStore.RetentionDays != 30 {
		t.Errorf("retention = %d, want 30", cfg.EventStore.RetentionDays)
	}
}

func TestLoadFullConfig(t *testing.T) {
	path := writeConfig(t, `
data_dir = "/tmp/hive-data"

[cluster]
role = "coordinator"

[log]
level = "debug"
format = "json"

[event_store]
retention_days = 7

[security]
allowed_users = ["slack:U123"]
allowed_channels = ["C123"]

[agents.claude]
protocol = "acp"
command = ["claude", "acp"]

[agents.remote-devin]
protocol = "acp"
endpoint = "wss://example.invalid/acp"

[transport.slack.acknowledgement]
enabled = true
mode = "reaction"
reaction = "eyes"
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Cluster.Role != config.RoleCoordinator {
		t.Errorf("role = %q", cfg.Cluster.Role)
	}
	if got := cfg.Agents["claude"].Command; len(got) != 2 || got[0] != "claude" {
		t.Errorf("claude command = %v", got)
	}
	if got := cfg.Agents["remote-devin"].Endpoint; got != "wss://example.invalid/acp" {
		t.Errorf("remote endpoint = %q", got)
	}
	if !cfg.Transports["slack"].Acknowledgement.Enabled {
		t.Error("slack acknowledgement should be enabled")
	}
	if len(cfg.Security.AllowedUsers) != 1 {
		t.Errorf("allowed_users = %v", cfg.Security.AllowedUsers)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	path := writeConfig(t, "[cluster]\nrole = \"auto\"\nprioirty = 10\n")
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected an error for an unknown key")
	}
	// go-toml v2 strict mode reports that unknown fields are present without
	// naming them, so only the rejection itself is asserted here.
	if !strings.Contains(err.Error(), "config") {
		t.Errorf("error should reference the config file: %v", err)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"role", "[cluster]\nrole = \"leader\"\n", "cluster.role"},
		{"log format", "[log]\nformat = \"xml\"\n", "log.format"},
		{"log level", "[log]\nlevel = \"loud\"\n", "log.level"},
		{"retention", "[event_store]\nretention_days = -1\n", "retention_days"},
		{"agent protocol", "[agents.x]\ncommand = [\"a\"]\n", "agents.x.protocol"},
		{"agent target", "[agents.x]\nprotocol = \"acp\"\n", "agents.x"},
		{"agent both", "[agents.x]\nprotocol = \"acp\"\ncommand = [\"a\"]\nendpoint = \"wss://x\"\n", "mutually exclusive"},
		{"ack mode", "[transport.slack.acknowledgement]\nenabled = true\nmode = \"sparkle\"\n", "acknowledgement.mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load(writeConfig(t, tc.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			err = cfg.Validate()
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestAcknowledgementDisabledSkipsModeCheck(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, "[transport.slack.acknowledgement]\nmode = \"whatever\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled acknowledgement must not be validated: %v", err)
	}
}

func TestEffectiveDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.Default()
	got, err := cfg.EffectiveDataDir()
	if err != nil {
		t.Fatalf("EffectiveDataDir: %v", err)
	}
	if want := filepath.Join(home, config.DefaultDirName, "data"); got != want {
		t.Errorf("data dir = %q, want %q", got, want)
	}

	cfg.DataDir = "~/custom"
	got, err = cfg.EffectiveDataDir()
	if err != nil {
		t.Fatalf("EffectiveDataDir: %v", err)
	}
	if want := filepath.Join(home, "custom"); got != want {
		t.Errorf("data dir = %q, want %q", got, want)
	}

	cfg.DataDir = "/absolute/path"
	if got, err = cfg.EffectiveDataDir(); err != nil || got != "/absolute/path" {
		t.Errorf("data dir = %q, err = %v", got, err)
	}
}

func TestDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := config.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if want := filepath.Join(home, config.DefaultDirName, "config.toml"); got != want {
		t.Errorf("default path = %q, want %q", got, want)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
