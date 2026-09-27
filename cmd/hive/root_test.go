package main

import (
	"reflect"
	"testing"
)

// A single-dash long flag is normalised to its long spelling, wherever it
// appears.
//
// Found by running the daemon: the node child is started with `-config`, cobra
// read the config path as the command name, and `hive logs -lines 5` was "unknown
// shorthand flag: 'l' in -lines".
func TestNormalizeFlags(t *testing.T) {
	root := newRootCommand(&flags{})

	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{
			"a global flag before the command",
			[]string{"-config", "/tmp/x.toml", "serve", "-role", "node"},
			[]string{"--config", "/tmp/x.toml", "serve", "--role", "node"},
		},
		{
			"already long",
			[]string{"--config", "/tmp/x.toml", "status"},
			[]string{"--config", "/tmp/x.toml", "status"},
		},
		{
			"a command's own flag",
			[]string{"logs", "-lines", "5", "-level", "warn"},
			[]string{"logs", "--lines", "5", "--level", "warn"},
		},
		{
			"a shorthand is left alone",
			[]string{"-h", "session", "create", "-agent", "devin"},
			[]string{"-h", "session", "create", "--agent", "devin"},
		},
		{
			"an equals form keeps its value attached",
			[]string{"-config=/tmp/x.toml", "doctor"},
			[]string{"--config=/tmp/x.toml", "doctor"},
		},
		{
			// A value is never rewritten, even when it looks like a flag.
			"a value that looks like a flag",
			[]string{"logs", "--grep", "-lines"},
			[]string{"logs", "--grep", "-lines"},
		},
		{
			"a value of a single-dash flag",
			[]string{"logs", "-grep", "-level"},
			[]string{"logs", "--grep", "-level"},
		},
		{
			"everything after -- is positional",
			[]string{"session", "prompt", "sess_1", "--", "-lines"},
			[]string{"session", "prompt", "sess_1", "--", "-lines"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeFlags(root, tc.args); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("normalizeFlags(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// A normalised invocation still resolves the command it names.
func TestNormalizedFlagsResolveTheCommand(t *testing.T) {
	root := newRootCommand(&flags{})

	args := normalizeFlags(root, []string{"-config", "/tmp/x.toml", "session", "list"})
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
