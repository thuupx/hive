package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thupham/hive/internal/event"
)

// ReplayResult is the outcome of a cursor replay request.
type ReplayResult struct {
	// Events are the durable events after the requested cursor.
	Events []*event.Event

	// Gap is set when the requested cursor has been pruned. Events is empty
	// in that case: the client must rehydrate before continuing rather than
	// silently skipping history.
	Gap *event.CursorGap
}

// EarliestSequence returns the lowest durable sequence still present on a
// session stream, or 0 when the stream currently holds no events.
//
// This is an introspection aid. Replay decisions use PrunedThrough, because a
// fully pruned stream has no events left and would otherwise look empty.
func (s *Store) EarliestSequence(ctx context.Context, sessionID string) (int64, error) {
	var seq *int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MIN(sequence) FROM events WHERE session_id = ?`, sessionID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("storage: read earliest sequence for %s: %w", sessionID, err)
	}
	if seq == nil {
		return 0, nil
	}
	return *seq, nil
}

// PrunedThrough returns the highest sequence that retention has deleted on a
// session stream, or 0 when nothing has been pruned.
//
// Every sequence at or below this value is permanently unavailable.
func (s *Store) PrunedThrough(ctx context.Context, sessionID string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx,
		`SELECT pruned_through FROM stream_watermarks WHERE session_id = ?`, sessionID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("storage: read stream watermark for %s: %w", sessionID, err)
	}
	return seq, nil
}

// Replay returns durable events after fromSequence.
//
// When the requested cursor has been pruned, the result carries a cursor gap
// instead of skipping history: the client must rehydrate from the snapshot
// boundary and resume from the next valid sequence.
func (s *Store) Replay(ctx context.Context, sessionID string, fromSequence int64, limit int) (*ReplayResult, error) {
	prunedThrough, err := s.PrunedThrough(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	if fromSequence < prunedThrough {
		gap := &event.CursorGap{
			Requested:    fromSequence,
			NextSequence: prunedThrough + 1,
		}

		snap, err := s.LatestSnapshot(ctx, sessionID)
		switch {
		case err == nil:
			gap.SnapshotID = snap.ID
			gap.SnapshotSequence = snap.Sequence
		case errors.Is(err, ErrNotFound):
			// No snapshot: the client resumes from NextSequence without
			// context.
		default:
			return nil, err
		}
		return &ReplayResult{Gap: gap}, nil
	}

	events, err := s.ReadEvents(ctx, sessionID, fromSequence, limit)
	if err != nil {
		return nil, err
	}
	return &ReplayResult{Events: events}, nil
}

// PruneEvents deletes durable events created before the cutoff, up to limit,
// and advances the session prune watermark.
//
// Only already-published events are pruned. Dropping an unpublished event
// would lose realtime delivery that has not happened yet. Event retention and
// session retention are independent, so sessions are untouched.
func (s *Store) PruneEvents(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}

	var deleted int64
	err := s.WriteTx(ctx, func(tx Execer) error {
		ids, highest, err := pruneCandidates(ctx, tx, cutoff, limit)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}

		placeholders := make([]string, len(ids))
		args := make([]any, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			args[i] = id
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM events WHERE id IN (`+strings.Join(placeholders, ", ")+`)`, args...)
		if err != nil {
			return fmt.Errorf("storage: prune events: %w", err)
		}
		if deleted, err = res.RowsAffected(); err != nil {
			return fmt.Errorf("storage: prune events: %w", err)
		}

		for sessionID, seq := range highest {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO stream_watermarks (session_id, pruned_through, updated_at)
				VALUES (?, ?, ?)
				ON CONFLICT (session_id) DO UPDATE SET
					pruned_through = MAX(pruned_through, excluded.pruned_through),
					updated_at = excluded.updated_at`,
				sessionID, seq, unixNano(now())); err != nil {
				return fmt.Errorf("storage: update stream watermark for %s: %w", sessionID, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// pruneCandidates returns the event ids eligible for pruning and, per session,
// the highest sequence among them.
func pruneCandidates(ctx context.Context, tx Execer, cutoff time.Time, limit int) ([]string, map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT e.id, e.session_id, e.sequence
		FROM events e
		JOIN outbox o ON o.event_id = e.id
		WHERE o.published_at IS NOT NULL AND e.created_at < ?
		ORDER BY e.sequence
		LIMIT ?`, unixNano(cutoff), limit)
	if err != nil {
		return nil, nil, fmt.Errorf("storage: select prune candidates: %w", err)
	}
	defer rows.Close()

	var ids []string
	highest := map[string]int64{}
	for rows.Next() {
		var (
			id        string
			sessionID string
			sequence  int64
		)
		if err := rows.Scan(&id, &sessionID, &sequence); err != nil {
			return nil, nil, fmt.Errorf("storage: scan prune candidate: %w", err)
		}
		ids = append(ids, id)
		if sequence > highest[sessionID] {
			highest[sessionID] = sequence
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("storage: select prune candidates: %w", err)
	}
	return ids, highest, nil
}
