package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// PutSnapshot stores a context snapshot.
//
// SchemaVersion is required: a snapshot must identify the context schema it
// was built with so a later version can migrate or reject it explicitly. A
// snapshot is not a replacement for the event log.
func (s *Store) PutSnapshot(ctx context.Context, tx Execer, snap *Snapshot) error {
	switch {
	case snap == nil:
		return errors.New("storage: nil snapshot")
	case snap.ID == "":
		return errors.New("storage: snapshot id is required")
	case snap.SessionID == "":
		return fmt.Errorf("storage: snapshot %s: session id is required", snap.ID)
	case snap.SchemaVersion < 1:
		return fmt.Errorf("storage: snapshot %s: schema version must be at least 1", snap.ID)
	}
	if len(snap.Payload) == 0 {
		snap.Payload = json.RawMessage("null")
	}
	if snap.CreatedAt.IsZero() {
		snap.CreatedAt = now()
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO session_snapshots (id, session_id, schema_version, sequence, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		snap.ID, snap.SessionID, snap.SchemaVersion, snap.Sequence,
		string(snap.Payload), unixNano(snap.CreatedAt))
	if err != nil {
		return fmt.Errorf("storage: insert snapshot %s: %w", snap.ID, err)
	}
	return nil
}

// LatestSnapshot returns the snapshot with the highest sequence for a
// session, or ErrNotFound.
func (s *Store) LatestSnapshot(ctx context.Context, sessionID string) (*Snapshot, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, session_id, schema_version, sequence, payload, created_at
		FROM session_snapshots
		WHERE session_id = ?
		ORDER BY sequence DESC, created_at DESC
		LIMIT 1`, sessionID)

	var (
		snap    Snapshot
		payload string
		created int64
	)
	err := row.Scan(&snap.ID, &snap.SessionID, &snap.SchemaVersion, &snap.Sequence, &payload, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("storage: snapshot for session %s: %w", sessionID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read snapshot for %s: %w", sessionID, err)
	}
	snap.Payload = json.RawMessage(payload)
	snap.CreatedAt = fromUnixNano(created)
	return &snap, nil
}
