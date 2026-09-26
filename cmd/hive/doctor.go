package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/thupham/hive/internal/client"
	"github.com/thupham/hive/internal/config"
	"github.com/thupham/hive/internal/storage"
)

// finding is one thing doctor looked at.
type finding struct {
	level  string // "ok", "warn", or "fail"
	what   string
	detail string

	// fix is what to do about it. A finding without one is a dead end for a user.
	fix string
}

// runDoctor checks the installation and reports what is wrong.
//
// The point is to answer "why does it not work" without reading source. Every
// finding that is not ok carries what to do about it, because a diagnosis without
// a remedy is only half an answer.
func runDoctor(f flags) error {
	cfg, err := loadConfig(f)
	if err != nil {
		fmt.Println("fail  configuration")
		fmt.Printf("      %v\n", err)
		fmt.Println("      fix: run `hive init` to write a starter configuration")
		return errors.New("the configuration could not be read")
	}

	var findings []finding
	findings = append(findings, checkConfig(cfg)...)
	findings = append(findings, checkWorkspace(cfg)...)
	findings = append(findings, checkGuardedFolder(cfg)...)
	findings = append(findings, checkDataDir(cfg)...)
	findings = append(findings, checkAgents(cfg)...)
	findings = append(findings, checkTransports(cfg)...)
	findings = append(findings, checkDaemon(cfg)...)
	findings = append(findings, checkStore(cfg)...)
	findings = append(findings, checkService()...)

	return report(findings)
}

// report prints the findings and returns an error when something failed.
func report(findings []finding) error {
	failed, warned := 0, 0
	for _, f := range findings {
		fmt.Printf("%-5s %s\n", f.level, f.what)
		if f.detail != "" {
			fmt.Printf("      %s\n", f.detail)
		}
		if f.fix != "" {
			fmt.Printf("      fix: %s\n", f.fix)
		}
		switch f.level {
		case "fail":
			failed++
		case "warn":
			warned++
		}
	}

	fmt.Println()
	switch {
	case failed > 0:
		fmt.Printf("%d problem(s) and %d warning(s)\n", failed, warned)
		return errors.New("doctor found problems")
	case warned > 0:
		fmt.Printf("no problems, %d warning(s)\n", warned)
	default:
		fmt.Println("everything checks out")
	}
	return nil
}

// checkConfig reports what the configuration says about itself.
func checkConfig(cfg config.Config) []finding {
	var out []finding

	if len(cfg.Agents) == 0 {
		out = append(out, finding{
			level:  "fail",
			what:   "no agents are configured",
			detail: "Hive can serve status but cannot create a session.",
			fix:    "add an [agents.<name>] section naming an ACP agent you have installed",
		})
		return out
	}

	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)

	what := "agents configured"
	if cfg.DefaultAgent != "" {
		what = fmt.Sprintf("agents configured (default %s)", cfg.DefaultAgent)
	}
	out = append(out, finding{level: "ok", what: what, detail: strings.Join(names, ", ")})

	if cfg.DefaultAgent != "" {
		if _, ok := cfg.Agents[cfg.DefaultAgent]; !ok {
			out = append(out, finding{
				level:  "fail",
				what:   "the default agent is not configured",
				detail: fmt.Sprintf("default_agent = %q, which no [agents.*] section defines", cfg.DefaultAgent),
				fix:    "set default_agent to one of: " + strings.Join(names, ", "),
			})
		}
	}

	return out
}

// checkWorkspace reports where an agent would work.
//
// It is the root an agent can read and write, so it is worth saying out loud —
// including when it is the default, because a user who expects their repository
// wants to know the agent is somewhere else. The check exists because the answer
// used to be the daemon's own directory, which for a service is the filesystem
// root.
func checkWorkspace(cfg config.Config) []finding {
	dir, err := cfg.EffectiveWorkspaceDir()
	if err != nil {
		return []finding{{
			level:  "fail",
			what:   "the workspace could not be resolved",
			detail: err.Error(),
			fix:    "set workspace_dir to an absolute path you own",
		}}
	}

	if filepath.Clean(dir) == string(filepath.Separator) {
		return []finding{{
			level:  "fail",
			what:   "the workspace is the filesystem root",
			detail: "/ is every file you can reach, not a project.",
			fix:    "set workspace_dir to the project directory the agent should work in",
		}}
	}

	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return []finding{{
			level:  "fail",
			what:   "the workspace is not a directory",
			detail: dir,
			fix:    "create it, or set workspace_dir to one that exists",
		}}
	}

	if cfg.WorkspaceDir == "" {
		return []finding{{
			level:  "ok",
			what:   "workspace is the default",
			detail: dir + " (set workspace_dir to work in a project)",
		}}
	}
	return []finding{{level: "ok", what: "workspace", detail: dir}}
}

// guardedFolders are the user directories macOS protects with TCC.
//
// Reading one is a consent decision the user makes for the whole process tree,
// so an agent that works inside one produces a dialog.
var guardedFolders = []string{"Desktop", "Documents", "Downloads"}

