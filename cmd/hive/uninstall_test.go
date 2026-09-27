package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandboxHome points HOME at a temporary directory and creates the layout a real
// installation has, returning the paths.
func sandboxHome(t *testing.T) (home, dataDir, configPath, workspace string) {
	t.Helper()

	home = t.TempDir()
	t.Setenv("HOME", home)

	home = filepath.Join(home, ".hive")
	dataDir = filepath.Join(home, "data")
	configPath = filepath.Join(home, "config.toml")
	workspace = filepath.Join(home, "workspace")

	for _, dir := range []string{dataDir, workspace, filepath.Join(dataDir, "bin")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeFile(t, filepath.Join(dataDir, "coordinator.db"), "db")
	writeFile(t, filepath.Join(dataDir, "bin", "hive"), "binary")
	writeFile(t, filepath.Join(home, "config.toml"), "# config\n")
	writeFile(t, filepath.Join(workspace, "notes.txt"), "work")
	return home, dataDir, configPath, workspace
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// A full uninstall removes what Hive owns and keeps the user's workspace.
func TestUninstallKeepsTheWorkspace(t *testing.T) {
	home, dataDir, configPath, workspace := sandboxHome(t)

	plan, err := planUninstall(flags{}, false)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.homeDir != home {
		t.Fatalf("home = %q, want %q", plan.homeDir, home)
	}
	if plan.removeWorkspace {
		t.Fatal("the workspace should be kept by default")
	}
	if err := plan.apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if exists(dataDir) {
		t.Error("the data directory should be removed")
	}
	if exists(configPath) {
		t.Error("the configuration should be removed")
	}
	if !exists(workspace) {
		t.Error("the workspace should be kept")
	}
}

// `-workspace` removes the workspace when it is inside Hive's home.
func TestUninstallWorkspaceFlagRemovesItInsideHome(t *testing.T) {
	_, _, _, workspace := sandboxHome(t)

	plan, err := planUninstall(flags{}, true)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.removeWorkspace {
		t.Fatal("the workspace should be removed when asked for")
	}
	if err := plan.apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if exists(workspace) {
		t.Error("the workspace should be removed")
	}
}

// A workspace pointed at a project is never deleted by an uninstall, even with
// `-workspace`.
func TestUninstallRefusesAWorkspaceOutsideHome(t *testing.T) {
	home, _, configPath, _ := sandboxHome(t)

	// A workspace that is the user's project, not Hive's own directory.
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(project, "main.go"), "package main\n")
	writeFile(t, configPath, "workspace_dir = "+quote(project)+"\n")

	plan, err := planUninstall(flags{}, true)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.removeWorkspace {
		t.Fatal("a workspace outside the home must not be removed")
	}
	if !plan.workspaceOutsideHome {
		t.Fatal("the plan should record that the workspace was left")
	}
	if err := plan.apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !exists(filepath.Join(project, "main.go")) {
		t.Error("the user's project must be left alone")
	}
	if exists(filepath.Join(home, "data")) {
		t.Error("the data directory should still be removed")
	}
}

// The home directory goes when nothing the user kept is left in it.
func TestUninstallRemovesTheHomeWhenEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	home = filepath.Join(home, ".hive")

	// No workspace: only what Hive owns.
	for _, dir := range []string{filepath.Join(home, "data")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	writeFile(t, filepath.Join(home, "config.toml"), "# config\n")

	plan, err := planUninstall(flags{}, false)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := plan.apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if exists(home) {
		t.Error("the home directory should be removed once empty")
	}
}

// A custom data directory is honoured, because it is what the configuration
// says.
func TestUninstallUsesTheConfiguredDataDir(t *testing.T) {
	_, _, configPath, _ := sandboxHome(t)

	custom := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(custom, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, configPath, "data_dir = "+quote(custom)+"\n")

	plan, err := planUninstall(flags{}, false)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.dataDir != custom {
		t.Fatalf("data dir = %q, want %q", plan.dataDir, custom)
	}
	if err := plan.apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if exists(custom) {
		t.Error("the configured data directory should be removed")
	}
}

// The prompt defaults to no, so a bare newline leaves the installation alone.
func TestConfirmDefaultsToNo(t *testing.T) {
	for _, tc := range []struct {
		answer string
		want   bool
	}{
		{"y\n", true},
		{"yes\n", true},
		{"Y\n", true},
		{"n\n", false},
		{"\n", false},
		{"", false},
		{"sure\n", false},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			var out bytes.Buffer
			got, err := confirm(strings.NewReader(tc.answer), &out)
			if err != nil {
				t.Fatalf("confirm: %v", err)
			}
			if got != tc.want {
				t.Errorf("confirm(%q) = %v, want %v", tc.answer, got, tc.want)
			}
		})
	}
}

// The binaries the release install script put on PATH are found by the running
// executable's directory, and only Hive's own files there are removed.
func TestUninstallFindsBinariesOnPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"hive", "hive-plugin-zalo", "hive-coordinator", "other-tool"} {
		writeFile(t, filepath.Join(binDir, name), "x")
	}

	var plan uninstallPlan
	plan.resolveBinaries(filepath.Join(binDir, "hive"), filepath.Join(home, ".hive", "data"))

	if plan.binaryDir == "" {
		t.Fatal("the install directory should have been recognised")
	}
	got := map[string]bool{}
	for _, binary := range plan.binaries {
		got[filepath.Base(binary)] = true
	}
	for _, want := range []string{"hive", "hive-plugin-zalo", "hive-coordinator"} {
		if !got[want] {
			t.Errorf("%s should be removed", want)
		}
	}
	if got["other-tool"] {
		t.Error("a neighbouring tool must not be removed")
	}
}

// A build run from a checkout is reported, not deleted.
func TestUninstallKeepsACheckoutBuild(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	checkout := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(checkout, "hive"), "x")

	var plan uninstallPlan
	plan.resolveBinaries(filepath.Join(checkout, "hive"), filepath.Join(home, ".hive", "data"))

	if plan.binaryDir != "" || len(plan.binaries) != 0 {
		t.Fatal("a checkout build must not be removed")
	}
	if plan.keptBinaryDir != resolvePath(checkout) {
		t.Fatalf("keptBinaryDir = %q, want %q", plan.keptBinaryDir, resolvePath(checkout))
	}
}

// Only Hive's own binaries are named, so an uninstall never removes a neighbour.
func TestIsHiveBinary(t *testing.T) {
	for _, name := range []string{"hive", "hive-plugin-slack", "hive-coordinator", "hive-node", "hive-agent-devin", "hive-transport-zalo"} {
		if !isHiveBinary(name) {
			t.Errorf("%q should be a hive binary", name)
		}
	}
	for _, name := range []string{"hivemind", "git", "hive.test", "slack"} {
		if isHiveBinary(name) {
			t.Errorf("%q should not be a hive binary", name)
		}
	}
}

// quote renders a TOML string.
func quote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}
