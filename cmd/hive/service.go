package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/thupham/hive/internal/config"
)

// The service label is how the supervisor knows the daemon. It is reverse-DNS so
// it cannot collide with something else in the same namespace.
const serviceLabel = "ai.hive.daemon"

// serviceEnvFile is where the secrets the daemon needs are kept.
//
// They are not put in the service definition: on macOS the definition can be
// printed by `launchctl print`, so a token there is readable by anything that can
// talk to the supervisor. A file the user owns and only the user can read is
// narrower.
const serviceEnvFile = "service.env"

// serviceEnvMode is read and written by the owner only.
const serviceEnvMode = 0o600

// runService manages the background service.
func runService(f flags, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: hive service <install|uninstall|status>")
	}

	switch args[0] {
	case "install":
		return installService(f)
	case "uninstall":
		return uninstallService(f)
	case "status":
		return serviceStatus()
	default:
		return fmt.Errorf("unknown service command %q", args[0])
	}
}

// installService registers the daemon to start at login.
func installService(f flags) error {
	cfg, err := loadConfig(f)
	if err != nil {
		return err
	}

	dir, err := cfg.EffectiveDataDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	// The secrets are collected from the environment the user is installing from,
	// which is the only place they exist: Hive never stores them in its config.
	secrets, missing := serviceSecrets(cfg)
	if len(secrets) > 0 {
		if err := writeServiceEnv(dir, secrets); err != nil {
			return err
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr,
			"hive: these variables are not set in this shell, so the service will not have them: %s\n",
			strings.Join(missing, ", "))
	}
	fmt.Printf("hive: the service will inherit PATH from this shell\n")

	binary, err := installBinary(dir)
	if err != nil {
		return err
	}

	switch runtime.GOOS {
	case "darwin":
		err = installLaunchAgent(binary, dir)
	case "linux":
		err = installSystemdUnit(binary, dir)
	default:
		return fmt.Errorf("hive: %s is not supported for service install", runtime.GOOS)
	}
	if err != nil {
		return err
	}

	fmt.Printf("hive: the daemon will start at login\n  binary: %s\n  data:   %s\n", binary, dir)
	if len(secrets) > 0 {
		fmt.Printf("  secrets: %s (%#o)\n", filepath.Join(dir, serviceEnvFile), serviceEnvMode)
	}
	fmt.Println("\nStart it now with: hive service status")
	return nil
}

// installBinary puts a copy of the daemon somewhere the service can run it.
//
// Two reasons, and both matter. A service must not depend on a source checkout
// that can move or be deleted, and on macOS a binary under a protected directory
// such as Documents cannot be executed by a background agent at all: the attempt
// is blocked without a visible prompt, which looks exactly like a daemon that
// starts and does nothing.
func installBinary(dir string) (string, error) {
	current, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(current); err == nil {
		current = resolved
	}

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", err
	}

	// The plugins are found next to the daemon, so they travel with it.
	entries, err := os.ReadDir(filepath.Dir(current))
	if err != nil {
		return "", err
	}

	copied := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name != "hive" && !strings.HasPrefix(name, "hive-plugin-") {
			continue
		}
		if err := copyExecutable(filepath.Join(filepath.Dir(current), name),
			filepath.Join(binDir, name)); err != nil {
			return "", err
		}
		copied++
	}
	if copied == 0 {
		return "", fmt.Errorf("hive: no binaries found next to %s", current)
	}

	return filepath.Join(binDir, "hive"), nil
}

// copyExecutable copies a binary, writing it aside and renaming so a running
// daemon is never replaced mid-read.
func copyExecutable(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()

	temp := target + ".new"
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(temp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(temp)
		return err
	}
	return os.Rename(temp, target)
}

// serviceSecrets collects the environment variables the configuration needs.
//
// Only what the config actually references is captured, so installing the service
// does not quietly copy every secret in the shell into a file.
func serviceSecrets(cfg config.Config) (map[string]string, []string) {
	wanted := map[string]bool{}

	for name, transport := range cfg.Transports {
		if !transport.Enabled {
			continue
		}
		if name == "slack" {
			wanted["SLACK_APP_TOKEN"] = true
			wanted["SLACK_BOT_TOKEN"] = true
		}
	}
	for _, agent := range cfg.Agents {
		if agent.APIKeyEnv != "" {
			wanted[agent.APIKeyEnv] = true
		}
	}

	found := map[string]string{}
	var missing []string
	for name := range wanted {
		value := os.Getenv(name)
		if value == "" {
			missing = append(missing, name)
			continue
		}
		found[name] = value
	}

	// The agents live on the user's PATH, and a background service does not
	// inherit a login shell's. Without this the daemon starts, listens, and cannot
	// run any agent, which looks like a working daemon that answers nothing.
	if path := os.Getenv("PATH"); path != "" {
		found["PATH"] = path
	}

	sort.Strings(missing)
	return found, missing
}