// checkGuardedFolder reports when the workspace is somewhere macOS will prompt
// for.
//
// The prompt is expected rather than a fault: the access is attributed to the
// responsible process, and for a daemon that is Hive. Saying so here is what
// turns a surprising dialog into a known one; the remedy is one grant, or a
// workspace somewhere else.
func checkGuardedFolder(cfg config.Config) []finding {
	if runtime.GOOS != "darwin" {
		return nil
	}

	dir, err := cfg.EffectiveWorkspaceDir()
	if err != nil {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	guarded := append([]string{filepath.Join(home, "Library", "Mobile Documents")}, guardedFolders...)
	for _, name := range guarded {
		folder := name
		if !filepath.IsAbs(folder) {
			folder = filepath.Join(home, name)
		}
		if !within(dir, folder) {
			continue
		}
		return []finding{{
			level: "warn",
			what:  "the workspace is in a folder macOS guards",
			detail: fmt.Sprintf(
				"%s: macOS asks before an agent reads it, and the dialog names Hive because a daemon is the responsible process for everything it launches.",
				dir),
			fix: "grant Hive Full Disk Access once (System Settings > Privacy & Security > Full Disk Access), or set workspace_dir outside Desktop, Documents and Downloads",
		}}
	}
	return nil
}

// within reports whether path is folder itself or inside it.
func within(path, folder string) bool {
	rel, err := filepath.Rel(folder, path)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// checkDataDir reports whether Hive can write where it needs to.
func checkDataDir(cfg config.Config) []finding {
	dir, err := cfg.EffectiveDataDir()
	if err != nil {
		return []finding{{
			level:  "fail",
			what:   "the data directory could not be resolved",
			detail: err.Error(),
			fix:    "set data_dir to a path you own",
		}}
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return []finding{{
			level:  "fail",
			what:   "the data directory could not be created",
			detail: fmt.Sprintf("%s: %v", dir, err),
			fix:    "set data_dir to a path you can write to",
		}}
	}

	probe := filepath.Join(dir, ".doctor")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return []finding{{
			level:  "fail",
			what:   "the data directory is not writable",
			detail: fmt.Sprintf("%s: %v", dir, err),
			fix:    "check the permissions on the directory",
		}}
	}
	_ = os.Remove(probe)

	return []finding{{level: "ok", what: "data directory is writable", detail: dir}}
}

// checkAgents reports whether each configured agent can actually be run.
//
// This is the most common reason a session is created and then does nothing, so it
// is checked by looking for the binary rather than by starting one.
func checkAgents(cfg config.Config) []finding {
	var out []finding

	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		agent := cfg.Agents[name]
		if len(agent.Command) == 0 {
			out = append(out, finding{
				level:  "fail",
				what:   fmt.Sprintf("agent %s has no command", name),
				detail: "a run of this agent cannot start.",
				fix:    fmt.Sprintf("set command in [agents.%s]", name),
			})
			continue
		}

		binary := agent.Command[0]
		path, err := exec.LookPath(binary)
		if err != nil {
			out = append(out, finding{
				level:  "warn",
				what:   fmt.Sprintf("agent %s is not on PATH", name),
				detail: fmt.Sprintf("%q was not found, so a run of this agent will fail to start.", binary),
				fix:    fmt.Sprintf("install %s, or give an absolute path in [agents.%s] command", binary, name),
			})
			continue
		}

		out = append(out, finding{
			level:  "ok",
			what:   fmt.Sprintf("agent %s found", name),
			detail: path + " " + strings.Join(agent.Command[1:], " "),
		})
	}
	return out
}

// checkTransports reports whether each enabled transport can reach its platform.
func checkTransports(cfg config.Config) []finding {
	var out []finding

	names := make([]string, 0, len(cfg.Transports))
	for name := range cfg.Transports {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		transport := cfg.Transports[name]
		if !transport.Enabled {
			continue
		}

		if name == "slack" {
			// A token may be in this shell or in the file the service reads. Only
			// reporting the first would fail a working installation, and a check
			// that cries wolf is worse than no check.
			serviceEnv := readServiceEnv()

			for _, variable := range []string{"SLACK_APP_TOKEN", "SLACK_BOT_TOKEN"} {
				if os.Getenv(variable) != "" || serviceEnv[variable] != "" {
					continue
				}
				out = append(out, finding{
					level:  "fail",
					what:   fmt.Sprintf("the Slack transport has no %s", variable),
					detail: "the transport will exit and no message will be answered.",
					fix:    fmt.Sprintf("export %s, or run `hive service install` from a shell that has it", variable),
				})
			}
			if len(cfg.Security.AllowedUsers) == 0 {
				out = append(out, finding{
					level:  "warn",
					what:   "no Slack user is allowed",
					detail: "authorization denies by default, so every message is refused.",
					fix:    `add the user to security.allowed_users as "slack:<user id>"`,
				})
			}
			out = append(out, finding{level: "ok", what: "the Slack transport is enabled"})
		}
	}
	return out
}

