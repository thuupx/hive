package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/thupham/hive/internal/session"
)

// InsertSession stores a new session. Inserting an existing id is an error;
// changing session state is a domain operation followed by UpdateSession.
func (s *Store) InsertSession(ctx context.Context, tx Execer, sess *session.Session) error {
	if err := sess.Validate(); err != nil {
		return err
	}
	meta := sess.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage("{}")
	}
	created := sess.CreatedAt
	if created.IsZero() {
		created = now()
	}
	updated := sess.UpdatedAt
	if updated.IsZero() {
		updated = created
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO sessions
			(id, state, workspace_id, default_interactive_run_id, metadata, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, string(sess.State), sess.WorkspaceID, sess.DefaultInteractiveRunID,
		string(meta), unixNano(created), unixNano(updated))
	if err != nil {
		return fmt.Errorf("storage: insert session %s: %w", sess.ID, err)
	}

	sess.Metadata = meta
	sess.CreatedAt = created
	sess.UpdatedAt = updated
	return nil
}

// UpdateSession persists the mutable fields of an existing session.
//
// It writes the whole row, so a caller that may be racing another writer must
// load the session inside the same transaction. Prefer UpdateSessionWith.
func (s *Store) UpdateSession(ctx context.Context, tx Execer, sess *session.Session) error {
	return s.updateSession(ctx, tx, sess)
}

// UpdateSessionWith loads a session inside a write transaction, applies fn, and
// persists the result.
//
// Read-modify-write must happen inside one transaction: the routing pointer and
// the session state are written by the control plane and by handoff activation,
// so persisting a stale in-memory copy would silently overwrite a newer one.
func (s *Store) UpdateSessionWith(ctx context.Context, sessionID string, fn func(sess *session.Session) error) (*session.Session, error) {
	var updated *session.Session

	err := s.WriteTx(ctx, func(tx Execer) error {
		current, err := s.getSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if err := fn(current); err != nil {
			return err
		}
		if err := s.updateSession(ctx, tx, current); err != nil {
			return err
		}
		updated = current
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// GetSession returns a session by id, or ErrNotFound.
func (s *Store) GetSession(ctx context.Context, id string) (*session.Session, error) {
	return s.getSession(ctx, s.db, id)
}

func (s *Store) getSession(ctx context.Context, q Execer, id string) (*session.Session, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, state, workspace_id, default_interactive_run_id, agent_config, metadata, created_at, updated_at
		FROM sessions WHERE id = ?`, id)

	var (
		sess      session.Session
		state     string
		workspace sql.NullString
		defRun    sql.NullString
		agentCfg  string
		meta      string
		created   int64
		updated   int64
	)
	err := row.Scan(&sess.ID, &state, &workspace, &defRun, &agentCfg, &meta, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("storage: session %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read session %s: %w", id, err)
	}

	sess.State = session.State(state)
	sess.WorkspaceID = workspace.String
	sess.DefaultInteractiveRunID = defRun.String
	sess.AgentConfig = decodeAgentConfig(agentCfg)
	sess.Metadata = json.RawMessage(meta)
	sess.CreatedAt = fromUnixNano(created)
	sess.UpdatedAt = fromUnixNano(updated)
	return &sess, nil
}

// decodeAgentConfig reads a stored selection map.
//
// An unreadable value is treated as empty rather than failing a read: a session
// must stay usable even if a selection was written by a newer build.
func decodeAgentConfig(raw string) map[string]string {
	config := map[string]string{}
	if raw == "" {
		return config
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return map[string]string{}
	}

	// An empty value is not a choice. A session recorded by an older build can
	// carry one, and applying it would fail every later run.
	for configID, value := range config {
		if strings.TrimSpace(value) == "" {
			delete(config, configID)
		}
	}
	return config
}

func (s *Store) updateSession(ctx context.Context, tx Execer, sess *session.Session) error {
	if err := sess.Validate(); err != nil {
		return err
	}
	meta := sess.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage("{}")
	}
	updated := now()

	agentConfig, err := json.Marshal(sess.AgentConfig)
	if err != nil {
		return fmt.Errorf("storage: session %s: encode agent config: %w", sess.ID, err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET state = ?, workspace_id = ?, default_interactive_run_id = ?, agent_config = ?, metadata = ?, updated_at = ?
		WHERE id = ?`,
		string(sess.State), sess.WorkspaceID, sess.DefaultInteractiveRunID,
		string(agentConfig), string(meta), unixNano(updated), sess.ID)
	if err != nil {
		return fmt.Errorf("storage: update session %s: %w", sess.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: update session %s: %w", sess.ID, err)
	}
	if affected == 0 {
		return fmt.Errorf("storage: session %s: %w", sess.ID, ErrNotFound)
	}

	sess.Metadata = meta
	sess.UpdatedAt = updated
	return nil
}

// ListSessions returns sessions, most recently updated first.
//
// A limit of zero or less returns every session.
func (s *Store) ListSessions(ctx context.Context, limit int) ([]*session.Session, error) {
	query := `
		SELECT id, state, workspace_id, default_interactive_run_id, agent_config, metadata, created_at, updated_at
		FROM sessions
		ORDER BY updated_at DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: list sessions: %w", err)
	}
	defer rows.Close()

	var out []*session.Session
	for rows.Next() {
		var (
			sess      session.Session
			state     string
			workspace sql.NullString
			defRun    sql.NullString
			agentCfg  string
			meta      string
			created   int64
			updated   int64
		)
		if err := rows.Scan(&sess.ID, &state, &workspace, &defRun, &agentCfg, &meta, &created, &updated); err != nil {
			return nil, fmt.Errorf("storage: scan session: %w", err)
		}
		sess.State = session.State(state)
		sess.WorkspaceID = workspace.String
		sess.DefaultInteractiveRunID = defRun.String
		sess.AgentConfig = decodeAgentConfig(agentCfg)
		sess.Metadata = json.RawMessage(meta)
		sess.CreatedAt = fromUnixNano(created)
		sess.UpdatedAt = fromUnixNano(updated)
		out = append(out, &sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list sessions: %w", err)
	}
	return out, nil
}
