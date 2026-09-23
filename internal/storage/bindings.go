package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Binding is a transport conversation bound to a Hive session.
//
// The binding cursor is the last Hive event sequence the transport has durably
// accepted for that binding. Transport-local delivery offsets stay
// transport-owned; this is only the Hive side.
type Binding struct {
	ID             string
	SessionID      string
	Transport      string
	ConversationID string
	Principal      string
	EventCursor    int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// UpsertBinding stores or refreshes a conversation binding.
func (s *Store) UpsertBinding(ctx context.Context, tx Execer, b *Binding) error {
	switch {
	case b == nil:
		return errors.New("storage: nil binding")
	case b.ID == "":
		return errors.New("storage: binding id is required")
	case b.SessionID == "":
		return fmt.Errorf("storage: binding %s: session id is required", b.ID)
	case b.Transport == "":
		return fmt.Errorf("storage: binding %s: transport is required", b.ID)
	case b.ConversationID == "":
		return fmt.Errorf("storage: binding %s: conversation id is required", b.ID)
	}

	now := time.Now().UTC()
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
	b.UpdatedAt = now

	_, err := tx.ExecContext(ctx, `
		INSERT INTO conversation_bindings
			(id, session_id, transport, conversation_id, principal, event_cursor, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (transport, conversation_id) DO UPDATE SET
			session_id = excluded.session_id,
			principal = excluded.principal,
			updated_at = excluded.updated_at`,
		b.ID, b.SessionID, b.Transport, b.ConversationID, b.Principal, b.EventCursor,
		unixNano(b.CreatedAt), unixNano(b.UpdatedAt))
	if err != nil {
		return fmt.Errorf("storage: upsert binding %s: %w", b.ID, err)
	}
	return nil
}

// GetBinding returns the binding for a transport conversation, or ErrNotFound.
func (s *Store) GetBinding(ctx context.Context, transport, conversationID string) (*Binding, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, session_id, transport, conversation_id, principal, event_cursor, created_at, updated_at
		FROM conversation_bindings
		WHERE transport = ? AND conversation_id = ?`, transport, conversationID)

	binding, err := scanBinding(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("storage: binding for %s/%s: %w", transport, conversationID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read binding for %s/%s: %w", transport, conversationID, err)
	}
	return binding, nil
}

// ListBindingsForSession returns the bindings attached to a session.
func (s *Store) ListBindingsForSession(ctx context.Context, sessionID string) ([]*Binding, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, transport, conversation_id, principal, event_cursor, created_at, updated_at
		FROM conversation_bindings
		WHERE session_id = ?
		ORDER BY created_at`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("storage: list bindings for %s: %w", sessionID, err)
	}
	return scanBindings(rows)
}

// ListSessionsForPrincipal returns the sessions a principal is bound to.
//
// This is what session isolation is built on: a principal that is not the owner
// reaches a session only through a binding it holds.
func (s *Store) ListSessionsForPrincipal(ctx context.Context, principal string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT session_id
		FROM conversation_bindings
		WHERE principal = ?
		ORDER BY session_id`, principal)
	if err != nil {
		return nil, fmt.Errorf("storage: list sessions for %s: %w", principal, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			return nil, fmt.Errorf("storage: scan session id: %w", err)
		}
		out = append(out, sessionID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list sessions for %s: %w", principal, err)
	}
	return out, nil
}

// AdvanceBindingCursor records that a transport durably accepted an event.
//
// The cursor only moves forward, so a redelivered older event cannot rewind it.
func (s *Store) AdvanceBindingCursor(ctx context.Context, tx Execer, bindingID string, sequence int64) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE conversation_bindings
		SET event_cursor = ?, updated_at = ?
		WHERE id = ? AND event_cursor < ?`,
		sequence, unixNano(time.Now().UTC()), bindingID, sequence)
	if err != nil {
		return fmt.Errorf("storage: advance binding %s: %w", bindingID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: advance binding %s: %w", bindingID, err)
	}
	if affected == 0 {
		// Either the binding is gone or the cursor is already at or past the
		// sequence. Both are fine; distinguish only for the missing case.
		var exists int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM conversation_bindings WHERE id = ?`, bindingID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("storage: binding %s: %w", bindingID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("storage: advance binding %s: %w", bindingID, err)
		}
	}
	return nil
}

func scanBinding(row scanner) (*Binding, error) {
	var (
		b         Binding
		principal sql.NullString
		created   int64
		updated   int64
	)
	if err := row.Scan(&b.ID, &b.SessionID, &b.Transport, &b.ConversationID, &principal,
		&b.EventCursor, &created, &updated); err != nil {
		return nil, err
	}
	b.Principal = principal.String
	b.CreatedAt = fromUnixNano(created)
	b.UpdatedAt = fromUnixNano(updated)
	return &b, nil
}

func scanBindings(rows *sql.Rows) ([]*Binding, error) {
	defer rows.Close()

	var out []*Binding
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("storage: scan binding: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: read bindings: %w", err)
	}
	return out, nil
}
