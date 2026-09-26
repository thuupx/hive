package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/thupham/hive/internal/event"
)

// AppendEvents makes events durable on their session stream.
//
// The store assigns Sequence: it is strictly monotonic within a session and
// is assigned only when the event is accepted. Events are persisted at most
// once by ID; a repeated ID is treated as a duplicate, consumes no sequence
// number, and is not written to the outbox a second time.
//
// The returned count is the number of events newly persisted.
//
// AppendEvents must run inside a write transaction so that sequence
// assignment and insertion are atomic.
func (s *Store) AppendEvents(ctx context.Context, tx Execer, evs ...*event.Event) (int, error) {
	maxSeq := make(map[string]int64)
	inserted := 0

	for _, ev := range evs {
		if err := validateEvent(ev); err != nil {
			return inserted, err
		}

		next, known := maxSeq[ev.SessionID]
		if !known {
			if err := tx.QueryRowContext(ctx,
				`SELECT COALESCE(MAX(sequence), 0) FROM events WHERE session_id = ?`,
				ev.SessionID).Scan(&next); err != nil {
				return inserted, fmt.Errorf("storage: read stream sequence for %s: %w", ev.SessionID, err)
			}
		}
		candidate := next + 1

		res, err := tx.ExecContext(ctx, `
			INSERT INTO events
				(id, sequence, session_id, run_id, origin_node, timestamp, type, version, protocol, method, payload, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO NOTHING`,
			ev.ID, candidate, ev.SessionID, ev.RunID, ev.OriginNode,
			unixNano(ev.Timestamp), ev.Type, ev.Version, ev.Protocol, ev.Method,
			string(ev.Payload), unixNano(now()))
		if err != nil {
			return inserted, fmt.Errorf("storage: append event %s: %w", ev.ID, err)
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return inserted, fmt.Errorf("storage: append event %s: %w", ev.ID, err)
		}
		if affected == 0 {
			continue
		}

		maxSeq[ev.SessionID] = candidate
		ev.Sequence = candidate
		inserted++

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO outbox (event_id, session_id, sequence, created_at)
			VALUES (?, ?, ?, ?)`,
			ev.ID, ev.SessionID, candidate, unixNano(now())); err != nil {
			return inserted, fmt.Errorf("storage: enqueue outbox for %s: %w", ev.ID, err)
		}
	}
	return inserted, nil
}

// ReadEvents returns durable events for a session with sequence greater than
// fromSequence, in stream order.
func (s *Store) ReadEvents(ctx context.Context, sessionID string, fromSequence int64, limit int) ([]*event.Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, sequence, session_id, run_id, origin_node, timestamp, type, version, protocol, method, payload
		FROM events
		WHERE session_id = ? AND sequence > ?
		ORDER BY sequence
		LIMIT ?`, sessionID, fromSequence, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: read events for %s: %w", sessionID, err)
	}
	return scanEvents(rows)
}

// EventCount returns the number of durable events on a session stream.
func (s *Store) EventCount(ctx context.Context, sessionID string) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count events for %s: %w", sessionID, err)
	}
	return n, nil
}

// LatestEventOfType returns the newest event of one type on a session stream, or
// nil when the stream holds none.
//
// A status read wants the last thing of a kind — the newest usage report, say —
// and reading the whole stream to find it would grow with the conversation.
func (s *Store) LatestEventOfType(ctx context.Context, sessionID, eventType string) (*event.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, sequence, session_id, run_id, origin_node, timestamp, type, version, protocol, method, payload
		FROM events
		WHERE session_id = ? AND type = ?
		ORDER BY sequence DESC
		LIMIT 1`, sessionID, eventType)
	if err != nil {
		return nil, fmt.Errorf("storage: read the latest %s event for %s: %w", eventType, sessionID, err)
	}

	found, err := scanEvents(rows)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, nil
	}
	return found[0], nil
}

// ToolCallCount returns how many distinct tool calls a session recorded.
//
// A call is published once per state change, so counting events would count
// updates; the agent's own tool-call id is what identifies a call.
func (s *Store) ToolCallCount(ctx context.Context, sessionID string) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT json_extract(payload, '$.toolCallId'))
		FROM events
		WHERE session_id = ? AND type = ?`,
		sessionID, event.TypeTool).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count tool calls for %s: %w", sessionID, err)
	}
	return n, nil
}

