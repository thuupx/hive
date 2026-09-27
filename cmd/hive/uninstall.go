package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/term"

	"github.com/thuupx/hive/internal/config"
)

// runUninstall removes this installation.
//
// It is the inverse of `init` and `service install` together: it stops and
// removes the service, then removes the binaries, the data directory, and the
// configuration. The workspace is the user's files, so it is kept unless
// `-workspace` asks for it, and even then only when it lives inside Hive's own
// home.
//
// Nothing is removed until the user says yes, and `-yes` is refused when there is
// no terminal to ask, so an unattended script cannot delete an installation by
// accident.
func runUninstall(f flags, args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	assumeYes := fs.Bool("yes", false, "remove without asking for confirmation")
	removeWorkspace := fs.Bool("workspace", false, "also remove the agent workspace and everything in it")
	if err := parseArgsAndFlags(fs, args); err != nil {
		return err
	}

	plan, err := planUninstall(f, *removeWorkspace)
	if err != nil {
		return err
	}

	plan.print(os.Stdout)

	if !*assumeYes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errors.New("refusing to remove an installation without confirmation; pass -yes to do it unattended")
		}
		ok, err := confirm(os.Stdin, os.Stdout)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("hive: nothing was removed")
			return nil
		}
	}

	if err := plan.apply(); err != nil {
		return err
	}
	plan.report(os.Stdout)
	return nil
}

// uninstallPlan is what an uninstall will remove, resolved before anything is.
type uninstallPlan struct {
	// homeDir is Hive's home, ~/.hive. It is removed only when it ends up empty.
	homeDir string

	// dataDir holds the database, the logs, the secrets, and the installed
	// binaries.
	dataDir string

	// configPath is the configuration file.
	configPath string

	// servicePath is the platform service definition, empty when there is none.
	servicePath string

	// workspace is where the agent works. It is kept unless removeWorkspace, and
	// even then only when it is inside homeDir.
	workspace       string
	removeWorkspace bool

	// workspaceOutsideHome records a `-workspace` request that was refused
	// because the directory is not Hive's to delete.
	workspaceOutsideHome bool

	// binaries are the hive binaries installed outside dataDir, and binaryDir is
	// where they are.
	binaries  []string
	binaryDir string

	// keptBinaryDir is a directory that holds hive binaries but is not one this
	// command removes, such as a source checkout's build directory.
	keptBinaryDir string
}

// planUninstall resolves what an uninstall will remove without removing it.
func planUninstall(f flags, removeWorkspace bool) (uninstallPlan, error) {
	home, err := config.HomeDir()
	if err != nil {
		return uninstallPlan{}, err
	}

	configPath := f.configPath
	if configPath == "" {
		if configPath, err = config.DefaultPath(); err != nil {
			return uninstallPlan{}, err
		}
	}

	// A configuration that cannot be read must not block removing an
	// installation: the built-in defaults are where an unconfigured Hive lives,
	// and a broken file is one of the things being removed.
	cfg, err := config.Load(configPath)
	if err != nil {
		cfg = config.Default()
	}

	dataDir, err := cfg.EffectiveDataDir()
	if err != nil {
		return uninstallPlan{}, err
	}
	workspace, err := cfg.EffectiveWorkspaceDir()
	if err != nil {
		workspace = ""
	}

	servicePath, err := serviceDefinitionPath()
	if err != nil {
		return uninstallPlan{}, err
	}

	plan := uninstallPlan{
		homeDir:     home,
		dataDir:     dataDir,
		configPath:  configPath,
		servicePath: servicePath,
		workspace:   workspace,
	}

	// The workspace is the user's files. `-workspace` removes it only when it is
	// inside Hive's own home: a workspace pointed at a project must never be
	// deleted by an uninstall.
	if removeWorkspace && workspace != "" {
		if within(workspace, home) {
			plan.removeWorkspace = true
		} else {
			plan.workspaceOutsideHome = true
		}
	}

	if self, err := os.Executable(); err == nil {
		plan.resolveBinaries(self, dataDir)
	}
	return plan, nil
}

// resolveBinaries finds the hive binaries that live outside the data directory.
//
// The release tarball installs them on PATH, and the service copies them into the
// data directory. Only a directory this installation owns is removed: a build run
// from a checkout is reported, not deleted.
func (p *uninstallPlan) resolveBinaries(executable, dataDir string) {
	dir := filepath.Dir(resolvePath(executable))
	dataDir = resolvePath(dataDir)

	switch {
	case within(dir, dataDir):
		// Removed with the data directory.
		return
	case dir == resolvePath(defaultInstallDir()):
		p.binaryDir = dir
		p.binaries = hiveBinariesIn(dir)
	default:
		if found := hiveBinariesIn(dir); len(found) > 0 {
			p.keptBinaryDir = dir
		}
	}
}

