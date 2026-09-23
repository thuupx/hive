package agent

import (
	"errors"
	"testing"
)

func TestStateTransitions(t *testing.T) {
	cases := []struct {
		from State
		to   State
		ok   bool
	}{
		{StateCreated, StateQueued, true},
		{StateCreated, StateStarting, true},
		{StateCreated, StateCancelled, true},
		{StateCreated, StateFailed, true},
		{StateCreated, StateRunning, false},
		{StateCreated, StateCompleted, false},
		{StateQueued, StateStarting, true},
		{StateQueued, StateFailed, true},
		{StateQueued, StateRunning, false},
		{StateStarting, StateRunning, true},
		{StateStarting, StateCompleted, true},
		{StateStarting, StateInterrupted, true},
		{StateStarting, StateCreated, false},
		{StateRunning, StateCompleted, true},
		{StateRunning, StateFailed, true},
		{StateRunning, StateCancelled, true},
		{StateRunning, StateInterrupted, true},
		{StateRunning, StateStarting, false},
		{StateCompleted, StateRunning, false},
		{StateCompleted, StateFailed, false},
		{StateFailed, StateRunning, false},
		// interrupted has no outgoing edge: recovery must go through Recover.
		{StateInterrupted, StateStarting, false},
		{StateInterrupted, StateRunning, false},
		{StateInterrupted, StateCompleted, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"->"+string(tc.to), func(t *testing.T) {
			if got := tc.from.CanTransitionTo(tc.to); got != tc.ok {
				t.Fatalf("CanTransitionTo = %v, want %v", got, tc.ok)
			}
		})
	}
}

func TestTerminalStates(t *testing.T) {
	terminal := []State{StateCompleted, StateCancelled, StateFailed, StateInterrupted}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	live := []State{StateCreated, StateQueued, StateStarting, StateRunning}
	for _, s := range live {
		if s.IsTerminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

func TestNewStartsCreated(t *testing.T) {
	run := New("run_1", "sess_1", "devin", "acp")
	if run.State != StateCreated {
		t.Fatalf("state = %q, want created", run.State)
	}
	if run.ExecutionGeneration != 1 {
		t.Fatalf("generation = %d, want 1", run.ExecutionGeneration)
	}
	if err := run.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestTransitionTracksTimestamps(t *testing.T) {
	run := New("run_1", "sess_1", "devin", "acp")

	if err := run.Transition(StateRunning); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("created -> running should be rejected, got %v", err)
	}
	if err := run.Transition(StateStarting); err != nil {
		t.Fatalf("created -> starting: %v", err)
	}
	if err := run.Transition(StateRunning); err != nil {
		t.Fatalf("starting -> running: %v", err)
	}
	if run.StartedAt.IsZero() {
		t.Error("StartedAt should be set when the run reaches running")
	}
	if !run.EndedAt.IsZero() {
		t.Error("EndedAt should be unset while the run is live")
	}
	if err := run.Transition(StateCompleted); err != nil {
		t.Fatalf("running -> completed: %v", err)
	}
	if run.EndedAt.IsZero() {
		t.Error("EndedAt should be set when the run becomes terminal")
	}
}

func TestTransitionIsIdempotent(t *testing.T) {
	run := New("run_1", "sess_1", "devin", "acp")
	if err := run.Transition(StateStarting); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := run.Transition(StateStarting); err != nil {
		t.Fatalf("repeating a transition should be a no-op, got %v", err)
	}
}

func TestTransitionRejectsUnknownStates(t *testing.T) {
	run := New("run_1", "sess_1", "devin", "acp")
	if err := run.Transition("teleporting"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}

	empty := &AgentRun{ID: "run_2"}
	if err := empty.Transition(StateStarting); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition for an unknown current state", err)
	}
}

func TestAcceptsPrompt(t *testing.T) {
	run := New("run_1", "sess_1", "devin", "acp")
	if !run.AcceptsPrompt() {
		t.Error("a live run should accept prompts")
	}
	for _, next := range []State{StateStarting, StateRunning} {
		if err := run.Transition(next); err != nil {
			t.Fatalf("transition to %s: %v", next, err)
		}
	}
	if !run.AcceptsPrompt() {
		t.Error("a running run should accept prompts")
	}
	if err := run.Transition(StateCompleted); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if run.AcceptsPrompt() {
		t.Error("a terminal run should not accept prompts")
	}
}

func TestCheckGeneration(t *testing.T) {
	run := New("run_1", "sess_1", "devin", "acp")
	if err := run.CheckGeneration(1); err != nil {
		t.Fatalf("CheckGeneration(1): %v", err)
	}
	if err := run.CheckGeneration(2); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("err = %v, want ErrStaleGeneration", err)
	}
}

func TestRecoverRequiresInterrupted(t *testing.T) {
	run := New("run_1", "sess_1", "devin", "acp")
	if err := run.Transition(StateStarting); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := run.Recover("node_exec_2"); !errors.Is(err, ErrNotRecoverable) {
		t.Fatalf("err = %v, want ErrNotRecoverable", err)
	}
	if run.ExecutionGeneration != 1 {
		t.Fatalf("generation = %d, want 1 after a rejected recovery", run.ExecutionGeneration)
	}
}

func TestRecoverBumpsGenerationAndFencesTheOldOne(t *testing.T) {
	run := New("run_1", "sess_1", "devin", "acp")
	for _, next := range []State{StateStarting, StateRunning, StateInterrupted} {
		if err := run.Transition(next); err != nil {
			t.Fatalf("transition to %s: %v", next, err)
		}
	}

	if err := run.Recover("node_exec_2"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if run.ExecutionGeneration != 2 {
		t.Fatalf("generation = %d, want 2", run.ExecutionGeneration)
	}
	if run.State != StateStarting {
		t.Fatalf("state = %q, want starting", run.State)
	}
	if run.NodeExecutionID != "node_exec_2" {
		t.Fatalf("node execution id = %q", run.NodeExecutionID)
	}
	if !run.EndedAt.IsZero() {
		t.Error("EndedAt should be cleared for the new generation")
	}
	if err := run.CheckGeneration(1); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("the previous generation was not fenced: %v", err)
	}
	if err := run.CheckGeneration(2); err != nil {
		t.Fatalf("CheckGeneration(2): %v", err)
	}
}

func TestValidate(t *testing.T) {
	valid := New("run_1", "sess_1", "devin", "acp")

	cases := []struct {
		name string
		run  *AgentRun
		ok   bool
	}{
		{"valid", valid, true},
		{"nil", nil, false},
		{"no id", &AgentRun{SessionID: "s", AgentID: "a", Protocol: "acp", State: StateCreated, ExecutionGeneration: 1}, false},
		{"no session", &AgentRun{ID: "r", AgentID: "a", Protocol: "acp", State: StateCreated, ExecutionGeneration: 1}, false},
		{"no agent", &AgentRun{ID: "r", SessionID: "s", Protocol: "acp", State: StateCreated, ExecutionGeneration: 1}, false},
		{"no protocol", &AgentRun{ID: "r", SessionID: "s", AgentID: "a", State: StateCreated, ExecutionGeneration: 1}, false},
		{"unknown state", &AgentRun{ID: "r", SessionID: "s", AgentID: "a", Protocol: "acp", State: "limbo", ExecutionGeneration: 1}, false},
		{"zero generation", &AgentRun{ID: "r", SessionID: "s", AgentID: "a", Protocol: "acp", State: StateCreated}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}
