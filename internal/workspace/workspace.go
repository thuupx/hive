// Package workspace models Hive workspace identity and node-local locations.
//
// A workspace is not a Git abstraction. Hive does not own git checkout, branch,
// worktree, merge, or rebase; agents may use those mechanisms themselves. Hive
// may detect that several active runs reference the same location and expose a
// warning, but it does not lock or orchestrate anything.
package workspace

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Workspace is a named project identity.
type Workspace struct {
	ID        string
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// New creates a workspace.
func New(id, name string) *Workspace {
	now := time.Now().UTC()
	return &Workspace{ID: id, Name: name, CreatedAt: now, UpdatedAt: now}
}

// Validate reports whether the workspace is well formed enough to persist.
func (w *Workspace) Validate() error {
	switch {
	case w == nil:
		return errors.New("workspace: nil workspace")
	case w.ID == "":
		return errors.New("workspace: id is required")
	case w.Name == "":
		return fmt.Errorf("workspace: %s: name is required", w.ID)
	}
	return nil
}

// Location is where a workspace lives on a node.
//
// The same workspace has one location per node, and the paths may differ.
type Location struct {
	WorkspaceID string
	NodeID      string
	Path        string
}

// Usage is a workspace location and the runs that reference it.
type Usage struct {
	WorkspaceID string
	NodeID      string
	Path        string

	// RunIDs are the runs that reference this location, in a stable order.
	RunIDs []string
}

// Shared reports whether more than one run references the location.
func (u Usage) Shared() bool { return len(u.RunIDs) > 1 }

// Warning describes a shared location.
//
// It is a warning, never a lock: concurrent runs on one location are a real
// workflow, and Hive's job is to make the overlap visible rather than to prevent
// it.
type Warning struct {
	WorkspaceID string
	NodeID      string
	Path        string
	RunIDs      []string
}

// Message renders the warning for a human.
func (w Warning) Message() string {
	return fmt.Sprintf(
		"%d active runs share the workspace location %s on %s: %v",
		len(w.RunIDs), w.Path, w.NodeID, w.RunIDs)
}

// Warnings returns one warning per shared location, in a stable order.
func Warnings(usages []Usage) []Warning {
	var out []Warning

	for _, usage := range usages {
		if !usage.Shared() {
			continue
		}
		runIDs := append([]string(nil), usage.RunIDs...)
		sort.Strings(runIDs)

		out = append(out, Warning{
			WorkspaceID: usage.WorkspaceID,
			NodeID:      usage.NodeID,
			Path:        usage.Path,
			RunIDs:      runIDs,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].WorkspaceID != out[j].WorkspaceID {
			return out[i].WorkspaceID < out[j].WorkspaceID
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out
}