// writeServiceEnv writes the environment file the service sources.
func writeServiceEnv(dir string, secrets map[string]string) error {
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# Written by `hive service install`. Read by the service at login.\n")
	b.WriteString("# It holds credentials, so it is readable by its owner only.\n")

	for _, name := range names {
		// Single quotes: a token is not a shell expression, and it must survive
		// being sourced.
		fmt.Fprintf(&b, "export %s='%s'\n", name, strings.ReplaceAll(secrets[name], "'", `'\''`))
	}

	path := filepath.Join(dir, serviceEnvFile)
	if err := os.WriteFile(path, []byte(b.String()), serviceEnvMode); err != nil {
		return err
	}
	// WriteFile only applies the mode when it creates the file, so it is set again.
	return os.Chmod(path, serviceEnvMode)
}

// installLaunchAgent writes the macOS LaunchAgent.
func installLaunchAgent(binary, dir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	logDir := filepath.Join(dir, "log")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}

	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		return err
	}

	// The shell sources the secrets and then replaces itself with the daemon, so
	// the supervisor tracks one process.
	script := fmt.Sprintf(". %s && exec %s serve",
		shellQuote(filepath.Join(dir, serviceEnvFile)), shellQuote(binary))

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>

  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>-c</string>
    <string>%s</string>
  </array>

  <key>RunAtLoad</key>
  <true/>

  <!-- A daemon that stops is a daemon that is not listening. -->
  <key>KeepAlive</key>
  <true/>

  <key>ProcessType</key>
  <string>Background</string>

  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, serviceLabel, xmlEscape(script),
		xmlEscape(filepath.Join(logDir, "hive.log")),
		xmlEscape(filepath.Join(logDir, "hive.err.log")))

	path := filepath.Join(agents, serviceLabel+".plist")
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return err
	}

	// Reload so the change takes effect without a logout.
	domain := launchDomain()
	_ = exec.Command("launchctl", "bootout", domain+"/"+serviceLabel).Run()

	if out, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		// Installing over a service that is already loaded is a normal thing to do:
		// it is how a new build is picked up. bootstrap refuses that, so the loaded
		// service is restarted instead, which is what the user meant.
		if _, loaded := exec.Command("launchctl", "print", domain+"/"+serviceLabel).Output(); loaded == nil {
			if out, err := exec.Command("launchctl", "kickstart", "-k", domain+"/"+serviceLabel).CombinedOutput(); err != nil {
				return fmt.Errorf("hive: launchctl kickstart: %s: %w", strings.TrimSpace(string(out)), err)
			}
			return nil
		}
		return fmt.Errorf("hive: launchctl bootstrap: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// launchDomain is the user's launchd domain.
func launchDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

// installSystemdUnit writes a user systemd unit.
func installSystemdUnit(binary, dir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	units := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(units, 0o755); err != nil {
		return err
	}

	script := fmt.Sprintf(". %s && exec %s serve",
		shellQuote(filepath.Join(dir, serviceEnvFile)), shellQuote(binary))

	unit := fmt.Sprintf(`[Unit]
Description=Hive personal agent gateway
After=network-online.target

[Service]
Type=simple
ExecStart=/bin/sh -c %s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, shellQuote(script))

	path := filepath.Join(units, serviceLabel+".service")
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return err
	}

	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", serviceLabel+".service").CombinedOutput(); err != nil {
		return fmt.Errorf("hive: systemctl enable: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// uninstallService removes the service and the secrets it used.
func uninstallService(f flags) error {
	cfg, err := loadConfig(f)
	if err != nil {
		return err
	}
	dir, err := cfg.EffectiveDataDir()
	if err != nil {
		return err
	}

	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		_ = exec.Command("launchctl", "bootout", launchDomain()+"/"+serviceLabel).Run()
		if err := os.Remove(filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")); err != nil && !os.IsNotExist(err) {
			return err
		}
	case "linux":
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		_ = exec.Command("systemctl", "--user", "disable", "--now", serviceLabel+".service").Run()
		if err := os.Remove(filepath.Join(home, ".config", "systemd", "user", serviceLabel+".service")); err != nil && !os.IsNotExist(err) {
			return err
		}
	default:
		return fmt.Errorf("hive: %s is not supported for service uninstall", runtime.GOOS)
	}

	// The secrets exist for the service, so they go with it.
	if err := os.Remove(filepath.Join(dir, serviceEnvFile)); err != nil && !os.IsNotExist(err) {
		return err
	}

	fmt.Println("hive: the service is removed")
	return nil
}

// serviceStatus reports whether the service is running.
func serviceStatus() error {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("launchctl", "print", launchDomain()+"/"+serviceLabel).CombinedOutput()
		if err != nil {
			fmt.Println("hive: the service is not loaded")
			return nil
		}
		for _, line := range strings.Split(string(out), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "state =") || strings.HasPrefix(trimmed, "pid =") {
				fmt.Println(" ", trimmed)
			}
		}
		return nil

	case "linux":
		out, err := exec.Command("systemctl", "--user", "is-active", serviceLabel+".service").CombinedOutput()
		fmt.Println(" ", strings.TrimSpace(string(out)))
		if err != nil {
			return nil
		}
		return nil

	default:
		return fmt.Errorf("hive: %s is not supported for service status", runtime.GOOS)
	}
}

// shellQuote makes a path safe to embed in a shell command.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// xmlEscape makes a string safe to embed in a plist value.
func xmlEscape(value string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return replacer.Replace(value)
}
