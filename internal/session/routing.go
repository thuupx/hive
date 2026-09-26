package session

import (
	"errors"
	"fmt"

	"github.com/thuupx/hive/internal/agent"
)

// Errors returned by prompt routing.
var (
	// ErrRunNotFound reports that an explicitly targeted AgentRun does not
	// exist.
	ErrRunNotFound = errors.New("session: agent run not found")

	// ErrRunTerminated reports that an explicitly targeted AgentRun can no
	// longer accept prompts.
	ErrRunTerminated = errors.New("session: agent run no longer accepts prompts")

	// ErrArchived reports that the session no longer accepts prompts.
	ErrArchived = errors.New("session: archived")
)

// Target is the resolved destination of a session.prompt.
type Target struct {
	// RunID is the AgentRun the prompt must be delivered to. It is empty
	// when NewRun is set.
	RunID string

	// NewRun reports that the prompt must create a new AgentRun.
	NewRun bool
}

// RoutePrompt resolves which AgentRun a prompt targets.
//
// The rule is deterministic and never depends on timestamps:
//
//	explicit run_id
//	  > session.DefaultInteractiveRunID, when it names a run that accepts prompts
//	  > creating a new AgentRun
//
// A default run that is missing or terminal leads to a new AgentRun: a prompt
// is new user intent, not the automatic replacement of execution that §11.2
// forbids. An explicit run_id is never silently redirected, because prompting
// a different run than the caller named would be a surprising side effect.
//
// lookup returns the run with the given id. It is a parameter so routing can
// be resolved without depending on a repository.
func RoutePrompt(explicitRunID string, sess *Session, lookup func(id string) (*agent.AgentRun, bool)) (Target, error) {
	if sess == nil {
		return Target{}, errors.New("session: nil session")
	}
	if lookup == nil {
		return Target{}, errors.New("session: nil run lookup")
	}
	if sess.State == StateArchived {
		return Target{}, fmt.Errorf("%w: %s", ErrArchived, sess.ID)
	}

	if explicitRunID != "" {
		run, ok := lookup(explicitRunID)
		if !ok {
			return Target{}, fmt.Errorf("%w: %s", ErrRunNotFound, explicitRunID)
		}
		if !run.AcceptsPrompt() {
			return Target{}, fmt.Errorf("%w: %s is %s", ErrRunTerminated, run.ID, run.State)
		}
		return Target{RunID: run.ID}, nil
	}

	if id := sess.DefaultInteractiveRunID; id != "" {
		if run, ok := lookup(id); ok && run.AcceptsPrompt() {
			return Target{RunID: run.ID}, nil
		}
	}
	return Target{NewRun: true}, nil
}
