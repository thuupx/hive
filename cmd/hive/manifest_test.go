package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thuupx/hive/internal/config"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// writeScript writes an executable stub, standing in for a plugin binary.
func writeScript(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// The core learns what a plugin needs by asking it, so a plugin it has never
// heard of is discovered the same way as one it ships with.
func TestPluginManifestsDiscoverByAsking(t *testing.T) {
	dir := t.TempDir()

	writeScript(t, dir, "hive-plugin-demo", `cat <<'EOF'
{"id":"demo","type":"transport","secrets":[{"any":["DEMO_TOKEN","DEMO_TOKEN_ALIAS"]}],"config":{"section":"transport.demo"}}
EOF`)
	writeScript(t, dir, "hive-plugin-agentish", `cat <<'EOF'
{"id":"agentish","type":"agent"}
EOF`)
	// A binary that cannot describe itself, and a file that is not a plugin, must
	// not stop the ones that can.
	writeScript(t, dir, "hive-plugin-broken", "exit 1")
	writeScript(t, dir, "hive", "exit 1")

	manifests := pluginManifests(dir)
	if len(manifests) != 2 {
		t.Fatalf("manifests = %v, want demo and agentish", manifests)
	}
	if got := manifests["demo"].Secrets; len(got) != 1 || len(got[0].Any) != 2 || got[0].Any[0] != "DEMO_TOKEN" {
		t.Fatalf("demo secrets = %v", got)
	}

	transports := transportManifests(dir)
	if len(transports) != 1 || transports[0].ID != "demo" {
		t.Fatalf("transports = %v, want only demo", transports)
	}
}

// The secrets a service carries come from the transport's manifest, not from the
// core knowing which transport wants which token.
func TestServiceSecretsComeFromTheManifest(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "s3cret")
	t.Setenv("DEMO_ALIAS_B", "alias")
	t.Setenv("HIVE_TEST_MISSING_TOKEN", "")

	cfg := config.Config{Transports: map[string]config.TransportConfig{
		"demo": {Enabled: true},
		"off":  {Enabled: false},
	}}
	manifests := map[string]v1.PluginManifest{
		"demo": {Secrets: []v1.PluginSecret{
			{Any: []string{"DEMO_TOKEN"}},
			// An alias: either name satisfies the requirement.
			{Any: []string{"DEMO_ALIAS_A", "DEMO_ALIAS_B"}},
			{Any: []string{"HIVE_TEST_MISSING_TOKEN"}},
		}},
		"off": {Secrets: []v1.PluginSecret{{Any: []string{"OFF_TOKEN"}}}},
	}

	found, missing := serviceSecrets(cfg, manifests, nil)
	if found["DEMO_TOKEN"] != "s3cret" {
		t.Fatalf("DEMO_TOKEN = %q, want it captured", found["DEMO_TOKEN"])
	}
	if found["DEMO_ALIAS_B"] != "alias" {
		t.Fatalf("DEMO_ALIAS_B = %q, want the alias captured", found["DEMO_ALIAS_B"])
	}
	if found["PATH"] == "" {
		t.Error("PATH should be captured so the service can find agents")
	}
	if len(missing) != 1 || missing[0] != "HIVE_TEST_MISSING_TOKEN" {
		t.Fatalf("missing = %v", missing)
	}
	// A disabled transport is not asked for its secret.
	if _, ok := found["OFF_TOKEN"]; ok {
		t.Error("a disabled transport's secret should not be captured")
	}
}

// A secret the guided setup already stored is satisfied, so re-installing from a
// shell that does not have it warns about nothing and erases nothing.
//
// Found by running it: `hive service install` after `hive init` overwrote the
// secrets file with just PATH, and the daemon then started without its token.
func TestServiceSecretsKeepsWhatIsAlreadyStored(t *testing.T) {
	t.Setenv("HIVE_TEST_MISSING_TOKEN", "")

	cfg := config.Config{Transports: map[string]config.TransportConfig{"demo": {Enabled: true}}}
	manifests := map[string]v1.PluginManifest{
		"demo": {Secrets: []v1.PluginSecret{{Any: []string{"HIVE_TEST_MISSING_TOKEN"}}}},
	}

	existing := map[string]string{"HIVE_TEST_MISSING_TOKEN": "from-the-file", "PATH": "/usr/bin"}

	found, missing := serviceSecrets(cfg, manifests, existing)
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want none: the file already has it", missing)
	}
	if found["HIVE_TEST_MISSING_TOKEN"] != "" {
		t.Error("a value that is only in the file should not be collected again")
	}

	// And with neither, it is still reported.
	if _, missing := serviceSecrets(cfg, manifests, nil); len(missing) != 1 {
		t.Fatalf("missing = %v, want the variable reported", missing)
	}
}

// An allow list names principals, and a principal names its transport.
func TestAllowsTransport(t *testing.T) {
	allowed := []string{"slack:U1", "zalo:u2"}
	if !allowsTransport(allowed, "zalo") {
		t.Error("zalo should be allowed")
	}
	if allowsTransport(allowed, "telegram") {
		t.Error("telegram should not be allowed")
	}
}
