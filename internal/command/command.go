// Package command defines the mutating command model.
//
// A command is a logical operation. Its id is the idempotency key and is
// stable across retries of the same operation, so a retry resolves to the
// existing operation instead of creating a second one.
package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// State is the lifecycle state of a mutating command.
type State string

const (
	StateReceived  State = "received"
	StateAccepted  State = "accepted"
	StateCompleted State = "completed"
	StateRejected  State = "rejected"
	StateFailed    State = "failed"
	StateRetryable State = "retryable"
)

// transitions is the allowed state graph.
var transitions = map[State][]State{
	StateReceived:  {StateAccepted, StateRejected, StateFailed},
	StateAccepted:  {StateCompleted, StateRejected, StateFailed, StateRetryable},
	StateRetryable: {StateAccepted, StateCompleted, StateRejected, StateFailed},
	StateCompleted: nil,
	StateRejected:  nil,
	StateFailed:    nil,
}

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// IsTerminal reports whether s is a final outcome.
func (s State) IsTerminal() bool {
	switch s {
	case StateCompleted, StateRejected, StateFailed:
		return true
	default:
		return false
	}
}

// IsPending reports whether the operation has not reached an outcome yet.
func (s State) IsPending() bool { return s.Valid() && !s.IsTerminal() }

// CanTransitionTo reports whether s may move directly to next.
func (s State) CanTransitionTo(next State) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// ErrInvalidTransition reports a state change the domain does not allow.
var ErrInvalidTransition = errors.New("command: invalid state transition")

// Command is the durable record of a logical mutating operation.
//
// SourceID correlates the command with the originating transport delivery
// when the transport provides a stable identifier. It is delivery identity,
// not operation identity: a redelivery reuses SourceID, while a user
// intentionally repeating an action produces a new SourceID and therefore a
// new command.
//
// AcceptedTerm is always 0 in v1 and is reserved for coordinator failover.
type Command struct {
	ID           string
	Actor        string
	SourceID     string
	Method       string
	AcceptedTerm int64
	Target       string
	State        State
	Result       json.RawMessage
	Error        json.RawMessage
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// New creates a command in the received state.
func New(id, actor, method string) *Command {
	now := time.Now().UTC()
	return &Command{
		ID:        id,
		Actor:     actor,
		Method:    method,
		State:     StateReceived,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// Validate reports whether the command is well formed enough to persist.
func (c *Command) Validate() error {
	switch {
	case c == nil:
		return errors.New("command: nil command")
	case c.ID == "":
		return errors.New("command: id is required")
	case c.Method == "":
		return fmt.Errorf("command: %s: method is required", c.ID)
	case !c.State.Valid():
		return fmt.Errorf("command: %s: unknown state %q", c.ID, c.State)
	}
	return nil
}

// IsTerminal reports whether the operation reached an outcome.
func (c *Command) IsTerminal() bool { return c.State.IsTerminal() }

// Transition moves the command to next, validating the transition.
//
// A transition to the current state is a no-op so that a retried command
// resolves idempotently.
func (c *Command) Transition(next State) error {
	if !c.State.Valid() {
		return fmt.Errorf("%w: current state %q is unknown", ErrInvalidTransition, c.State)
	}
	if !next.Valid() {
		return fmt.Errorf("%w: target state %q is unknown", ErrInvalidTransition, next)
	}
	if c.State == next {
		return nil
	}
	if !c.State.CanTransitionTo(next) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, c.State, next)
	}
	c.State = next
	c.UpdatedAt = time.Now().UTC()
	return nil
}
