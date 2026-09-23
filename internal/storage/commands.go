package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/thupham/hive/internal/command"
	"github.com/thupham/hive/internal/event"
)

// EnsureCommand inserts cmd when its id is new and otherwise returns the
// existing record. The bool reports whether cmd was created.
//
// This is the idempotency boundary for mutating commands: retrying the same
// command id returns the existing logical operation instead of creating a
// second one.
func (s *Store) EnsureCommand(ctx context.Context, tx Execer, cmd *command.Command) (*command.Command, bool, error) {
	if err := cmd.Validate(); err != nil {
		return nil, false, err
	}
	applyCommandTimestamps(cmd)

	res, err := tx.ExecContext(ctx, `
		INSERT INTO commands
			(id, actor, source_id, method, accepted_term, target, state, result, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		cmd.ID, cmd.Actor, cmd.SourceID, cmd.Method, cmd.AcceptedTerm, cmd.Target,
		string(cmd.State), nullableJSON(cmd.Result), nullableJSON(cmd.Error),
		unixNano(cmd.CreatedAt), unixNano(cmd.UpdatedAt))
	if err != nil {
		return nil, false, fmt.Errorf("storage: insert command %s: %w", cmd.ID, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("storage: insert command %s: %w", cmd.ID, err)
	}
	if affected > 0 {
		return cmd, true, nil
	}

	existing, err := s.getCommand(ctx, tx, cmd.ID)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

// GetCommand returns a command by id, or ErrNotFound.
func (s *Store) GetCommand(ctx context.Context, id string) (*command.Command, error) {
	return s.getCommand(ctx, s.db, id)
}

// SetCommandState records a lifecycle transition and its outcome.
//
// The transition is validated by the domain, so a lifecycle change cannot be
// written as an arbitrary state string.
func (s *Store) SetCommandState(ctx context.Context, tx Execer, id string, state command.State, result, errData json.RawMessage) error {
	current, err := s.getCommand(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := current.Transition(state); err != nil {
		return fmt.Errorf("storage: command %s: %w", id, err)
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE commands
		SET state = ?, result = ?, error = ?, updated_at = ?
		WHERE id = ?`,
		string(state), nullableJSON(result), nullableJSON(errData), unixNano(now()), id)
	if err != nil {
		return fmt.Errorf("storage: update command %s: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: update command %s: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("storage: command %s: %w", id, ErrNotFound)
	}
	return nil
}

// AcceptCommandWith commits a command, a domain mutation, and the command's
// events in a single transaction (§7.1.2).
//
// When the command id is already bound to a logical operation, the existing
// record is returned and apply is not called. That is what stops a retry from
// creating a second session, AgentRun, handoff, or permission transition: the
// idempotency decision and the mutation are the same commit.
//
// The bool reports whether this call created the operation.
func (s *Store) AcceptCommandWith(ctx context.Context, cmd *command.Command, apply func(tx Execer) error, evs []*event.Event) (*command.Command, bool, error) {
	var (
		stored  *command.Command
		created bool
	)

	err := s.WriteTx(ctx, func(tx Execer) error {
		var err error
		stored, created, err = s.EnsureCommand(ctx, tx, cmd)
		if err != nil {
			return err
		}
		if !created {
			return nil
		}
		if apply != nil {
			if err := apply(tx); err != nil {
				return err
			}
		}
		_, err = s.AppendEvents(ctx, tx, evs...)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return stored, created, nil
}

// AcceptCommand is AcceptCommandWith without a domain mutation.
func (s *Store) AcceptCommand(ctx context.Context, cmd *command.Command, evs []*event.Event) (*command.Command, bool, error) {
	return s.AcceptCommandWith(ctx, cmd, nil, evs)
}

func (s *Store) getCommand(ctx context.Context, q Execer, id string) (*command.Command, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, actor, source_id, method, accepted_term, target, state, result, error, created_at, updated_at
		FROM commands WHERE id = ?`, id)
	cmd, err := scanCommand(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("storage: command %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read command %s: %w", id, err)
	}
	return cmd, nil
}

func applyCommandTimestamps(cmd *command.Command) {
	if cmd.CreatedAt.IsZero() {
		cmd.CreatedAt = now()
	}
	if cmd.UpdatedAt.IsZero() {
		cmd.UpdatedAt = cmd.CreatedAt
	}
}

func scanCommand(row scanner) (*command.Command, error) {
	var (
		cmd     command.Command
		source  sql.NullString
		term    sql.NullInt64
		target  sql.NullString
		result  sql.NullString
		errCol  sql.NullString
		state   string
		created int64
		updated int64
	)
	if err := row.Scan(&cmd.ID, &cmd.Actor, &source, &cmd.Method, &term, &target,
		&state, &result, &errCol, &created, &updated); err != nil {
		return nil, err
	}
	cmd.SourceID = source.String
	cmd.AcceptedTerm = term.Int64
	cmd.Target = target.String
	cmd.State = command.State(state)
	if result.Valid {
		cmd.Result = json.RawMessage(result.String)
	}
	if errCol.Valid {
		cmd.Error = json.RawMessage(errCol.String)
	}
	cmd.CreatedAt = fromUnixNano(created)
	cmd.UpdatedAt = fromUnixNano(updated)
	return &cmd, nil
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
