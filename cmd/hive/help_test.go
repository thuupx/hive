package main

import (
	"strings"
	"testing"
)

// Every command the usage lists must have help, and every command with help must
// be reachable.
//
// A command a user cannot look up is a command they will not find, and help for a
// command that does not exist is worse: it documents something that will fail.
func TestHelpCoversEveryCommandInTheUsage(t *testing.T) {
	listed := commandsInUsage(t)

	for _, name := range listed {
		if _, ok := findHelp(name); !ok && len(helpPrefix(name)) == 0 {
			t.Errorf("%q is in the usage but has no help", name)
		}
	}

	for _, entry := range helpTable {
		root := entry.path
		if i := strings.Index(root, " "); i > 0 {
			root = root[:i]
		}
		if !contains(listed, root) {
			t.Errorf("help documents %q, which the usage does not list", entry.path)
		}
	}
}

// The word `help` is a command, not a flag.
//
// Treating it as a flag made `hive help <command>` print the whole usage and
// nothing else, which is the one thing it exists not to do.
func TestHelpIsACommandNotAFlag(t *testing.T) {
	f, rest, err := parseArgs([]string{"help", "session"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if f.help {
		t.Fatal("the word help should not set the usage flag")
	}
	if len(rest) != 2 || rest[0] != "help" || rest[1] != "session" {
		t.Fatalf("rest = %v, want [help session]", rest)
	}

	// The flags still mean the usage.
	for _, flag := range []string{"-h", "--help"} {
		f, _, err := parseArgs([]string{flag})
		if err != nil {
			t.Fatalf("parseArgs(%q): %v", flag, err)
		}
		if !f.help {
			t.Errorf("%q should ask for the usage", flag)
		}
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
		if !strings.HasPrefix(entry.usage, "hive ") {
			t.Errorf("%q has a usage line that is not an invocation: %q", entry.path, entry.usage)
		}
	}
}

// commandsInUsage reads the command names out of the usage text.
func commandsInUsage(t *testing.T) []string {
	t.Helper()

	start := strings.Index(usage, "Commands:")
	if start < 0 {
		t.Fatal("the usage has no command list")
	}

	var names []string
	for _, line := range strings.Split(usage[start:], "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "Commands:") {
			continue
		}
		if strings.HasPrefix(trimmed, "Global flags:") {
			break
		}

		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		names = append(names, fields[0])
	}
	return names
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
