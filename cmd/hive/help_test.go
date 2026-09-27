package main

import (
	"reflect"
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

	// fang adds `man`, and cobra adds `completion` and its shells; none of them is
	// a command a user reads this table for.
	generated := []string{"man", "completion"}
	isGenerated := func(path string) bool {
		for _, prefix := range generated {
			if path == prefix || strings.HasPrefix(path, prefix+" ") {
				return true
			}
		}
		return false
	}

	for _, cmd := range allCommands(root) {
		// CommandPath is "hive session create"; the table names it "session create".
		path := strings.TrimPrefix(cmd.CommandPath(), root.Name()+" ")
		if isGenerated(path) {
			continue
		}
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

// Every command is in a section, and every section names a real command.
//
// An ungrouped command is listed under a heading of its own at the top of the
// help, which is where a new command silently ends up.
func TestEveryCommandIsGrouped(t *testing.T) {
	root := newRootCommand(&flags{})

	grouped := map[string]bool{}
	for _, group := range commandGroups {
		for _, path := range group.commands {
			cmd, ok := findCommand(root, path)
			if !ok {
				t.Errorf("the %s section names %q, which is not a command", group.title, path)
				continue
			}
			if cmd.GroupID != group.id {
				t.Errorf("%q is in group %q, want %q", path, cmd.GroupID, group.id)
			}
			grouped[path] = true
		}
	}

	for _, cmd := range root.Commands() {
		if cmd.Hidden {
			continue
		}
		path := strings.TrimPrefix(cmd.CommandPath(), root.Name()+" ")
		if !grouped[path] {
			t.Errorf("%q has no section, so the help lists it on its own", path)
		}
	}
}

// The help lists the commands in the order the sections declare them.
//
// Cobra sorts alphabetically by default, which put "config, doctor, init" under
// Setup — not the order anyone does them in.
func TestHelpOrderIsTheDeclaredOrder(t *testing.T) {
	root := newRootCommand(&flags{})

	var want []string
	for _, group := range commandGroups {
		want = append(want, group.commands...)
	}

	var got []string
	for _, cmd := range root.Commands() {
		if cmd.Hidden {
			continue
		}
		got = append(got, strings.TrimPrefix(cmd.CommandPath(), root.Name()+" "))
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("the help lists commands as\n  %v\nwant\n  %v", got, want)
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
