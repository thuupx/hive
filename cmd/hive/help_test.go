package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// allCommands walks the tree, deepest last.
func allCommands(root *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, cmd := range root.Commands() {
		out = append(out, cmd)
		out = append(out, allCommands(cmd)...)
	}
	return out
}

// findHelpEntry is the curated prose for a command path.
func findHelpEntry(path string) (commandHelp, bool) {
	for _, entry := range helpTable {
		if entry.path == path {
			return entry, true
		}
	}
	return commandHelp{}, false
}

// Every command has curated help, and every help entry names a real command.
//
// A command a user cannot look up is a command they will not find, and help for a
// command that does not exist documents something that will fail.
func TestHelpCoversEveryCommand(t *testing.T) {
	root := newRootCommand(&flags{})

	// fang adds `man` and cobra adds `completion` at execution time; neither is
	// a command a user reads this table for.
	generated := map[string]bool{"man": true, "completion": true}

	for _, cmd := range allCommands(root) {
		if generated[cmd.Name()] {
			continue
		}
		// CommandPath is "hive session create"; the table names it "session create".
		path := strings.TrimPrefix(cmd.CommandPath(), root.Name()+" ")
		if _, ok := findHelpEntry(path); !ok {
			t.Errorf("%q is a command but has no help entry", cmd.CommandPath())
		}
	}

	for _, entry := range helpTable {
		if _, ok := findCommand(root, entry.path); !ok {
			t.Errorf("help documents %q, which is not a command", entry.path)
		}
	}
}

// The word `help` is a command, not a flag.
//
// Treating it as a flag made `hive help <command>` print the whole usage and
// nothing else, which is the one thing it exists not to do.
func TestHelpIsACommandNotAFlag(t *testing.T) {
	root := newRootCommand(&flags{})

	help, ok := findCommand(root, "help")
	if !ok {
		t.Fatal("the tree has no help command")
	}
	if help.Name() != "help" {
		t.Fatalf("help command = %q", help.Name())
	}

	// --help is still a flag on every command, which is what makes
	// `hive session create --help` work. Cobra adds it when it is needed, so it
	// is asked for the same way.
	session, ok := findCommand(root, "session create")
	if !ok {
		t.Fatal("the tree has no session create command")
	}
	session.InitDefaultHelpFlag()
	if session.Flags().Lookup("help") == nil {
		t.Error("a command should still answer --help")
	}
}

// A command's help says what it is for, not only how it is invoked.
func TestEveryHelpEntrySaysWhatTheCommandIsFor(t *testing.T) {
	for _, entry := range helpTable {
		if strings.TrimSpace(entry.summary) == "" {
			t.Errorf("%q has no summary", entry.path)
		}
		if strings.TrimSpace(entry.usage) == "" {
			t.Errorf("%q has no usage line", entry.path)
		}
		if !strings.HasPrefix(entry.usage, "hive "+entry.path) {
			t.Errorf("%q has a usage line that does not match its path: %q", entry.path, entry.usage)
		}
	}
}

// A flag the help names must exist, or the help is describing a command that
// cannot be invoked.
func TestDocumentedFlagsExist(t *testing.T) {
	root := newRootCommand(&flags{})

	for _, entry := range helpTable {
		cmd, ok := findCommand(root, entry.path)
		if !ok {
			continue
		}
		for _, documented := range entry.flags {
			name := strings.TrimLeft(documented.flag, "-")
			switch {
			case cmd.Flags().Lookup(name) != nil:
			case cmd.PersistentFlags().Lookup(name) != nil:
			// A global flag is inherited, and `serve` documents -role because a
			// node child is started with it.
			case cmd.InheritedFlags().Lookup(name) != nil:
			default:
				t.Errorf("%s documents %q, which the command does not define", entry.path, documented.flag)
			}
		}
	}
}

// The global flags are persistent, so a command may name them after itself.
//
// This is what the old parseArgsAndFlags existed to guarantee: a caller must not
// have to remember flag order.
func TestGlobalFlagsArePersistent(t *testing.T) {
	root := newRootCommand(&flags{})

	for _, name := range []string{"config", "role", "coordinator-url", "node-id"} {
		if root.PersistentFlags().Lookup(name) == nil {
			t.Errorf("the root does not define a persistent --%s", name)
		}
	}
}
