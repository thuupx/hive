package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thupham/hive/internal/config"
)

// Windows resolves an executable by its extension, so an alias must carry one
// there and must not anywhere else.
func TestExecutableSuffix(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want string
	}{
		{"windows", ".exe"},
		{"darwin", ""},
		{"linux", ""},
	} {
		if got := executableSuffix(tc.goos); got != tc.want {
			t.Errorf("executableSuffix(%q) = %q, want %q", tc.goos, got, tc.want)
		}
	}
}

func TestEnsureRoleAliasesNamesEveryRole(t *testing.T) {
	dir := t.TempDir()
	writeFakeBinary(t, filepath.Join(dir, "hive"))
	writeFakeBinary(t, filepath.Join(dir, "hive-plugin-acp"))
	writeFakeBinary(t, filepath.Join(dir, "hive-plugin-slack"))

	if err := ensureRoleAliases(dir, []string{"devin", "hermes"}, []string{"slack"}); err != nil {
		t.Fatalf("ensureRoleAliases: %v", err)
	}

	for _, name := range []string{
		aliasCoordinator,
		aliasNode,
		aliasAgentPrefix + "devin",
		aliasAgentPrefix + "hermes",
		aliasTransportPrefix + "slack",
	} {
		if _, err := os.Lstat(aliasPath(dir, name)); err != nil {
			t.Errorf("no alias for %s: %v", name, err)
		}
	}
}

// A role whose binary is not installed is not named: a coordinator that runs no
// transport must not get an alias for one.
func TestEnsureRoleAliasesSkipsMissingBinaries(t *testing.T) {
	dir := t.TempDir()
	writeFakeBinary(t, filepath.Join(dir, "hive"))

	if err := ensureRoleAliases(dir, []string{"devin"}, []string{"slack"}); err != nil {
		t.Fatalf("ensureRoleAliases: %v", err)
	}
	if _, err := os.Lstat(aliasPath(dir, aliasAgentPrefix+"devin")); !os.IsNotExist(err) {
		t.Error("an agent alias was created without the adapter binary")
	}
}

// A missing alias falls back to the real binary, which is what keeps a
// read-only installation and a build run from a checkout working.
func TestRoleExecutableUsesTheAliasAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "hive")
	writeFakeBinary(t, real)

	if err := ensureAlias(real, aliasPath(dir, aliasNode)); err != nil {
		t.Fatalf("ensureAlias: %v", err)
	}

	if got, want := roleExecutable(dir, aliasNode, real), aliasPath(dir, aliasNode); got != want {
		t.Errorf("roleExecutable = %q, want the alias %q", got, want)
	}
	if got := roleExecutable(dir, aliasCoordinator, real); got != real {
		t.Errorf("roleExecutable = %q, want the fallback %q", got, real)
	}
}

// A stale alias is replaced, so an alias left by an earlier installation cannot
// point at a binary that is no longer there.
func TestEnsureAliasReplacesAnExistingAlias(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "hive")
	second := filepath.Join(dir, "hive-plugin-acp")
	writeFakeBinary(t, first)
	writeFakeBinary(t, second)

	alias := aliasPath(dir, aliasAgentPrefix+"devin")
	if err := ensureAlias(first, alias); err != nil {
		t.Fatalf("ensureAlias: %v", err)
	}
	if err := ensureAlias(second, alias); err != nil {
		t.Fatalf("ensureAlias: %v", err)
	}

	// SameFile compares what the alias actually names, which holds for a
	// symbolic link and for the hard link Windows uses.
	aliasInfo, err := os.Stat(alias)
	if err != nil {
		t.Fatalf("stat %s: %v", alias, err)
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		t.Fatalf("stat %s: %v", second, err)
	}
	if !os.SameFile(aliasInfo, secondInfo) {
		t.Errorf("the alias does not name %s", second)
	}
}

func writeFakeBinary(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// The aliases belong beside the binary, which installBinary puts in a bin
// subdirectory of the data directory. Naming them beside the data directory
// leaves the supervisor starting the real binary, so the coordinator keeps the
// name hive while the plugins, named at runtime, do not.
func TestServiceRunPathNamesBesideTheBinary(t *testing.T) {
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	binary := filepath.Join(binDir, "hive")
	writeFakeBinary(t, binary)

	run, err := serviceRunPath(binary, config.Config{})
	if err != nil {
		t.Fatalf("serviceRunPath: %v", err)
	}
	if want := aliasPath(binDir, aliasCoordinator); run != want {
		t.Errorf("serviceRunPath = %q, want %q", run, want)
	}
}
