// Package permission relays agent permission requests.
//
// Hive does not implement a tool-permission engine: the underlying agent owns
// tool authorization semantics. Hive treats a permission request as opaque
// protocol data except for the metadata required to route, expire, authorize,
// and correlate the response.
package permission

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// State is the lifecycle state of a permission request.
type State string

const (
	StatePending   State = "pending"
	StateApproved  State = "approved"
	StateDenied    State = "denied"
	StateExpired   State = "expired"
	StateCancelled State = "cancelled"
)

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	switch s {
	case StatePending, StateApproved, StateDenied, StateExpired, StateCancelled:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether s is a resolved state.
//
// A permission request is a one-time state transition. Once resolved it cannot
// transition to another terminal state.
func (s State) IsTerminal() bool { return s.Valid() && s != StatePending }

// ErrInvalidTransition reports a state change the domain does not allow.
var ErrInvalidTransition = errors.New("permission: invalid state transition")

// Request is a pending or resolved permission request.
//
// Payload is the agent's request, preserved as opaque protocol data. Hive
// routes and correlates it but does not interpret it.
type Request struct {
	ID             string
	SessionID      string
	RunID          string
	AgentRequestID string
	Payload        json.RawMessage
	State          State
	ExpiresAt      time.Time
	CreatedAt      time.Time
	ResolvedAt     time.Time
}

// New creates a pending permission request.
//
// A zero expiresAt means the request never times out on its own.
func New(id, sessionID, runID, agentRequestID string, payload json.RawMessage, expiresAt time.Time) *Request {
	return &Request{
		ID:             id,
		SessionID:      sessionID,
		RunID:          runID,
		AgentRequestID: agentRequestID,
		Payload:        payload,
		State:          StatePending,
		ExpiresAt:      expiresAt,
		CreatedAt:      time.Now().UTC(),
	}
}

// Validate reports whether the request is well formed enough to persist.
func (r *Request) Validate() error {
	switch {
	case r == nil:
		return errors.New("permission: nil request")
	case r.ID == "":
		return errors.New("permission: id is required")
	case r.SessionID == "":
		return fmt.Errorf("permission: %s: session id is required", r.ID)
	case r.AgentRequestID == "":
		return fmt.Errorf("permission: %s: agent request id is required", r.ID)
	case !r.State.Valid():
		return fmt.Errorf("permission: %s: unknown state %q", r.ID, r.State)
	}
	return nil
}

// IsPending reports whether the request still awaits a decision.
func (r *Request) IsPending() bool { return r.State == StatePending }

// IsExpired reports whether the deadline has passed at time at.
func (r *Request) IsExpired(at time.Time) bool {
	return !r.ExpiresAt.IsZero() && !at.Before(r.ExpiresAt)
}

// Resolve applies a terminal transition and reports whether this call won.
//
// Resolution is a one-time transition with first-wins semantics. A request
// that is already resolved returns false and is left untouched, so repeating
// the same response cannot produce duplicate authorization and a later
// response simply observes the resolved state.
//
// Storage performs the same transition as an atomic compare-and-set, which is
// what makes concurrent responses safe across processes. This method is the
// in-memory form of that rule.
func (r *Request) Resolve(next State) (bool, error) {
	if !r.State.Valid() {
		return false, fmt.Errorf("%w: current state %q is unknown", ErrInvalidTransition, r.State)
	}
	if !next.Valid() {
		return false, fmt.Errorf("%w: target state %q is unknown", ErrInvalidTransition, next)
	}
	if !next.IsTerminal() {
		return false, fmt.Errorf("%w: %s is not a terminal state", ErrInvalidTransition, next)
	}
	if r.State != StatePending {
		return false, nil
	}
	r.State = next
	r.ResolvedAt = time.Now().UTC()
	return true, nil
}

// Expire applies the timeout terminal state, and only when the deadline has
// actually passed.
//
// Timeout is a safe terminal state. A pending request must never become an
// implicit approval, including when the coordinator connection is lost.
func (r *Request) Expire(at time.Time) (bool, error) {
	if !r.IsExpired(at) {
		return false, fmt.Errorf("permission: %s has not expired", r.ID)
	}
	return r.Resolve(StateExpired)
}

// Decision is a resolved outcome a caller may apply.
type Decision struct {
	// Approved reports whether the user authorized the operation.
	Approved bool

	// OptionID optionally names the specific choice the user made.
	//
	// A transport that renders the agent's own options sets this so the
	// adapter can answer with exactly what the agent offered. It is opaque
	// correlation data, like the request payload itself.
	OptionID string
}

// StateOf maps a decision to its terminal state.
func StateOf(d Decision) State {
	if d.Approved {
		return StateApproved
	}
	return StateDenied
}
