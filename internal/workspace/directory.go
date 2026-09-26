package workspace

import "errors"

// ErrNoWorkspace reports that a run has no directory to work in.
var ErrNoWorkspace = errors.New(
	"no workspace: set workspace_dir in the configuration, or create a workspace and name it on the session")

// Directory resolves the directory a run works in.
//
// A run must have one: an agent that writes files needs to know where, and ACP
// requires a cwd to create a session. A location the coordinator named wins, then
// the configured workspace.
//
// It never falls back to the process's own directory. A daemon started by the
// system runs in the filesystem root, and an agent told to work there can read
// and write everything its user can — that is not a working directory, it is the
// whole machine. Refusing is the honest answer, and the error says what to fix.
func Directory(requested, configured string) (string, error) {
	switch {
	case requested != "":
		return requested, nil
	case configured != "":
		return configured, nil
	default:
		return "", ErrNoWorkspace
	}
}
