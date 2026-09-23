package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/thupham/hive/internal/agent"
)

// InsertAgentRun stores a new AgentRun.
func (s *Store) InsertAgentRun(ctx context.Context, tx Execer, run *agent.AgentRun) error {
	if err := run.Validate(); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO agent_runs
			(id, session_id, agent_id, node_id, protocol, runtime_session_id,
			 execution_generation, node_execution_id, state, started_at, ended_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.SessionID, run.AgentID, run.NodeID, run.Protocol, run.RuntimeSessionID,
		run.ExecutionGeneration, run.NodeExecutionID, string(run.State),
		nullableTime(run.StartedAt), nullableTime(run.EndedAt))
	if err != nil {
		return fmt.Errorf("storage: insert agent run %s: %w", run.ID, err)
	}
	return nil
}

// UpdateAgentRun persists the mutable fields of an existing AgentRun,
// including its execution generation.
func (s *Store) UpdateAgentRun(ctx context.Context, tx Execer, run *agent.AgentRun) error {
	if err := run.Validate(); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE agent_runs
		SET node_id = ?, runtime_session_id = ?, execution_generation = ?,
		    node_execution_id = ?, state = ?, started_at = ?, ended_at = ?
		WHERE id = ?`,
		run.NodeID, run.RuntimeSessionID, run.ExecutionGeneration,
		run.NodeExecutionID, string(run.State),
		nullableTime(run.StartedAt), nullableTime(run.EndedAt), run.ID)
	if err != nil {
		return fmt.Errorf("storage: update agent run %s: %w", run.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: update agent run %s: %w", run.ID, err)
	}
	if affected == 0 {
		return fmt.Errorf("storage: agent run %s: %w", run.ID, ErrNotFound)
	}
	return nil
}

// GetAgentRun returns an AgentRun by id, or ErrNotFound.
func (s *Store) GetAgentRun(ctx context.Context, id string) (*agent.AgentRun, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, session_id, agent_id, node_id, protocol, runtime_session_id,
		       execution_generation, node_execution_id, state, started_at, ended_at
		FROM agent_runs WHERE id = ?`, id)

	run, err := scanAgentRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("storage: agent run %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read agent run %s: %w", id, err)
	}
	return run, nil
}

// ListAgentRuns returns the runs of a session, oldest first.
func (s *Store) ListAgentRuns(ctx context.Context, sessionID string) ([]*agent.AgentRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, agent_id, node_id, protocol, runtime_session_id,
		       execution_generation, node_execution_id, state, started_at, ended_at
		FROM agent_runs
		WHERE session_id = ?
		ORDER BY rowid`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("storage: list agent runs for %s: %w", sessionID, err)
	}
	defer rows.Close()

	var out []*agent.AgentRun
	for rows.Next() {
		run, err := scanAgentRun(rows)
		if err != nil {
			return nil, fmt.Errorf("storage: scan agent run: %w", err)
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list agent runs for %s: %w", sessionID, err)
	}
	return out, nil
}

func scanAgentRun(row scanner) (*agent.AgentRun, error) {
	var (
		run       agent.AgentRun
		state     string
		nodeID    sql.NullString
		runtimeID sql.NullString
		nodeExec  sql.NullString
		started   sql.NullInt64
		ended     sql.NullInt64
	)
	if err := row.Scan(&run.ID, &run.SessionID, &run.AgentID, &nodeID, &run.Protocol,
		&runtimeID, &run.ExecutionGeneration, &nodeExec, &state, &started, &ended); err != nil {
		return nil, err
	}
	run.NodeID = nodeID.String
	run.RuntimeSessionID = runtimeID.String
	run.NodeExecutionID = nodeExec.String
	run.State = agent.State(state)
	run.StartedAt = timeFromNull(started)
	run.EndedAt = timeFromNull(ended)
	return &run, nil
}
