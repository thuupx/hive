package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/thupham/hive/internal/event"
)

// NodeExecution is the node's record of one execution generation.
//
// It is execution evidence, not authoritative domain state: the coordinator
// owns the AgentRun state machine, and this record exists so the node can report
// what it actually has when the coordinator asks.
type NodeExecution struct {
	AgentRunID       string
	Generation       int64
	NodeExecutionID  string
	RuntimeSessionID string
	State            string
	StartedAt        time.Time
	UpdatedAt        time.Time
}

// BufferedEvent is an event held on the node until the coordinator accepts it.
//
// LocalSequence is node-local ordering, a synchronization aid only. The
// authoritative stream sequence is assigned by the coordinator when the event
// becomes durable.
type BufferedEvent struct {
	Event         *event.Event
	LocalSequence int64
	BufferedAt    time.Time
}

// PutNodeExecution stores or replaces the node's execution record.
func (s *Store) PutNodeExecution(ctx context.Context, tx Execer, exec *NodeExecution) error {
	switch {
	case exec == nil:
		return errors.New("storage: nil node execution")
	case exec.AgentRunID == "":
		return errors.New("storage: node execution agent run id is required")
	case exec.Generation < 1:
		return fmt.Errorf("storage: node execution %s: generation must be at least 1", exec.AgentRunID)
	}

	started := exec.StartedAt
	if started.IsZero() {
		started = now()
	}
	updated := exec.UpdatedAt
	if updated.IsZero() {
		updated = started
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO node_executions
			(agent_run_id, generation, node_execution_id, runtime_session_id, state, started_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_run_id) DO UPDATE SET
			generation = excluded.generation,
			node_execution_id = excluded.node_execution_id,
			runtime_session_id = excluded.runtime_session_id,
			state = excluded.state,
			updated_at = excluded.updated_at`,
		exec.AgentRunID, exec.Generation, exec.NodeExecutionID, exec.RuntimeSessionID,
		exec.State, unixNano(started), unixNano(updated))
	if err != nil {
		return fmt.Errorf("storage: put node execution %s: %w", exec.AgentRunID, err)
	}

	exec.StartedAt = started
	exec.UpdatedAt = updated
	return nil
}

// GetNodeExecution returns the node's record for an AgentRun, or ErrNotFound.
func (s *Store) GetNodeExecution(ctx context.Context, agentRunID string) (*NodeExecution, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT agent_run_id, generation, node_execution_id, runtime_session_id, state, started_at, updated_at
		FROM node_executions WHERE agent_run_id = ?`, agentRunID)

	var (
		exec       NodeExecution
		nodeExec   sql.NullString
		runtimeSID sql.NullString
		started    int64
		updated    int64
	)
	err := row.Scan(&exec.AgentRunID, &exec.Generation, &nodeExec, &runtimeSID,
		&exec.State, &started, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("storage: node execution %s: %w", agentRunID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read node execution %s: %w", agentRunID, err)
	}

	exec.NodeExecutionID = nodeExec.String
	exec.RuntimeSessionID = runtimeSID.String
	exec.StartedAt = fromUnixNano(started)
	exec.UpdatedAt = fromUnixNano(updated)
	return &exec, nil
}

// DeleteNodeExecution removes the node's record for an AgentRun.
func (s *Store) DeleteNodeExecution(ctx context.Context, tx Execer, agentRunID string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM node_executions WHERE agent_run_id = ?`, agentRunID); err != nil {
		return fmt.Errorf("storage: delete node execution %s: %w", agentRunID, err)
	}
	return nil
}

// ListNodeExecutions returns the node's execution records, oldest first.
func (s *Store) ListNodeExecutions(ctx context.Context) ([]*NodeExecution, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT agent_run_id, generation, node_execution_id, runtime_session_id, state, started_at, updated_at
		FROM node_executions
		ORDER BY started_at`)
	if err != nil {
		return nil, fmt.Errorf("storage: list node executions: %w", err)
	}
	defer rows.Close()

	var out []*NodeExecution
	for rows.Next() {
		var (
			exec       NodeExecution
			nodeExec   sql.NullString
			runtimeSID sql.NullString
			started    int64
			updated    int64
		)
		if err := rows.Scan(&exec.AgentRunID, &exec.Generation, &nodeExec, &runtimeSID,
			&exec.State, &started, &updated); err != nil {
			return nil, fmt.Errorf("storage: scan node execution: %w", err)
		}
		exec.NodeExecutionID = nodeExec.String
		exec.RuntimeSessionID = runtimeSID.String
		exec.StartedAt = fromUnixNano(started)
		exec.UpdatedAt = fromUnixNano(updated)
		out = append(out, &exec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list node executions: %w", err)
	}
	return out, nil
}

// BufferEvent stores an event locally and returns its node-local sequence.
//
// A repeated event id keeps its existing sequence and does not create a second
// buffered event, so a redelivery from the agent cannot duplicate the buffer.
func (s *Store) BufferEvent(ctx context.Context, tx Execer, ev *event.Event) (int64, error) {
	if err := validateEvent(ev); err != nil {
		return 0, err
	}

	var existing int64
	err := tx.QueryRowContext(ctx,
		`SELECT local_sequence FROM node_event_buffer WHERE event_id = ?`, ev.ID).Scan(&existing)
	switch {
	case err == nil:
		return existing, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("storage: read buffered event %s: %w", ev.ID, err)
	}

	var next int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(local_sequence), 0) FROM node_event_buffer`).Scan(&next); err != nil {
		return 0, fmt.Errorf("storage: read buffer sequence: %w", err)
	}
	sequence := next + 1

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO node_event_buffer
			(event_id, local_sequence, session_id, run_id, type, version, protocol, method, payload, buffered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, sequence, ev.SessionID, ev.RunID, ev.Type, ev.Version,
		ev.Protocol, ev.Method, string(ev.Payload), unixNano(now())); err != nil {
		return 0, fmt.Errorf("storage: buffer event %s: %w", ev.ID, err)
	}
	return sequence, nil
}

// BufferedEvents returns buffered events in local order.
func (s *Store) BufferedEvents(ctx context.Context, limit int) ([]*BufferedEvent, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id, local_sequence, session_id, run_id, type, version, protocol, method, payload, buffered_at
		FROM node_event_buffer
		ORDER BY local_sequence
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: read buffered events: %w", err)
	}
	return scanBufferedEvents(rows)
}

// BufferedEventByID returns one buffered event, or ErrNotFound.
func (s *Store) BufferedEventByID(ctx context.Context, eventID string) (*BufferedEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id, local_sequence, session_id, run_id, type, version, protocol, method, payload, buffered_at
		FROM node_event_buffer WHERE event_id = ?`, eventID)
	if err != nil {
		return nil, fmt.Errorf("storage: read buffered event %s: %w", eventID, err)
	}
	buffered, err := scanBufferedEvents(rows)
	if err != nil {
		return nil, err
	}
	if len(buffered) == 0 {
		return nil, fmt.Errorf("storage: buffered event %s: %w", eventID, ErrNotFound)
	}
	return buffered[0], nil
}