// HasEvent reports whether an event id is already durable.
//
// This is what makes buffered-event replay idempotent: a node asks about the
// events it still holds, and the coordinator requests only the ones it does not
// have.
func (s *Store) HasEvent(ctx context.Context, eventID string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM events WHERE id = ?`, eventID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("storage: check event %s: %w", eventID, err)
	}
	return true, nil
}

// PendingOutbox returns durable events that have not been published to the
// event bus yet, in stream order.
func (s *Store) PendingOutbox(ctx context.Context, limit int) ([]*event.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.sequence, e.session_id, e.run_id, e.origin_node, e.timestamp,
		       e.type, e.version, e.protocol, e.method, e.payload
		FROM outbox o
		JOIN events e ON e.id = o.event_id
		WHERE o.published_at IS NULL
		ORDER BY o.sequence
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: read pending outbox: %w", err)
	}
	return scanEvents(rows)
}

// MarkPublished records that events reached the event bus. Publication is
// at-least-once, so marking is best-effort bookkeeping, not a delivery
// guarantee.
func (s *Store) MarkPublished(ctx context.Context, tx Execer, eventIDs ...string) error {
	for _, id := range eventIDs {
		if _, err := tx.ExecContext(ctx, `
			UPDATE outbox SET published_at = ?
			WHERE event_id = ? AND published_at IS NULL`,
			unixNano(now()), id); err != nil {
			return fmt.Errorf("storage: mark event %s published: %w", id, err)
		}
	}
	return nil
}

// MarkPublishedBatch marks events published inside its own write transaction.
func (s *Store) MarkPublishedBatch(ctx context.Context, eventIDs ...string) error {
	if len(eventIDs) == 0 {
		return nil
	}
	return s.WriteTx(ctx, func(tx Execer) error {
		return s.MarkPublished(ctx, tx, eventIDs...)
	})
}

func validateEvent(ev *event.Event) error {
	switch {
	case ev == nil:
		return errors.New("storage: nil event")
	case ev.ID == "":
		return errors.New("storage: event id is required")
	case ev.SessionID == "":
		return fmt.Errorf("storage: event %s: session id is required", ev.ID)
	case ev.Type == "":
		return fmt.Errorf("storage: event %s: type is required", ev.ID)
	case ev.Version < 1:
		return fmt.Errorf("storage: event %s: version must be at least 1", ev.ID)
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = now()
	}
	if len(ev.Payload) == 0 {
		ev.Payload = json.RawMessage("null")
	}
	return nil
}

func scanEvents(rows *sql.Rows) ([]*event.Event, error) {
	defer rows.Close()

	var out []*event.Event
	for rows.Next() {
		var (
			ev       event.Event
			runID    sql.NullString
			origin   sql.NullString
			protocol sql.NullString
			method   sql.NullString
			ts       int64
			payload  string
		)
		if err := rows.Scan(&ev.ID, &ev.Sequence, &ev.SessionID, &runID, &origin, &ts,
			&ev.Type, &ev.Version, &protocol, &method, &payload); err != nil {
			return nil, fmt.Errorf("storage: scan event: %w", err)
		}
		ev.RunID = runID.String
		ev.OriginNode = origin.String
		ev.Protocol = protocol.String
		ev.Method = method.String
		ev.Timestamp = fromUnixNano(ts)
		ev.Payload = json.RawMessage(payload)
		out = append(out, &ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: read events: %w", err)
	}
	return out, nil
}
