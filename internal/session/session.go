// Package session defines the Hive Session model.
//
// The Hive Session is the central domain abstraction. It is not an agent
// session: a session can contain multiple AgentRuns, and a completed
// AgentRun does not end the session.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// State is the lifecycle state of a Hive Session.
type State string

const (
	StateActive   State = "active"
	StateIdle     State = "idle"
	StateHandoff  State = "handoff"
	StateArchived State = "archived"
)

// transitions is the allowed state graph.
var transitions = map[State][]State{
	StateActive:   {StateIdle, StateHandoff, StateArchived},
	StateIdle:     {StateActive, StateHandoff, StateArchived},
	StateHandoff:  {StateActive, StateIdle, StateArchived},
	StateArchived: {StateActive},
}

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

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
var ErrInvalidTransition = errors.New("session: invalid state transition")

// Session is the orchestration context that outlives any single agent run.
//
// DefaultInteractiveRunID is a domain field, not metadata: it is the
// deterministic input to prompt routing.
type Session struct {
	ID                      string
	State                   State
	WorkspaceID             string
	DefaultInteractiveRunID string

	// AgentConfig are the agent selectors the user chose for this session, keyed
	// by the agent's own config id.
	//
	// It lives on the session rather than on a run, because a new AgentRun
	// continues the same conversation: a user who picked a model should not have
	// it silently revert when the next run starts.
	AgentConfig map[string]string

	Metadata  json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SetAgentConfig records a chosen selector value.
func (s *Session) SetAgentConfig(configID, value string) {
	if s.AgentConfig == nil {
		s.AgentConfig = map[string]string{}
	}
	s.AgentConfig[configID] = value
	s.UpdatedAt = time.Now().UTC()
}

// New creates an active session.
func New(id string) *Session {
	now := time.Now().UTC()
	return &Session{
		ID:          id,
		State:       StateActive,
		AgentConfig: map[string]string{},
		Metadata:    json.RawMessage("{}"),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// Validate reports whether the session is well formed enough to persist.
func (s *Session) Validate() error {
	switch {
	case s == nil:
		return errors.New("session: nil session")
	case s.ID == "":
		return errors.New("session: id is required")
	case !s.State.Valid():
		return fmt.Errorf("session: %s: unknown state %q", s.ID, s.State)
	}
	return nil
}

// Transition moves the session to next, validating the transition.
//
// A transition to the current state is a no-op so that retried operations are
// idempotent.
func (s *Session) Transition(next State) error {
	if !s.State.Valid() {
		return fmt.Errorf("%w: current state %q is unknown", ErrInvalidTransition, s.State)
	}
	if !next.Valid() {
		return fmt.Errorf("%w: target state %q is unknown", ErrInvalidTransition, next)
	}
	if s.State == next {
		return nil
	}
	if !s.State.CanTransitionTo(next) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, s.State, next)
	}
	s.State = next
	s.UpdatedAt = time.Now().UTC()
	return nil
}

// SetDefaultInteractiveRun points prompt routing at runID.
//
// A handoff performs this on activation, not when the handoff record is
// created: the target run must already be able to accept a prompt, otherwise
// routing would point at a run that cannot serve.
func (s *Session) SetDefaultInteractiveRun(runID string) error {
	if runID == "" {
		return errors.New("session: run id is required")
	}
	s.DefaultInteractiveRunID = runID
	s.UpdatedAt = time.Now().UTC()
	return nil
}

// Snapshot is a point-in-time representation of the context required to
// resume or hand off a session.
//
// A snapshot is not a replacement for the event log, and it must identify the
// context schema it was built with so a later version can migrate or reject
// it explicitly.
type Snapshot struct {
	ID            string
	SessionID     string
	SchemaVersion int
	Sequence      int64
	Payload       json.RawMessage
	CreatedAt     time.Time
}
