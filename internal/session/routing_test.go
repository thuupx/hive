package session

import (
	"errors"
	"testing"

	"github.com/thupham/hive/internal/agent"
)

func lookupOf(runs ...*agent.AgentRun) func(string) (*agent.AgentRun, bool) {
	byID := make(map[string]*agent.AgentRun, len(runs))
	for _, r := range runs {
		byID[r.ID] = r
	}
	return func(id string) (*agent.AgentRun, bool) {
		r, ok := byID[id]
		return r, ok
	}
}

func liveRun(id string) *agent.AgentRun {
	run := agent.New(id, "sess_1", "devin", "acp")
	for _, next := range []agent.State{agent.StateStarting, agent.StateRunning} {
		if err := run.Transition(next); err != nil {
			panic(err)
		}
	}
	return run
}

func terminalRun(id string) *agent.AgentRun {
	run := liveRun(id)
	if err := run.Transition(agent.StateCompleted); err != nil {
		panic(err)
	}
	return run
}

func TestRoutePromptTargetsExplicitRun(t *testing.T) {
	sess := New("sess_1")
	run := liveRun("run_1")

	target, err := RoutePrompt("run_1", sess, lookupOf(run))
	if err != nil {
		t.Fatalf("RoutePrompt: %v", err)
	}
	if target.RunID != "run_1" || target.NewRun {
		t.Fatalf("target = %+v", target)
	}
}

func TestRoutePromptExplicitRunOverridesDefault(t *testing.T) {
	sess := New("sess_1")
	if err := sess.SetDefaultInteractiveRun("run_1"); err != nil {
		t.Fatalf("SetDefaultInteractiveRun: %v", err)
	}

	target, err := RoutePrompt("run_2", sess, lookupOf(liveRun("run_1"), liveRun("run_2")))
	if err != nil {
		t.Fatalf("RoutePrompt: %v", err)
	}
	if target.RunID != "run_2" {
		t.Fatalf("target = %+v, want run_2", target)
	}
}

func TestRoutePromptRejectsUnknownExplicitRun(t *testing.T) {
	_, err := RoutePrompt("run_missing", New("sess_1"), lookupOf())
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
}

// An explicit target is never silently redirected: prompting a different run
// than the caller named would be a surprising side effect.
func TestRoutePromptRejectsTerminalExplicitRun(t *testing.T) {
	_, err := RoutePrompt("run_1", New("sess_1"), lookupOf(terminalRun("run_1")))
	if !errors.Is(err, ErrRunTerminated) {
		t.Fatalf("err = %v, want ErrRunTerminated", err)
	}
}

func TestRoutePromptUsesDefaultRun(t *testing.T) {
	sess := New("sess_1")
	if err := sess.SetDefaultInteractiveRun("run_1"); err != nil {
		t.Fatalf("SetDefaultInteractiveRun: %v", err)
	}

	target, err := RoutePrompt("", sess, lookupOf(liveRun("run_1")))
	if err != nil {
		t.Fatalf("RoutePrompt: %v", err)
	}
	if target.RunID != "run_1" || target.NewRun {
		t.Fatalf("target = %+v", target)
	}
}

// A prompt to a terminal default run is new user intent, not the automatic
// replacement of execution that §11.2 forbids.
func TestRoutePromptCreatesNewRunWhenDefaultIsTerminal(t *testing.T) {
	sess := New("sess_1")
	if err := sess.SetDefaultInteractiveRun("run_1"); err != nil {
		t.Fatalf("SetDefaultInteractiveRun: %v", err)
	}

	target, err := RoutePrompt("", sess, lookupOf(terminalRun("run_1")))
	if err != nil {
		t.Fatalf("RoutePrompt: %v", err)
	}
	if !target.NewRun || target.RunID != "" {
		t.Fatalf("target = %+v, want a new run", target)
	}
}

func TestRoutePromptCreatesNewRunWhenDefaultIsDangling(t *testing.T) {
	sess := New("sess_1")
	if err := sess.SetDefaultInteractiveRun("run_gone"); err != nil {
		t.Fatalf("SetDefaultInteractiveRun: %v", err)
	}

	target, err := RoutePrompt("", sess, lookupOf())
	if err != nil {
		t.Fatalf("RoutePrompt: %v", err)
	}
	if !target.NewRun {
		t.Fatalf("target = %+v, want a new run", target)
	}
}

func TestRoutePromptCreatesNewRunWhenNoDefault(t *testing.T) {
	target, err := RoutePrompt("", New("sess_1"), lookupOf())
	if err != nil {
		t.Fatalf("RoutePrompt: %v", err)
	}
	if !target.NewRun {
		t.Fatalf("target = %+v, want a new run", target)
	}
}

func TestRoutePromptRejectsArchivedSession(t *testing.T) {
	sess := New("sess_1")
	if err := sess.Transition(StateArchived); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	_, err := RoutePrompt("", sess, lookupOf(liveRun("run_1")))
	if !errors.Is(err, ErrArchived) {
		t.Fatalf("err = %v, want ErrArchived", err)
	}
}

func TestRoutePromptIsDeterministic(t *testing.T) {
	sess := New("sess_1")
	if err := sess.SetDefaultInteractiveRun("run_2"); err != nil {
		t.Fatalf("SetDefaultInteractiveRun: %v", err)
	}
	// run_1 was created first and is also live. Routing must follow the
	// session pointer, not creation order or timestamps.
	runs := lookupOf(liveRun("run_1"), liveRun("run_2"))

	for i := 0; i < 5; i++ {
		target, err := RoutePrompt("", sess, runs)
		if err != nil {
			t.Fatalf("RoutePrompt: %v", err)
		}
		if target.RunID != "run_2" {
			t.Fatalf("iteration %d target = %+v, want run_2", i, target)
		}
	}
}

func TestRoutePromptRejectsNilInputs(t *testing.T) {
	if _, err := RoutePrompt("", nil, lookupOf()); err == nil {
		t.Fatal("a nil session should be rejected")
	}
	if _, err := RoutePrompt("", New("sess_1"), nil); err == nil {
		t.Fatal("a nil lookup should be rejected")
	}
}
