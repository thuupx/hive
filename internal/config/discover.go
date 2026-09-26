package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Source says how an agent was discovered, so a convention is distinguishable
// from a fact.
type Source string

const (
	// SourceAdapter is an ACP adapter binary, which is named for what it is.
	SourceAdapter Source = "adapter"

	// SourceConvention is a tool that speaks ACP as a mode rather than through a
	// dedicated binary. The command is a convention and may need editing.
	SourceConvention Source = "convention"
)

// DiscoveredAgent is an ACP agent found on this machine.
type DiscoveredAgent struct {
	Name    string
	Command []string
	Source  Source
}

// NeedsVerification reports whether the discovered command is a guess.
func (a DiscoveredAgent) NeedsVerification() bool { return a.Source == SourceConvention }

// AdapterSuffix is the naming convention for an ACP adapter binary.
//
// A dedicated adapter is named for what it is, so a PATH scan finds adapters this
// build has never heard of without guessing any invocation.
const AdapterSuffix = "-acp"

// ownAdapterBase is the base name of Hive's own ACP adapter.
//
// "hive-plugin-acp" ends in the adapter suffix, so a PATH scan reads it as an
// agent called "hive-plugin" — and a release install puts that binary on the
// PATH, which means every fresh installation would be offered Hive itself as an
// agent. An agent that launches Hive's own adapter is not an agent.
const ownAdapterBase = "hive-plugin"

// ACPSubcommand is the convention for a tool that speaks ACP as a mode.
const ACPSubcommand = "acp"

// commonTools are tools that may speak ACP through a subcommand.
//
// The list is a starting point, not an authority: it exists so a first run has
// something to offer, and every command it produces is marked for verification
// because Hive cannot know how a given build exposes ACP.
var commonTools = []string{
	"claude", "gemini", "codex", "opencode", "devin", "cursor-agent",
}

// DiscoverAgents finds ACP agents on this machine.
//
// It looks for adapter binaries by their naming convention, then for common tools
// that may expose ACP as a subcommand. A tool it does not know is configured by
// hand, which is why discovery is a convenience rather than a requirement.
func DiscoverAgents() []DiscoveredAgent {
	seen := make(map[string]bool)
	var found []DiscoveredAgent

	add := func(agent DiscoveredAgent) {
		if agent.Name == "" || seen[agent.Name] {
			return
		}
		seen[agent.Name] = true
		found = append(found, agent)
	}

	// An adapter binary is named for what it is, so the scan needs no guess.
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}

			name := entry.Name()
			base, ok := strings.CutSuffix(name, AdapterSuffix)
			if !ok || base == "" || base == ownAdapterBase {
				continue
			}
			if !isExecutable(dir, name) {
				continue
			}

			add(DiscoveredAgent{
				Name:    base,
				Command: []string{name},
				Source:  SourceAdapter,
			})
		}
	}

	// A tool that exposes ACP as a mode is launched with that subcommand.
	for _, tool := range commonTools {
		path, err := exec.LookPath(tool)
		if err != nil {
			continue
		}
		if !isExecutable(filepath.Dir(path), filepath.Base(path)) {
			continue
		}

		add(DiscoveredAgent{
			Name:    tool,
			Command: []string{tool, ACPSubcommand},
			Source:  SourceConvention,
		})
	}

	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	return found
}

func isExecutable(dir, name string) bool {
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}