// defaultInstallDir is where the release install script puts the binaries.
func defaultInstallDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "bin")
}

// resolvePath resolves a path's symlinks, so two spellings of the same directory
// compare equal. On macOS /var is a symlink to /private/var, and an executable's
// own path is the resolved one.
func resolvePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// hiveBinariesIn lists the hive binaries in a directory.
func hiveBinariesIn(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var out []string
	for _, entry := range entries {
		if entry.IsDir() || !isHiveBinary(entry.Name()) {
			continue
		}
		out = append(out, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(out)
	return out
}

// isHiveBinary reports whether a file name is one Hive installs.
//
// The role aliases are hard links beside the binary, so they go with it.
func isHiveBinary(name string) bool {
	name = strings.TrimSuffix(name, executableSuffix(runtime.GOOS))
	switch {
	case name == "hive", name == aliasCoordinator, name == aliasNode:
		return true
	case strings.HasPrefix(name, "hive-plugin-"),
		strings.HasPrefix(name, aliasAgentPrefix),
		strings.HasPrefix(name, aliasTransportPrefix):
		return true
	default:
		return false
	}
}

// print writes what the plan will do, before it asks.
func (p uninstallPlan) print(w io.Writer) {
	fmt.Fprintln(w, "hive: this will remove")

	if p.servicePath != "" {
		fmt.Fprintf(w, "  the background service   %s\n", p.servicePath)
	}
	fmt.Fprintf(w, "  the installed binaries   %s\n", filepath.Join(p.dataDir, "bin"))
	fmt.Fprintf(w, "  the data directory       %s\n", p.dataDir)
	fmt.Fprintf(w, "  the configuration        %s\n", p.configPath)

	if p.binaryDir != "" {
		fmt.Fprintf(w, "  the binaries on PATH     %s\n", p.binaryDir)
	}
	if p.removeWorkspace {
		fmt.Fprintf(w, "  the workspace            %s\n", p.workspace)
	}

	if !p.removeWorkspace && p.workspace != "" {
		fmt.Fprintf(w, "\nhive: it will keep the workspace %s\n", p.workspace)
		fmt.Fprintln(w, "hive: pass -workspace to remove it and everything in it")
	}
	if p.workspaceOutsideHome {
		fmt.Fprintf(w, "\nhive: the workspace %s is outside %s, so it will be kept\n", p.workspace, p.homeDir)
		fmt.Fprintln(w, "hive: remove it yourself if you want it gone")
	}
	if p.keptBinaryDir != "" {
		fmt.Fprintf(w, "\nhive: the binaries in %s will be kept\n", p.keptBinaryDir)
		fmt.Fprintln(w, "hive: remove them yourself if you want them gone")
	}
	fmt.Fprintln(w)
}

// confirm asks a yes/no question and reads one answer.
//
// The default is no: a bare newline, a closed stream, or anything that is not a
// clear yes leaves the installation alone.
func confirm(in io.Reader, out io.Writer) (bool, error) {
	fmt.Fprint(out, "Remove the installation? [y/N] ")

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// apply removes what the plan names.
//
// It keeps going after a failure so one unremovable file does not leave half an
// installation behind, and reports everything that went wrong.
func (p uninstallPlan) apply() error {
	var errs []error
	remove := func(path string) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
	}

	// The service goes first: a daemon that restarts while its binaries and data
	// are being removed would recreate part of what is being removed. It is
	// stopped only when a definition is there to stop.
	if p.servicePath != "" {
		if _, err := os.Stat(p.servicePath); err == nil {
			stopService()
		}
		remove(p.servicePath)
	}

	for _, binary := range p.binaries {
		remove(binary)
	}

	// The data directory is removed whole: it holds the database, the logs, the
	// secrets, and the service's copy of the binaries.
	if err := os.RemoveAll(p.dataDir); err != nil {
		errs = append(errs, fmt.Errorf("remove %s: %w", p.dataDir, err))
	}
	remove(p.configPath)

	if p.removeWorkspace && p.workspace != "" {
		if err := os.RemoveAll(p.workspace); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", p.workspace, err))
		}
	}

	// The home directory is removed only when it is empty: a kept workspace, or
	// anything else the user put there, keeps it.
	_ = os.Remove(p.homeDir)

	return errors.Join(errs...)
}

// report says what happened.
func (p uninstallPlan) report(w io.Writer) {
	fmt.Fprintln(w, "hive: the installation was removed")
	if !p.removeWorkspace && p.workspace != "" {
		fmt.Fprintf(w, "hive: the workspace was kept: %s\n", p.workspace)
	}
	if p.keptBinaryDir != "" {
		fmt.Fprintf(w, "hive: the binaries in %s were kept\n", p.keptBinaryDir)
	}
}
