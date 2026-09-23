package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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
func (s *Store) UpdateSession(ctx context.Context, tx Execer, sess *session.Session) error {
	if err := sess.Validate(); err != nil {
		return err
	}
	meta := sess.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage("{}")
	}
	updated := now()

	res, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET state = ?, workspace_id = ?, default_interactive_run_id = ?, metadata = ?, updated_at = ?
		WHERE id = ?`,
		string(sess.State), sess.WorkspaceID, sess.DefaultInteractiveRunID,
		string(meta), unixNano(updated), sess.ID)
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

// GetSession returns a session by id, or ErrNotFound.
func (s *Store) GetSession(ctx context.Context, id string) (*session.Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, state, workspace_id, default_interactive_run_id, metadata, created_at, updated_at
		FROM sessions WHERE id = ?`, id)

	var (
		sess      session.Session
		state     string
		workspace sql.NullString
		defRun    sql.NullString
		meta      string
		created   int64
		updated   int64
	)
	err := row.Scan(&sess.ID, &state, &workspace, &defRun, &meta, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("storage: session %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read session %s: %w", id, err)
	}

	sess.State = session.State(state)
	sess.WorkspaceID = workspace.String
	sess.DefaultInteractiveRunID = defRun.String
	sess.Metadata = json.RawMessage(meta)
	sess.CreatedAt = fromUnixNano(created)
	sess.UpdatedAt = fromUnixNano(updated)
	return &sess, nil
}
