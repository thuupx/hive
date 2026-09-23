package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// scanner is satisfied by *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// InsertSession stores a new session. Inserting an existing id is an error;
// use a domain-level operation to change session state.
func (s *Store) InsertSession(ctx context.Context, tx Execer, sess *Session) error {
	if sess.ID == "" {
		return errors.New("storage: session id is required")
	}
	if sess.State == "" {
		return fmt.Errorf("storage: session %s: state is required", sess.ID)
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
		sess.ID, sess.State, sess.WorkspaceID, sess.DefaultInteractiveRunID,
		string(meta), unixNano(created), unixNano(updated))
	if err != nil {
		return fmt.Errorf("storage: insert session %s: %w", sess.ID, err)
	}
	return nil
}

// GetSession returns a session by id, or ErrNotFound.
func (s *Store) GetSession(ctx context.Context, id string) (*Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, state, workspace_id, default_interactive_run_id, metadata, created_at, updated_at
		FROM sessions WHERE id = ?`, id)

	var (
		sess      Session
		workspace sql.NullString
		defRun    sql.NullString
		meta      string
		created   int64
		updated   int64
	)
	err := row.Scan(&sess.ID, &sess.State, &workspace, &defRun, &meta, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("storage: session %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read session %s: %w", id, err)
	}
	sess.WorkspaceID = workspace.String
	sess.DefaultInteractiveRunID = defRun.String
	sess.Metadata = json.RawMessage(meta)
	sess.CreatedAt = fromUnixNano(created)
	sess.UpdatedAt = fromUnixNano(updated)
	return &sess, nil
}
