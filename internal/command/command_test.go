package command

import (
	"errors"
	"testing"
)

func TestNewStartsReceived(t *testing.T) {
	cmd := New("cmd_1", "slack:U1", "session.create")
	if cmd.State != StateReceived {
		t.Fatalf("state = %q, want received", cmd.State)
	}
	if cmd.IsTerminal() {
		t.Fatal("a new command should not be terminal")
	}
	if !cmd.State.IsPending() {
		t.Fatal("a new command should be pending")
	}
	if err := cmd.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestCommandTransitions(t *testing.T) {
	cases := []struct {
		from State
		to   State
		ok   bool
	}{
		{StateReceived, StateAccepted, true},
		{StateReceived, StateRejected, true},
		{StateReceived, StateFailed, true},
		{StateReceived, StateCompleted, false},
		{StateReceived, StateRetryable, false},
		{StateAccepted, StateCompleted, true},
		{StateAccepted, StateRejected, true},
		{StateAccepted, StateFailed, true},
		{StateAccepted, StateRetryable, true},
		{StateRetryable, StateAccepted, true},
		{StateRetryable, StateCompleted, true},
		{StateRetryable, StateFailed, true},
		{StateCompleted, StateAccepted, false},
		{StateCompleted, StateRetryable, false},
		{StateRejected, StateAccepted, false},
		{StateFailed, StateRetryable, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"->"+string(tc.to), func(t *testing.T) {
			if got := tc.from.CanTransitionTo(tc.to); got != tc.ok {
				t.Fatalf("CanTransitionTo = %v, want %v", got, tc.ok)
			}
		})
	}
}

func TestTerminalAndPendingStates(t *testing.T) {
	for _, s := range []State{StateCompleted, StateRejected, StateFailed} {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
		if s.IsPending() {
			t.Errorf("%s should not be pending", s)
		}
	}
	for _, s := range []State{StateReceived, StateAccepted, StateRetryable} {
		if s.IsTerminal() {
			t.Errorf("%s should not be terminal", s)
		}
		if !s.IsPending() {
			t.Errorf("%s should be pending", s)
		}
	}
	if State("bogus").IsPending() {
		t.Error("an unknown state should not be pending")
	}
}

func TestTransitionIsIdempotent(t *testing.T) {
	cmd := New("cmd_1", "slack:U1", "session.create")
	if err := cmd.Transition(StateAccepted); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := cmd.Transition(StateAccepted); err != nil {
		t.Fatalf("repeating a transition should be a no-op, got %v", err)
	}
}

func TestTransitionRejectsInvalid(t *testing.T) {
	cmd := New("cmd_1", "slack:U1", "session.create")
	if err := cmd.Transition(StateCompleted); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("received -> completed should be rejected, got %v", err)
	}
	if err := cmd.Transition("exploded"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}

	if err := cmd.Transition(StateAccepted); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := cmd.Transition(StateCompleted); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if !cmd.IsTerminal() {
		t.Fatal("a completed command should be terminal")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		cmd  *Command
		ok   bool
	}{
		{"valid", New("cmd_1", "slack:U1", "session.create"), true},
		{"nil", nil, false},
		{"no id", &Command{Method: "m", State: StateReceived}, false},
		{"no method", &Command{ID: "cmd_1", State: StateReceived}, false},
		{"unknown state", &Command{ID: "cmd_1", Method: "m", State: "wandering"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cmd.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}
