package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/thupham/hive/internal/agent"
	"github.com/thupham/hive/internal/workspace"
)

// InsertWorkspace stores a new workspace.
func (s *Store) InsertWorkspace(ctx context.Context, tx Execer, w *workspace.Workspace) error {
	if err := w.Validate(); err != nil {
		return err
	}

	created := w.CreatedAt
	if created.IsZero() {
		created = now()
	}
	updated := w.UpdatedAt
	if updated.IsZero() {
		updated = created
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, created_at, updated_at)
		VALUES (?, ?, ?, ?)`,
		w.ID, w.Name, unixNano(created), unixNano(updated))
	if err != nil {
		return fmt.Errorf("storage: insert workspace %s: %w", w.ID, err)
	}

	w.CreatedAt = created
	w.UpdatedAt = updated
	return nil
}

// GetWorkspaceByName returns a workspace by name, or ErrNotFound.
func (s *Store) GetWorkspaceByName(ctx context.Context, name string) (*workspace.Workspace, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, created_at, updated_at FROM workspaces WHERE name = ?`, name)

	var (
		w       workspace.Workspace
		created int64
		updated int64
	)
	err := row.Scan(&w.ID, &w.Name, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("storage: workspace %s: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read workspace %s: %w", name, err)
	}

	w.CreatedAt = fromUnixNano(created)
	w.UpdatedAt = fromUnixNano(updated)
	return &w, nil
}

// ListWorkspaces returns every workspace, by name.
func (s *Store) ListWorkspaces(ctx context.Context) ([]*workspace.Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, created_at, updated_at FROM workspaces ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("storage: list workspaces: %w", err)
	}
	defer rows.Close()

	var out []*workspace.Workspace
	for rows.Next() {
		var (
			w       workspace.Workspace
			created int64
			updated int64
		)
		if err := rows.Scan(&w.ID, &w.Name, &created, &updated); err != nil {
			return nil, fmt.Errorf("storage: scan workspace: %w", err)
		}
		w.CreatedAt = fromUnixNano(created)
		w.UpdatedAt = fromUnixNano(updated)
		out = append(out, &w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list workspaces: %w", err)
	}
	return out, nil
}

// PutWorkspaceLocation records where a workspace lives on a node.
func (s *Store) PutWorkspaceLocation(ctx context.Context, tx Execer, loc workspace.Location) error {
	switch {
	case loc.WorkspaceID == "":
		return errors.New("storage: workspace location workspace id is required")
	case loc.NodeID == "":
		return errors.New("storage: workspace location node id is required")
	case loc.Path == "":
		return errors.New("storage: workspace location path is required")
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO workspace_locations (workspace_id, node_id, path)
		VALUES (?, ?, ?)
		ON CONFLICT (workspace_id, node_id) DO UPDATE SET path = excluded.path`,
		loc.WorkspaceID, loc.NodeID, loc.Path)
	if err != nil {
		return fmt.Errorf("storage: put workspace location %s/%s: %w", loc.WorkspaceID, loc.NodeID, err)
	}
	return nil
}

// ListWorkspaceLocations returns the locations of a workspace.
func (s *Store) ListWorkspaceLocations(ctx context.Context, workspaceID string) ([]workspace.Location, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT workspace_id, node_id, path
		FROM workspace_locations
		WHERE workspace_id = ?
		ORDER BY node_id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("storage: list workspace locations: %w", err)
	}
	defer rows.Close()

	var out []workspace.Location
	for rows.Next() {
		var loc workspace.Location
		if err := rows.Scan(&loc.WorkspaceID, &loc.NodeID, &loc.Path); err != nil {
			return nil, fmt.Errorf("storage: scan workspace location: %w", err)
		}
		out = append(out, loc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list workspace locations: %w", err)
	}
	return out, nil
}

// WorkspaceUsage groups the *active* runs of a workspace by location.
//
// A finished run does not overlap with anything, so only runs that are still live
// are reported. The terminal predicate comes from the domain rather than being
// restated in SQL.
func (s *Store) WorkspaceUsage(ctx context.Context) ([]workspace.Usage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT wl.workspace_id, wl.node_id, wl.path, ar.id, ar.session_id, ar.agent_id, ar.node_id,
		       ar.protocol, ar.runtime_session_id, ar.execution_generation, ar.node_execution_id,
		       ar.state, ar.started_at, ar.ended_at
		FROM workspace_locations wl
		JOIN sessions s ON s.workspace_id = wl.workspace_id
		JOIN agent_runs ar ON ar.session_id = s.id AND ar.node_id = wl.node_id
		ORDER BY wl.workspace_id, wl.node_id, ar.rowid`)
	if err != nil {
		return nil, fmt.Errorf("storage: read workspace usage: %w", err)
	}
	defer rows.Close()

	type key struct {
		workspaceID string
		nodeID      string
	}

	var (
		order []key
		byKey = map[key]*workspace.Usage{}
		byRun = map[key][]*agent.AgentRun{}
	)

	for rows.Next() {
		var (
			loc       workspace.Location
			run       agent.AgentRun
			sessionID string
			nodeID    sql.NullString
			runtimeID sql.NullString
			nodeExec  sql.NullString
			state     string
			started   sql.NullInt64
			ended     sql.NullInt64
		)
		if err := rows.Scan(&loc.WorkspaceID, &loc.NodeID, &loc.Path,
			&run.ID, &sessionID, &run.AgentID, &nodeID, &run.Protocol, &runtimeID,
			&run.ExecutionGeneration, &nodeExec, &state, &started, &ended); err != nil {
			return nil, fmt.Errorf("storage: scan workspace usage: %w", err)
		}

		run.SessionID = sessionID
		run.NodeID = nodeID.String
		run.RuntimeSessionID = runtimeID.String
		run.NodeExecutionID = nodeExec.String
		run.State = agent.State(state)
		run.StartedAt = timeFromNull(started)
		run.EndedAt = timeFromNull(ended)

		if run.IsTerminal() {
			continue
		}

		k := key{workspaceID: loc.WorkspaceID, nodeID: loc.NodeID}
		if _, seen := byKey[k]; !seen {
			byKey[k] = &workspace.Usage{
				WorkspaceID: loc.WorkspaceID,
				NodeID:      loc.NodeID,
				Path:        loc.Path,
			}
			order = append(order, k)
		}
		byRun[k] = append(byRun[k], &run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: read workspace usage: %w", err)
	}

	out := make([]workspace.Usage, 0, len(order))
	for _, k := range order {
		usage := byKey[k]
		for _, run := range byRun[k] {
			usage.RunIDs = append(usage.RunIDs, run.ID)
		}
		out = append(out, *usage)
	}
	return out, nil
}
