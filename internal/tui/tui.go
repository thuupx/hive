// Package tui renders the Hive management plane.
//
// The TUI is the management plane, not the primary chat UI. It is a client of
// Hive APIs: it reads through the Control API and contains no orchestration logic
// of its own.
package tui

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/thupham/hive/internal/client"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// Overview is a management plane snapshot.
type Overview struct {
	At          time.Time
	Nodes       []v1.NodeSummary
	Agents      []v1.AgentSummary
	Sessions    []v1.SessionSummary
	Permissions []v1.PermissionSummary
}

// Gather reads the management plane through the Control API.
func Gather(ctx context.Context, c *client.Client) (*Overview, error) {
	overview := &Overview{At: time.Now().UTC()}

	nodes, err := c.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	overview.Nodes = nodes.Nodes

	agents, err := c.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	overview.Agents = agents.Agents

	sessions, err := c.ListSessions(ctx, 0)
	if err != nil {
		return nil, err
	}
	overview.Sessions = sessions.Sessions

	permissions, err := c.ListPermissions(ctx, v1.PermissionListParams{})
	if err != nil {
		return nil, err
	}
	overview.Permissions = permissions.Permissions

	return overview, nil
}

// Run renders the overview until ctx is cancelled.
//
// A zero interval renders once. The management plane is a dashboard rather than
// an interactive application in v1, so it refreshes rather than owning the
// terminal.
func Run(ctx context.Context, c *client.Client, w io.Writer, interval time.Duration) error {
	for {
		overview, err := Gather(ctx, c)
		if err != nil {
			return err
		}
		Render(w, overview)

		if interval <= 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// Render writes the overview.
func Render(w io.Writer, o *Overview) {
	fmt.Fprintln(w, "Hive")
	fmt.Fprintln(w)

	connected := 0
	for _, node := range o.Nodes {
		if node.Connected {
			connected++
		}
	}
	fmt.Fprintf(w, "%s %d of %d nodes\n", marker(connected > 0), connected, len(o.Nodes))
	fmt.Fprintf(w, "%s %d agents\n", marker(len(o.Agents) > 0), len(o.Agents))
	fmt.Fprintf(w, "%s %d sessions\n", marker(len(o.Sessions) > 0), len(o.Sessions))
	fmt.Fprintf(w, "%s %d pending permissions\n", marker(len(o.Permissions) > 0), len(o.Permissions))
	fmt.Fprintf(w, "\nAs of %s\n", o.At.Format(time.RFC3339))

	if len(o.Permissions) > 0 {
		fmt.Fprintln(w, "\nPending permissions")
		for _, p := range o.Permissions {
			fmt.Fprintf(w, "  %s  session %s  run %s  since %s\n",
				p.PermissionID, shortID(p.SessionID), shortID(p.RunID), p.CreatedAt.Format(time.RFC3339))
		}
	}

	if len(o.Nodes) > 0 {
		fmt.Fprintln(w, "\nNodes")
		for _, n := range o.Nodes {
			state := "offline"
			if n.Connected {
				state = "connected"
			}
			fmt.Fprintf(w, "  %s  %s  generation %d  %d executions  last seen %s\n",
				n.NodeID, state, n.ConnectionGeneration, n.Executions, n.LastSeen.Format(time.RFC3339))
		}
	}
}

// RenderSession writes a session detail view.
func RenderSession(w io.Writer, status *v1.SessionStatusResult) {
	fmt.Fprintf(w, "Session %s\n\n", status.SessionID)
	if status.Workspace != "" {
		fmt.Fprintf(w, "Workspace: %s\n", status.Workspace)
	}
	fmt.Fprintf(w, "State: %s\n", status.State)
	fmt.Fprintf(w, "Default run: %s\n", shortID(status.DefaultRunID))

	fmt.Fprintln(w, "\nRuns:")
	if len(status.Runs) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		for _, run := range status.Runs {
			mark := "○"
			switch run.State {
			case "running", "starting":
				mark = "●"
			case "completed":
				mark = "✓"
			case "failed", "interrupted", "cancelled":
				mark = "✗"
			}
			fmt.Fprintf(w, "  %s %s  agent %s  node %s  state %s  generation %d\n",
				mark, run.RunID, run.AgentID, run.NodeID, run.State, run.ExecutionGeneration)
			if run.RuntimeSessionID != "" {
				fmt.Fprintf(w, "      runtime session %s\n", run.RuntimeSessionID)
			}
		}
	}

	renderStatistics(w, status)
}

// renderStatistics prints what a session runs with and what it has cost.
//
// A session with nothing recorded prints nothing here rather than a line of
// zeroes: zero tool calls and no report are different facts.
func renderStatistics(w io.Writer, status *v1.SessionStatusResult) {
	if len(status.Settings) == 0 && status.ToolCalls == 0 && status.Usage == nil {
		return
	}
	fmt.Fprintln(w, "\nStatistics:")

	if len(status.Settings) > 0 {
		ids := make([]string, 0, len(status.Settings))
		for id := range status.Settings {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Fprintf(w, "  %s: %s\n", id, status.Settings[id])
		}
	}
	if status.ToolCalls > 0 {
		fmt.Fprintf(w, "  tool calls: %d\n", status.ToolCalls)
	}
	if status.Usage != nil {
		if turn := status.Usage.Turn(); turn > 0 {
			fmt.Fprintf(w, "  tokens: %d this turn (in %d, out %d)\n",
				turn, status.Usage.InputTokens, status.Usage.OutputTokens)
		}
		if status.Usage.ContextSize > 0 {
			fmt.Fprintf(w, "  context: %d%% (%d/%d)\n",
				status.Usage.ContextUsed*100/status.Usage.ContextSize,
				status.Usage.ContextUsed, status.Usage.ContextSize)
		}
	}
}

func marker(present bool) string {
	if present {
		return "●"
	}
	return "○"
}

// shortID trims an identifier for display.
func shortID(id string) string {
	if id == "" {
		return "-"
	}
	if len(id) <= 20 {
		return id
	}
	return id[:17] + "..."
}
