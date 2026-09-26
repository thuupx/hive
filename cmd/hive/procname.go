package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/thupham/hive/internal/config"
)

// Process names.
//
// Every role runs the same binary, and every platform reports a process by the
// name of the executable it was started from. One binary therefore shows up
// under the same name for the coordinator, the node, and every agent — which is
// exactly the set an operator wants to tell apart.
//
// A role-named alias next to the binary is what distinguishes them. The alias
// is a hard link — the same file under another name — so it reads as the role
// whether the name is taken from the executable path, the command name, or the
// command line. It needs no privilege on any platform and costs no extra disk,
// and it sits beside the binary, so both are on the same volume.
//
// A platform that refuses the alias is not an error: the process keeps the real
// binary's name and nothing else changes.
const (
	aliasCoordinator = "hive-coordinator"
	aliasNode        = "hive-node"

	aliasAgentPrefix     = "hive-agent-"
	aliasTransportPrefix = "hive-transport-"
)

// processAlias is a role-named name for a binary.
type processAlias struct {
	name   string
	target string
}

// executableSuffix is what a platform appends to an executable's name. Windows
// resolves an executable by its extension, so an alias without one is not one.
func executableSuffix(goos string) string {
	if goos == "windows" {
		return ".exe"
	}
	return ""
}

// aliasPath is where the alias for name lives.
func aliasPath(dir, name string) string {
	return filepath.Join(dir, name+executableSuffix(runtime.GOOS))
}

// roleAliases are the aliases an installation names.
func roleAliases(dir string, agents, transports []string) []processAlias {
	hive := filepath.Join(dir, "hive")
	aliases := []processAlias{
		{aliasCoordinator, hive},
		{aliasNode, hive},
	}
	for _, name := range agents {
		aliases = append(aliases, processAlias{
			name:   aliasAgentPrefix + name,
			target: filepath.Join(dir, "hive-plugin-acp"),
		})
	}
	for _, name := range transports {
		aliases = append(aliases, processAlias{
			name:   aliasTransportPrefix + name,
			target: filepath.Join(dir, "hive-plugin-"+name),
		})
	}
	return aliases
}

// ensureRoleAliases names every role whose binary is installed.
//
// A role whose binary is absent is skipped, so a coordinator that runs no
// transport does not get an alias for one.
func ensureRoleAliases(dir string, agents, transports []string) error {
	for _, alias := range roleAliases(dir, agents, transports) {
		if _, err := os.Stat(alias.target); err != nil {
			continue
		}
		if err := ensureAlias(alias.target, aliasPath(dir, alias.name)); err != nil {
			return fmt.Errorf("alias %s: %w", alias.name, err)
		}
	}
	return nil
}

// ensureAlias makes alias name target, replacing whatever was there.
//
// It is a hard link, on every platform. The alias is then the same file under
// another name, so every way a process name is read — the executable path, the
// command name, and the command line — reports the alias. A symbolic link does
// not: a reader that resolves it sees the real binary's name instead, which is
// what a process monitor does.
//
// A hard link also needs no privilege, where a symbolic link does on Windows.
// The target is absolute and the alias sits beside it, so both are on the same
// volume.
func ensureAlias(target, alias string) error {
	if err := os.Remove(alias); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Link(target, alias)
}

// roleExecutable is the path to run for a role: its alias when the installation
// has one, and the real binary otherwise.
//
// Falling back matters. A directory that cannot be written, or a build run
// straight from a checkout, still runs; the only thing lost is the name.
func roleExecutable(dir, alias, fallback string) string {
	path := aliasPath(dir, alias)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return fallback
}

// topLevelAlias is the name the process the supervisor starts takes.
func topLevelAlias(cfg config.Config) string {
	if cfg.EffectiveRole() == config.RoleNode {
		return aliasNode
	}
	return aliasCoordinator
}

// serviceRunPath names the roles and returns the path the supervisor should
// start: the role alias when the installation has one, and the real binary
// otherwise.
//
// The aliases belong beside the binary installBinary returned, not beside the
// data directory: the binaries live in a bin subdirectory of it, and an alias
// created one level up is never found.
func serviceRunPath(binary string, cfg config.Config) (string, error) {
	dir := filepath.Dir(binary)
	err := ensureRoleAliases(dir, cfg.AgentNames(), enabledTransportNames(cfg))
	return roleExecutable(dir, topLevelAlias(cfg), binary), err
}

// nameRoles gives this installation's processes a name of their own.
//
// The coordinator and the node are this binary, so their aliases live beside
// it; the plugins are found where the plugins are, which HIVE_PLUGIN_DIR can
// move. It is best-effort: a name is a convenience, and an installation whose
// directory cannot be written must not fail to start over one.
func nameRoles(cfg config.Config, log *slog.Logger) {
	self, err := os.Executable()
	if err != nil {
		log.Warn("could not name the role processes", "error", err)
		return
	}
	if err := ensureRoleAliases(filepath.Dir(self), nil, nil); err != nil {
		log.Warn("could not name the coordinator and node processes", "error", err)
	}
	if err := ensureRoleAliases(pluginDir(), cfg.AgentNames(), enabledTransportNames(cfg)); err != nil {
		log.Warn("could not name the plugin processes", "error", err)
	}
}
