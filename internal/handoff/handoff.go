// Package handoff models cross-agent handoff.
//
// A handoff creates a new AgentRun rather than migrating an existing agent
// process. Hive must not attempt to convert one runtime's native session into
// another's: the source run's runtime session id and the target run's runtime
// session id are always distinct fields.
package handoff

import (
	"errors"
	"fmt"
	"time"

	"github.com/thuupx/hive/internal/event"
)

// State is the lifecycle state of a handoff.
type State string

const (
	StateRequested       State = "requested"
	StateContextBuilding State = "context_building"
	StateTargetStarting  State = "target_starting"
	StateActive          State = "active"
	StateFailed          State = "failed"
	StateCancelled       State = "cancelled"
)

// transitions is the allowed state graph.
//
// A handoff is itself a durable operation with an explicit lifecycle, so a crash
// at any point leaves an inspectable state and never silently claims that the
// session was transferred.
var transitions = map[State][]State{
	StateRequested:       {StateContextBuilding, StateFailed, StateCancelled},
	StateContextBuilding: {StateTargetStarting, StateFailed, StateCancelled},
	StateTargetStarting:  {StateActive, StateFailed, StateCancelled},
	StateActive:          nil,
	StateFailed:          nil,
	StateCancelled:       nil,
}

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// IsTerminal reports whether s ends the handoff.
func (s State) IsTerminal() bool {
	switch s {
	case StateActive, StateFailed, StateCancelled:
		return true
	default:
		return false
	}
}

// IsUsable reports whether the handoff actually transferred the session.
//
// Only an active handoff counts. A failed handoff must never be reported as a
// successful transfer, even when the target AgentRun exists.
func (s State) IsUsable() bool { return s == StateActive }

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
var ErrInvalidTransition = errors.New("handoff: invalid state transition")

// Handoff is the durable record of a transfer between agents.
type Handoff struct {
	ID          string
	SessionID   string
	SourceRunID string
	TargetRunID string
	State       State
	Summary     string

	// ContextSnapshotID identifies the context package handed to the target.
	ContextSnapshotID string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// New creates a handoff in the requested state.
func New(id, sessionID, sourceRunID, targetRunID string) *Handoff {
	now := time.Now().UTC()
	return &Handoff{
		ID:          id,
		SessionID:   sessionID,
		SourceRunID: sourceRunID,
		TargetRunID: targetRunID,
		State:       StateRequested,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// Validate reports whether the handoff is well formed enough to persist.
func (h *Handoff) Validate() error {
	switch {
	case h == nil:
		return errors.New("handoff: nil handoff")
	case h.ID == "":
		return errors.New("handoff: id is required")
	case h.SessionID == "":
		return fmt.Errorf("handoff: %s: session id is required", h.ID)
	case h.TargetRunID == "":
		return fmt.Errorf("handoff: %s: target run id is required", h.ID)
	case !h.State.Valid():
		return fmt.Errorf("handoff: %s: unknown state %q", h.ID, h.State)
	}
	return nil
}

// Transition moves the handoff to next, validating the transition.
func (h *Handoff) Transition(next State) error {
	if !h.State.Valid() {
		return fmt.Errorf("%w: current state %q is unknown", ErrInvalidTransition, h.State)
	}
	if !next.Valid() {
		return fmt.Errorf("%w: target state %q is unknown", ErrInvalidTransition, next)
	}
	if h.State == next {
		return nil
	}
	if !h.State.CanTransitionTo(next) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, h.State, next)
	}
	h.State = next
	h.UpdatedAt = time.Now().UTC()
	return nil
}

// Context is the package handed to the target agent.
//
// The first implementation keeps it explicit and deterministic. Token budgeting,
// relevance selection, summarization, compression, and artifact resolution are
// extensions, not prerequisites.
//
// Summary is optional: a handoff must remain valid when the source agent cannot
// generate one.
type Context struct {
	SessionID string
	Summary   string

	// RecentEvents is the tail of the session stream, so the target sees what
	// just happened without replaying unbounded history.
	RecentEvents []*event.Event

	// PriorMessages are the user-visible turns, which a new agent needs more than
	// the raw protocol traffic.
	PriorMessages []string

	Workspace WorkspaceRef
	Artifacts []ResourceRef
}

// WorkspaceRef identifies where the target should work.
type WorkspaceRef struct {
	WorkspaceID string
	NodeID      string
	Path        string
}

// ResourceRef identifies an artifact the target may need.
type ResourceRef struct {
	URI      string
	MimeType string
}

// IsEmpty reports whether the context carries nothing at all.
//
// An empty context is still a valid handoff: the target starts fresh inside the
// same session, which is better than refusing to transfer.
func (c Context) IsEmpty() bool {
	return c.Summary == "" && len(c.RecentEvents) == 0 &&
		len(c.PriorMessages) == 0 && len(c.Artifacts) == 0
}
