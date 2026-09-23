package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/thupham/hive/internal/handoff"
)

// InsertHandoff stores a new handoff record.
func (s *Store) InsertHandoff(ctx context.Context, tx Execer, h *handoff.Handoff) error {
	if err := h.Validate(); err != nil {
		return err
	}

	created := h.CreatedAt
	if created.IsZero() {
		created = now()
	}
	updated := h.UpdatedAt
	if updated.IsZero() {
		updated = created
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO handoffs
			(id, session_id, source_run_id, target_run_id, state, summary, context_snapshot_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.ID, h.SessionID, h.SourceRunID, h.TargetRunID, string(h.State),
		h.Summary, h.ContextSnapshotID, unixNano(created), unixNano(updated))
	if err != nil {
		return fmt.Errorf("storage: insert handoff %s: %w", h.ID, err)
	}

	h.CreatedAt = created
	h.UpdatedAt = updated
	return nil
}

// UpdateHandoff persists the mutable fields of an existing handoff.
//
// It writes the whole row, so a caller that may be racing another writer must
// load the handoff inside the same transaction. Prefer UpdateHandoffWith.
func (s *Store) UpdateHandoff(ctx context.Context, tx Execer, h *handoff.Handoff) error {
	return s.updateHandoff(ctx, tx, h)
}

func (s *Store) updateHandoff(ctx context.Context, tx Execer, h *handoff.Handoff) error {
	if err := h.Validate(); err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE handoffs
		SET state = ?, summary = ?, context_snapshot_id = ?, updated_at = ?
		WHERE id = ?`,
		string(h.State), h.Summary, h.ContextSnapshotID, unixNano(now()), h.ID)
	if err != nil {
		return fmt.Errorf("storage: update handoff %s: %w", h.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: update handoff %s: %w", h.ID, err)
	}
	if affected == 0 {
		return fmt.Errorf("storage: handoff %s: %w", h.ID, ErrNotFound)
	}
	return nil
}

// UpdateHandoffWith loads a handoff inside a write transaction, applies fn, and
// persists the result.
func (s *Store) UpdateHandoffWith(ctx context.Context, handoffID string, fn func(h *handoff.Handoff) error) (*handoff.Handoff, error) {
	var updated *handoff.Handoff

	err := s.WriteTx(ctx, func(tx Execer) error {
		current, err := s.getHandoff(ctx, tx, handoffID)
		if err != nil {
			return err
		}
		if err := fn(current); err != nil {
			return err
		}
		if err := s.updateHandoff(ctx, tx, current); err != nil {
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

// GetHandoff returns a handoff by id, or ErrNotFound.
func (s *Store) GetHandoff(ctx context.Context, id string) (*handoff.Handoff, error) {
	return s.getHandoff(ctx, s.db, id)
}

func (s *Store) getHandoff(ctx context.Context, q Execer, id string) (*handoff.Handoff, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, session_id, source_run_id, target_run_id, state, summary, context_snapshot_id, created_at, updated_at
		FROM handoffs WHERE id = ?`, id)

	h, err := scanHandoff(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("storage: handoff %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read handoff %s: %w", id, err)
	}
	return h, nil
}

// ListHandoffs returns the handoffs of a session, oldest first.
func (s *Store) ListHandoffs(ctx context.Context, sessionID string) ([]*handoff.Handoff, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, source_run_id, target_run_id, state, summary, context_snapshot_id, created_at, updated_at
		FROM handoffs
		WHERE session_id = ?
		ORDER BY created_at`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("storage: list handoffs for %s: %w", sessionID, err)
	}
	defer rows.Close()

	var out []*handoff.Handoff
	for rows.Next() {
		h, err := scanHandoff(rows)
		if err != nil {
			return nil, fmt.Errorf("storage: scan handoff: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list handoffs for %s: %w", sessionID, err)
	}
	return out, nil
}

func scanHandoff(row scanner) (*handoff.Handoff, error) {
	var (
		h          handoff.Handoff
		sourceRun  sql.NullString
		state      string
		summary    sql.NullString
		snapshotID sql.NullString
		created    int64
		updated    int64
	)
	if err := row.Scan(&h.ID, &h.SessionID, &sourceRun, &h.TargetRunID, &state,
		&summary, &snapshotID, &created, &updated); err != nil {
		return nil, err
	}
	h.SourceRunID = sourceRun.String
	h.State = handoff.State(state)
	h.Summary = summary.String
	h.ContextSnapshotID = snapshotID.String
	h.CreatedAt = fromUnixNano(created)
	h.UpdatedAt = fromUnixNano(updated)
	return &h, nil
}
