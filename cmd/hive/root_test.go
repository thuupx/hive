package main

import (
	"reflect"
	"testing"
)

// A single-dash long flag before the command name is normalised, because cobra
// decides which command is being run before it parses flags.
//
// Found by running the daemon: the node child is started with `-config`, and
// cobra read the config path as the command name.
func TestNormalizeGlobalFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{
			// Both spellings are accepted, wherever they appear.
			[]string{"-config", "/tmp/x.toml", "serve", "-role", "node"},
			[]string{"--config", "/tmp/x.toml", "serve", "--role", "node"},
		},
		{
			[]string{"--config", "/tmp/x.toml", "status"},
			[]string{"--config", "/tmp/x.toml", "status"},
		},
		{
			// A shorthand and a command-local flag are left alone.
			[]string{"-h", "session", "create", "-agent", "devin"},
			[]string{"-h", "session", "create", "-agent", "devin"},
		},
		{
			// An equals form keeps its value attached.
			[]string{"-config=/tmp/x.toml", "doctor"},
			[]string{"--config=/tmp/x.toml", "doctor"},
		},
	} {
		if got := normalizeGlobalFlags(tc.args); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("normalizeGlobalFlags(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// A normalised invocation still resolves the command it names.
func TestNormalizedFlagsResolveTheCommand(t *testing.T) {
	root := newRootCommand(&flags{})

	args := normalizeGlobalFlags([]string{"-config", "/tmp/x.toml", "session", "list"})
	cmd, rest, err := root.Find(args)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if cmd.CommandPath() != "hive session list" {
		t.Fatalf("command = %q, want hive session list", cmd.CommandPath())
	}
	if len(rest) == 0 {
		t.Fatal("the flags should be left for parsing")
	}
}
