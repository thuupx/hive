package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thuupx/hive/internal/config"
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

// A binary replaced by rename gets a new inode, so an alias made before the
// replacement still names the build that was replaced. Refreshing re-links the
// aliases the directory already has and creates none.
func TestRefreshRoleAliasesRelinksStaleAliases(t *testing.T) {
	dir := t.TempDir()
	hive := filepath.Join(dir, "hive")
	writeFakeBinary(t, hive)
	writeFakeBinary(t, filepath.Join(dir, "hive-plugin-zalo"))

	old := filepath.Join(dir, "old-hive")
	writeFakeBinary(t, old)
	stale := aliasPath(dir, aliasCoordinator)
	if err := ensureAlias(old, stale); err != nil {
		t.Fatalf("ensureAlias: %v", err)
	}
	if err := ensureAlias(old, aliasPath(dir, aliasTransportPrefix+"zalo")); err != nil {
		t.Fatalf("ensureAlias: %v", err)
	}
	orphan := aliasPath(dir, aliasTransportPrefix+"gone")
	if err := ensureAlias(old, orphan); err != nil {
		t.Fatalf("ensureAlias: %v", err)
	}

	if err := refreshRoleAliases(dir); err != nil {
		t.Fatalf("refreshRoleAliases: %v", err)
	}

	for _, tc := range []struct {
		alias  string
		target string
	}{
		{stale, hive},
		{aliasPath(dir, aliasTransportPrefix+"zalo"), filepath.Join(dir, "hive-plugin-zalo")},
	} {
		aliasInfo, err := os.Stat(tc.alias)
		if err != nil {
			t.Fatalf("stat %s: %v", tc.alias, err)
		}
		targetInfo, err := os.Stat(tc.target)
		if err != nil {
			t.Fatalf("stat %s: %v", tc.target, err)
		}
		if !os.SameFile(aliasInfo, targetInfo) {
			t.Errorf("%s still names the replaced binary, want %s", tc.alias, tc.target)
		}
	}

	// No alias is created for a role that was not already named, and an alias
	// whose binary is absent is left alone.
	if _, err := os.Lstat(aliasPath(dir, aliasNode)); !os.IsNotExist(err) {
		t.Error("refresh created an alias that was not already there")
	}
	orphanInfo, err := os.Stat(orphan)
	if err != nil {
		t.Fatalf("stat %s: %v", orphan, err)
	}
	oldInfo, err := os.Stat(old)
	if err != nil {
		t.Fatalf("stat %s: %v", old, err)
	}
	if !os.SameFile(orphanInfo, oldInfo) {
		t.Errorf("%s was touched although hive-plugin-gone is absent", orphan)
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