// readServiceEnv reads the secrets the service was installed with.
//
// A check that only looks at this shell would fail an installation that works, so
// it looks where the daemon actually gets them.
func readServiceEnv() map[string]string {
	values := map[string]string{}

	home, err := os.UserHomeDir()
	if err != nil {
		return values
	}

	data, err := os.ReadFile(filepath.Join(home, config.DefaultDirName, "data", serviceEnvFile))
	if err != nil {
		return values
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "export ") {
			continue
		}

		assignment := strings.TrimPrefix(line, "export ")
		name, value, ok := strings.Cut(assignment, "=")
		if !ok {
			continue
		}
		values[name] = strings.Trim(value, "'")
	}
	return values
}

// checkDaemon reports whether a daemon is reachable.
func checkDaemon(cfg config.Config) []finding {
	dir, err := cfg.EffectiveDataDir()
	if err != nil {
		return nil
	}
	socket := filepath.Join(dir, "hive.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := client.Dial(ctx, socket)
	if err != nil {
		return []finding{{
			level:  "warn",
			what:   "no daemon is answering",
			detail: fmt.Sprintf("%s: %v", socket, err),
			fix:    "run `hive serve`, or `hive service install` to run it at login",
		}}
	}
	defer c.Close()

	var out []finding

	nodes, err := c.ListNodes(ctx)
	if err != nil {
		out = append(out, finding{
			level:  "warn",
			what:   "the daemon is running but the node list could not be read",
			detail: err.Error(),
		})
	} else {
		connected := 0
		for _, node := range nodes.Nodes {
			if node.Connected {
				connected++
			}
		}
		if connected == 0 {
			out = append(out, finding{
				level:  "fail",
				what:   "no node is connected",
				detail: "a session can be created but no agent can run.",
				fix:    "check the daemon log; a node that cannot start an agent reports why",
			})
		} else {
			out = append(out, finding{
				level:  "ok",
				what:   fmt.Sprintf("%d node(s) connected", connected),
				detail: fmt.Sprintf("%d configured agent(s) reachable", len(cfg.Agents)),
			})
		}
	}

	// The Control API is what the CLI and the transport use, so a daemon that
	// answers is worth confirming rather than assuming.
	if _, err := c.ListSessions(ctx, 1); err != nil {
		out = append(out, finding{
			level:  "warn",
			what:   "the daemon answered but sessions could not be listed",
			detail: err.Error(),
		})
	}

	return out
}

// checkStore reports whether the durable state can be read.
func checkStore(cfg config.Config) []finding {
	dir, err := cfg.EffectiveDataDir()
	if err != nil {
		return nil
	}
	path := filepath.Join(dir, "coordinator.db")

	if _, err := os.Stat(path); err != nil {
		return []finding{{
			level:  "warn",
			what:   "there is no durable state yet",
			detail: "the database is created the first time the daemon starts.",
		}}
	}

	store, err := storage.Open(context.Background(), path)
	if err != nil {
		return []finding{{
			level:  "fail",
			what:   "the durable state could not be opened",
			detail: fmt.Sprintf("%s: %v", path, err),
			fix:    "check that nothing else holds the file, and that it is readable",
		}}
	}
	defer store.Close()

	version, err := store.MigrationVersion(context.Background())
	if err != nil {
		return []finding{{
			level:  "warn",
			what:   "the schema version could not be read",
			detail: err.Error(),
		}}
	}

	return []finding{{
		level:  "ok",
		what:   "durable state is readable",
		detail: fmt.Sprintf("%s, schema version %d", path, version),
	}}
}

// checkService reports whether the daemon is set to run at login.
func checkService() []finding {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	var path string
	if runtime.GOOS == "darwin" {
		path = filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
	} else {
		path = filepath.Join(home, ".config", "systemd", "user", serviceLabel+".service")
	}

	if _, err := os.Stat(path); err != nil {
		return []finding{{
			level:  "warn",
			what:   "the daemon is not set to run at login",
			detail: "Hive only runs while `hive serve` is in a terminal.",
			fix:    "run `hive service install`",
		}}
	}

	out := []finding{{level: "ok", what: "the daemon is set to run at login", detail: path}}

	// The service reads its secrets from a file, so a file that is missing or
	// world-readable is worth saying out loud.
	envFile := filepath.Join(home, ".hive", "data", serviceEnvFile)
	if info, err := os.Stat(envFile); err == nil {
		if info.Mode().Perm()&0o077 != 0 {
			out = append(out, finding{
				level:  "warn",
				what:   "the service's secrets file is readable by others",
				detail: fmt.Sprintf("%s is %#o", envFile, info.Mode().Perm()),
				fix:    fmt.Sprintf("chmod 600 %s", envFile),
			})
		}
	} else {
		out = append(out, finding{
			level:  "warn",
			what:   "the service has no secrets file",
			detail: "the daemon started by the service will not have its tokens.",
			fix:    "run `hive service install` from a shell that has them",
		})
	}

	return out
}
