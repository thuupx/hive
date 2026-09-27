package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thuupx/hive/internal/config"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// demoManifest is a transport that declares one secret and one option.
func demoManifest() v1.PluginManifest {
	return v1.PluginManifest{
		ID:      "demo",
		Type:    v1.PluginTypeTransport,
		Secrets: []v1.PluginSecret{{Any: []string{"DEMO_TOKEN"}, Description: "the demo token"}},
		Config: &v1.PluginConfigManifest{
			Section: "transport.demo",
			Summary: "Demo transport.",
			Options: []v1.PluginOptionManifest{{Name: "greeting", Default: "hi"}},
		},
	}
}

// The guided setup writes an active transport, and the credential goes to the
// secrets file rather than to the configuration.
func TestWriteSetupWritesTheConfigurationAndTheSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfgPath := filepath.Join(home, ".hive", "config.toml")
	agents := []config.DiscoveredAgent{{Name: "alpha", Command: []string{"alpha-acp"}}}

	err := writeSetup(cfgPath, agents, []v1.PluginManifest{demoManifest()}, setupAnswers{
		defaultAgent: "alpha",
		transports:   []string{"demo"},
		options:      map[string]map[string]string{"demo": {"greeting": "hello"}},
		secrets:      map[string]string{"DEMO_TOKEN": "s3cret"},
		workspace:    filepath.Join(home, "work"),
		confirmed:    true,
	})
	if err != nil {
		t.Fatalf("writeSetup: %v", err)
	}

	body := readFile(t, cfgPath)
	for _, want := range []string{
		"[transport.demo]\nenabled = true",
		`greeting = "hello"`,
		`default_agent = "alpha"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the configuration does not contain %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "s3cret") {
		t.Error("a credential must never be written to the configuration")
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("the generated configuration does not load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the generated configuration does not validate: %v", err)
	}
	if !cfg.Transports["demo"].Enabled {
		t.Error("the chosen transport should be enabled")
	}
	if got := cfg.Transports["demo"].Options["greeting"]; got != "hello" {
		t.Errorf("greeting = %q, want the answer the user gave", got)
	}

	// The secret is in the file the daemon reads, readable by its owner only.
	dir, err := config.Default().EffectiveDataDir()
	if err != nil {
		t.Fatalf("data dir: %v", err)
	}
	envPath := filepath.Join(dir, serviceEnvFile)
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf("the secrets file was not written: %v", err)
	}
	if info.Mode().Perm() != serviceEnvMode {
		t.Errorf("secrets mode = %#o, want %#o", info.Mode().Perm(), serviceEnvMode)
	}
	if !strings.Contains(readFile(t, envPath), "DEMO_TOKEN") {
		t.Error("the secrets file does not name the token")
	}
}

// The daemon fills in what it does not already have, and an exported variable
// wins over the file.
func TestLoadSecretsFilePrefersTheEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir, err := config.Default().EffectiveDataDir()
	if err != nil {
		t.Fatalf("data dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := writeServiceEnv(dir, map[string]string{
		"HIVE_TEST_TOKEN": "from-file",
		"HIVE_TEST_OTHER": "other",
	}); err != nil {
		t.Fatalf("write secrets: %v", err)
	}
	t.Setenv("HIVE_TEST_TOKEN", "from-env")

	if loaded := loadSecretsFile(config.Default()); loaded != 1 {
		t.Fatalf("loaded = %d, want only the variable the environment did not set", loaded)
	}
	if got := os.Getenv("HIVE_TEST_TOKEN"); got != "from-env" {
		t.Errorf("HIVE_TEST_TOKEN = %q, want the environment to win", got)
	}
	if got := os.Getenv("HIVE_TEST_OTHER"); got != "other" {
		t.Errorf("HIVE_TEST_OTHER = %q, want the file to fill it in", got)
	}
}

// The workspace rule is enforced while the answer is being typed.
func TestValidateWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, tc := range []struct {
		value   string
		wantErr bool
	}{
		{filepath.Join(home, "project"), false},
		{"", false},
		{"relative/path", true},
		{"/", true},
		{home, true},
	} {
		err := validateWorkspace(tc.value)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateWorkspace(%q) = %v, want error %v", tc.value, err, tc.wantErr)
		}
	}
}

// The confirmation names the credentials without showing them.
func TestSetupSummaryHidesCredentialValues(t *testing.T) {
	summary := setupSummary(&setupAnswers{
		defaultAgent: "alpha",
		transports:   []string{"demo"},
		secrets:      map[string]string{"DEMO_TOKEN": "s3cret"},
		workspace:    "/tmp/work",
	}, []v1.PluginManifest{demoManifest()})

	if !strings.Contains(summary, "DEMO_TOKEN") {
		t.Error("the summary should name the credential")
	}
	if strings.Contains(summary, "s3cret") {
		t.Error("the summary must not show the value")
	}
}
