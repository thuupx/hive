package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// makeExecutable writes an executable file.
func makeExecutable(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// An adapter binary is named for what it is, so discovery needs no guess.
func TestDiscoverFindsAdapterBinaries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the PATH scan is a unix convention")
	}

	dir := t.TempDir()
	makeExecutable(t, dir, "myagent-acp")
	t.Setenv("PATH", dir)

	found := DiscoverAgents()

	var adapter *DiscoveredAgent
	for i := range found {
		if found[i].Name == "myagent" {
			adapter = &found[i]
		}
	}
	if adapter == nil {
		t.Fatalf("found = %+v, want myagent", found)
	}
	if adapter.Source != SourceAdapter {
		t.Errorf("source = %q, want adapter", adapter.Source)
	}
	if adapter.NeedsVerification() {
		t.Error("an adapter binary is named for what it is, not a guess")
	}
	if len(adapter.Command) != 1 || adapter.Command[0] != "myagent-acp" {
		t.Errorf("command = %v", adapter.Command)
	}
}

// A file that cannot be executed is not an agent.
func TestDiscoverIgnoresNonExecutableFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the PATH scan is a unix convention")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "broken-acp")
	if err := os.WriteFile(path, []byte("not executable"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("PATH", dir)

	for _, agent := range DiscoverAgents() {
		if agent.Name == "broken" {
			t.Fatal("a non-executable file was reported as an agent")
		}
	}
}

// A tool that exposes ACP as a mode is a convention, and the result says so.
func TestDiscoverMarksConventionCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the PATH scan is a unix convention")
	}

	dir := t.TempDir()
	makeExecutable(t, dir, "claude")
	t.Setenv("PATH", dir)

	var found *DiscoveredAgent
	for _, agent := range DiscoverAgents() {
		if agent.Name == "claude" {
			found = &agent
		}
	}
	if found == nil {
		t.Fatal("claude was not discovered")
	}
	if found.Source != SourceConvention {
		t.Fatalf("source = %q, want convention", found.Source)
	}
	if !found.NeedsVerification() {
		t.Fatal("a convention command must be marked for verification")
	}
	if len(found.Command) != 2 || found.Command[1] != ACPSubcommand {
		t.Errorf("command = %v", found.Command)
	}
}

// A tool found by both the scan and the convention list appears once.
func TestDiscoverDeduplicates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the PATH scan is a unix convention")
	}

	dir := t.TempDir()
	// `claude-acp` is an adapter, and `claude` is a convention tool. They are
	// different agents, so both should appear.
	makeExecutable(t, dir, "claude-acp")
	makeExecutable(t, dir, "claude")
	t.Setenv("PATH", dir)

	seen := map[string]int{}
	for _, agent := range DiscoverAgents() {
		seen[agent.Name]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Errorf("agent %q appeared %d times", name, count)
		}
	}
	if seen["claude"] != 1 {
		t.Errorf("seen = %v, want claude once", seen)
	}
}

func TestDiscoverWithNothingOnPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	found := DiscoverAgents()
	for _, agent := range found {
		// A tool on the real machine may still be reachable through an absolute
		// path, but nothing from the temporary directory should be.
		if strings.HasPrefix(agent.Command[0], dir) {
			t.Errorf("agent %q came from the empty PATH", agent.Name)
		}
	}
}

// Hive's own adapter is not an agent.
//
// Found in a live installation: "hive-plugin-acp" ends in the adapter suffix, and
// a release install puts it on the PATH — so every fresh `hive init` offered Hive
// itself as an agent called "hive-plugin", and the daemon spawned it.
func TestDiscoverIgnoresHivesOwnAdapter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the PATH scan is a unix convention")
	}

	dir := t.TempDir()
	makeExecutable(t, dir, "hive-plugin-acp")
	makeExecutable(t, dir, "myagent-acp")
	t.Setenv("PATH", dir)

	found := DiscoverAgents()

	var sawReal bool
	for _, agent := range found {
		if agent.Name == ownAdapterBase {
			t.Fatalf("discovered %q, which is Hive's own adapter", agent.Name)
		}
		sawReal = sawReal || agent.Name == "myagent"
	}
	if !sawReal {
		t.Fatal("a real adapter beside Hive's own should still be discovered")
	}
}
