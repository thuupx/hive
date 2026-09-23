// Package agent defines the AgentRun model.
//
// An AgentRun is the execution assignment connecting a Hive Session to a
// particular agent runtime on a node. A Session is not owned by a node, and
// a completed AgentRun does not end the Session.
package agent

import (
	"errors"
	"fmt"
	"time"
)

// State is the lifecycle state of an AgentRun.
type State string

const (
	StateCreated     State = "created"
	StateQueued      State = "queued"
	StateStarting    State = "starting"
	StateRunning     State = "running"
	StateCompleted   State = "completed"
	StateCancelled   State = "cancelled"
	StateFailed      State = "failed"
	StateInterrupted State = "interrupted"
)

// transitions is the allowed state graph.
//
// interrupted has no outgoing edge on purpose. Recovering an interrupted run
// requires an explicit decision that creates a new execution generation, so
// it goes through Recover rather than Transition.
//
// starting may go straight to completed. The coordinator's view is derived from
// what the node reports, and a report of "running" can be coalesced away or lost
// while the terminal report survives. Refusing a terminal report because
// "running" was never observed would leave the run stuck in starting, which is
// worse than accepting that it finished quickly.
var transitions = map[State][]State{
	StateCreated:     {StateQueued, StateStarting, StateCancelled, StateFailed},
	StateQueued:      {StateStarting, StateCancelled, StateFailed},
	StateStarting:    {StateRunning, StateCompleted, StateFailed, StateCancelled, StateInterrupted},
	StateRunning:     {StateCompleted, StateFailed, StateCancelled, StateInterrupted},
	StateCompleted:   nil,
	StateCancelled:   nil,
	StateFailed:      nil,
	StateInterrupted: nil,
}

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// IsWorking reports whether a turn may be in flight.
//
// A handoff between turns is the normal case: a user asks for one when the agent
// has finished what it was doing. A run that is working is the one case worth
// refusing, because its work would be abandoned mid-flight.
func (s State) IsWorking() bool {
	return s == StateStarting || s == StateRunning
}

// IsTerminal reports whether s ends the current execution.
func (s State) IsTerminal() bool {
	switch s {
	case StateCompleted, StateCancelled, StateFailed, StateInterrupted:
		return true
	default:
		return false
	}
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

// Errors returned by AgentRun operations.
var (
	// ErrInvalidTransition reports a state change the domain does not allow.
	ErrInvalidTransition = errors.New("agent: invalid state transition")

	// ErrStaleGeneration reports that a command targets a superseded
	// execution generation.
	ErrStaleGeneration = errors.New("agent: stale execution generation")

	// ErrNotRecoverable reports an attempt to recover a run that is not
	// interrupted.
	ErrNotRecoverable = errors.New("agent: run is not recoverable")
)

// AgentRun is the logical execution of an agent runtime inside a session.
//
// ExecutionGeneration fences logical execution retries and resumes. A new
// generation is created only by an explicit recovery, retry, or resume
// decision; lease expiry alone never starts one.
//
// NodeExecutionID identifies the concrete execution instance on the node and
// is used during start and reconnect reconciliation.
type AgentRun struct {
	ID                  string
	SessionID           string
	AgentID             string
	NodeID              string
	Protocol            string
	RuntimeSessionID    string
	ExecutionGeneration int64
	NodeExecutionID     string
	State               State
	StartedAt           time.Time
	EndedAt             time.Time
}

// New creates a run in the created state at execution generation 1.
func New(id, sessionID, agentID, protocol string) *AgentRun {
	return &AgentRun{
		ID:                  id,
		SessionID:           sessionID,
		AgentID:             agentID,
		Protocol:            protocol,
		ExecutionGeneration: 1,
		State:               StateCreated,
	}
}

// Validate reports whether the run is well formed enough to persist.
func (r *AgentRun) Validate() error {
	switch {
	case r == nil:
		return errors.New("agent: nil run")
	case r.ID == "":
		return errors.New("agent: id is required")
	case r.SessionID == "":
		return fmt.Errorf("agent: run %s: session id is required", r.ID)
	case r.AgentID == "":
		return fmt.Errorf("agent: run %s: agent id is required", r.ID)
	case r.Protocol == "":
		return fmt.Errorf("agent: run %s: protocol is required", r.ID)
	case !r.State.Valid():
		return fmt.Errorf("agent: run %s: unknown state %q", r.ID, r.State)
	case r.ExecutionGeneration < 1:
		return fmt.Errorf("agent: run %s: execution generation must be at least 1", r.ID)
	}
	return nil
}

// IsTerminal reports whether the run has ended.
func (r *AgentRun) IsTerminal() bool { return r.State.IsTerminal() }

// AcceptsPrompt reports whether the run can receive a session.prompt.
//
// v1 has no non-interactive run kind, so a run accepts prompts while it is
// live. A terminal run does not: prompting it is rejected, or routed to a new
// AgentRun. Resuming a terminal run is an explicit operation, not an implicit
// side effect of prompting.
func (r *AgentRun) AcceptsPrompt() bool { return !r.IsTerminal() }

// Transition moves the run to next, validating the transition.
//
// A transition to the current state is a no-op so that retried operations are
// idempotent.
func (r *AgentRun) Transition(next State) error {
	if !r.State.Valid() {
		return fmt.Errorf("%w: current state %q is unknown", ErrInvalidTransition, r.State)
	}
	if !next.Valid() {
		return fmt.Errorf("%w: target state %q is unknown", ErrInvalidTransition, next)
	}
	if r.State == next {
		return nil
	}
	if !r.State.CanTransitionTo(next) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, r.State, next)
	}

	r.State = next
	switch {
	case next == StateRunning:
		if r.StartedAt.IsZero() {
			r.StartedAt = time.Now().UTC()
		}
	case next.IsTerminal():
		r.EndedAt = time.Now().UTC()
	}
	return nil
}

// CheckGeneration fences a command against the current execution generation.
//
// A node rejects commands for an older generation, so a late command from a
// previous coordinator connection cannot be applied to a new execution
// instance after recovery.
func (r *AgentRun) CheckGeneration(generation int64) error {
	if generation != r.ExecutionGeneration {
		return fmt.Errorf("%w: run %s is at generation %d, command carries %d",
			ErrStaleGeneration, r.ID, r.ExecutionGeneration, generation)
	}
	return nil
}

// Recover starts a new execution generation after an explicit recovery,
// retry, or resume decision.
//
// Only an interrupted run can be recovered. Lease expiry alone must not call
// Recover: an expired execution lease classifies the run as interrupted
// without authorizing automatic replacement, because the original execution
// may still be alive and duplicating it could repeat side effects.
//
// The state is assigned directly because interrupted has no Transition edge
// to starting: the generation bump is what makes the move safe.
func (r *AgentRun) Recover(nodeExecutionID string) error {
	if r.State != StateInterrupted {
		return fmt.Errorf("%w: %s", ErrNotRecoverable, r.State)
	}
	r.ExecutionGeneration++
	r.NodeExecutionID = nodeExecutionID
	r.State = StateStarting
	r.StartedAt = time.Time{}
	r.EndedAt = time.Time{}
	return nil
}
