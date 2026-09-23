package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/thupham/hive/internal/permission"
)

// InsertPermissionRequest stores a new permission request.
func (s *Store) InsertPermissionRequest(ctx context.Context, tx Execer, req *permission.Request) error {
	if err := req.Validate(); err != nil {
		return err
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("null")
	}
	created := req.CreatedAt
	if created.IsZero() {
		created = now()
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO permission_requests
			(id, session_id, run_id, agent_request_id, payload, state, expires_at, created_at, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.ID, req.SessionID, req.RunID, req.AgentRequestID, string(payload),
		string(req.State), nullableTime(req.ExpiresAt), unixNano(created),
		nullableTime(req.ResolvedAt))
	if err != nil {
		return fmt.Errorf("storage: insert permission request %s: %w", req.ID, err)
	}
	req.CreatedAt = created
	return nil
}

// GetPermissionRequest returns a permission request by id, or ErrNotFound.
func (s *Store) GetPermissionRequest(ctx context.Context, id string) (*permission.Request, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, session_id, run_id, agent_request_id, payload, state, expires_at, created_at, resolved_at
		FROM permission_requests WHERE id = ?`, id)

	req, err := scanPermissionRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("storage: permission request %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read permission request %s: %w", id, err)
	}
	return req, nil
}

// ResolvePermissionRequest applies a terminal transition as an atomic
// compare-and-set and reports whether this call won.
//
// Only the first valid terminal transition wins. A concurrent or repeated
// response observes the already-resolved state instead of producing a second
// authorization.
func (s *Store) ResolvePermissionRequest(ctx context.Context, id string, next permission.State) (*permission.Request, bool, error) {
	if !next.IsTerminal() {
		return nil, false, fmt.Errorf("permission: %s is not a terminal state", next)
	}

	var won bool
	err := s.WriteTx(ctx, func(tx Execer) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE permission_requests
			SET state = ?, resolved_at = ?
			WHERE id = ? AND state = ?`,
			string(next), unixNano(now()), id, string(permission.StatePending))
		if err != nil {
			return fmt.Errorf("storage: resolve permission request %s: %w", id, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("storage: resolve permission request %s: %w", id, err)
		}
		won = affected > 0
		return nil
	})
	if err != nil {
		return nil, false, err
	}

	req, err := s.GetPermissionRequest(ctx, id)
	if err != nil {
		return nil, false, err
	}
	return req, won, nil
}

// ExpirePermissionRequests applies the timeout terminal state to every pending
// request whose deadline has passed, and returns how many were expired.
//
// Expiry is a safe terminal state, never an approval.
func (s *Store) ExpirePermissionRequests(ctx context.Context, at time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}

	var expired int64
	err := s.WriteTx(ctx, func(tx Execer) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE permission_requests
			SET state = ?, resolved_at = ?
			WHERE id IN (
				SELECT id FROM permission_requests
				WHERE state = ? AND expires_at IS NOT NULL AND expires_at <= ?
				ORDER BY expires_at
				LIMIT ?
			)`,
			string(permission.StateExpired), unixNano(at), string(permission.StatePending), unixNano(at), limit)
		if err != nil {
			return fmt.Errorf("storage: expire permission requests: %w", err)
		}
		expired, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, err
	}
	return expired, nil
}

// ListOpenPermissionRequests returns the pending requests of a session, oldest
// first.
func (s *Store) ListOpenPermissionRequests(ctx context.Context, sessionID string) ([]*permission.Request, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, run_id, agent_request_id, payload, state, expires_at, created_at, resolved_at
		FROM permission_requests
		WHERE session_id = ? AND state = ?
		ORDER BY created_at`, sessionID, string(permission.StatePending))
	if err != nil {
		return nil, fmt.Errorf("storage: list permission requests for %s: %w", sessionID, err)
	}
	defer rows.Close()

	var out []*permission.Request
	for rows.Next() {
		req, err := scanPermissionRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("storage: scan permission request: %w", err)
		}
		out = append(out, req)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list permission requests for %s: %w", sessionID, err)
	}
	return out, nil
}

func scanPermissionRequest(row scanner) (*permission.Request, error) {
	var (
		req       permission.Request
		runID     sql.NullString
		payload   string
		state     string
		expiresAt sql.NullInt64
		createdAt int64
		resolved  sql.NullInt64
	)
	if err := row.Scan(&req.ID, &req.SessionID, &runID, &req.AgentRequestID, &payload,
		&state, &expiresAt, &createdAt, &resolved); err != nil {
		return nil, err
	}
	req.RunID = runID.String
	req.Payload = json.RawMessage(payload)
	req.State = permission.State(state)
	req.ExpiresAt = timeFromNull(expiresAt)
	req.CreatedAt = fromUnixNano(createdAt)
	req.ResolvedAt = timeFromNull(resolved)
	return &req, nil
}