// BufferedCount returns how many events are still buffered locally.
func (s *Store) BufferedCount(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_event_buffer`).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count buffered events: %w", err)
	}
	return n, nil
}

// DeleteBufferedEvents removes events the coordinator has accepted.
func (s *Store) DeleteBufferedEvents(ctx context.Context, tx Execer, eventIDs ...string) error {
	for _, id := range eventIDs {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM node_event_buffer WHERE event_id = ?`, id); err != nil {
			return fmt.Errorf("storage: delete buffered event %s: %w", id, err)
		}
	}
	return nil
}

func scanBufferedEvents(rows *sql.Rows) ([]*BufferedEvent, error) {
	defer rows.Close()

	var out []*BufferedEvent
	for rows.Next() {
		var (
			ev       event.Event
			sequence int64
			runID    sql.NullString
			protocol sql.NullString
			method   sql.NullString
			payload  string
			at       int64
		)
		if err := rows.Scan(&ev.ID, &sequence, &ev.SessionID, &runID, &ev.Type, &ev.Version,
			&protocol, &method, &payload, &at); err != nil {
			return nil, fmt.Errorf("storage: scan buffered event: %w", err)
		}
		ev.RunID = runID.String
		ev.Protocol = protocol.String
		ev.Method = method.String
		ev.Payload = json.RawMessage(payload)

		out = append(out, &BufferedEvent{
			Event:         &ev,
			LocalSequence: sequence,
			BufferedAt:    fromUnixNano(at),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: read buffered events: %w", err)
	}
	return out, nil
}
