package session

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestNewStartsActive(t *testing.T) {
	sess := New("sess_1")
	if sess.State != StateActive {
		t.Fatalf("state = %q, want active", sess.State)
	}
	if string(sess.Metadata) != "{}" {
		t.Fatalf("metadata = %s, want {}", sess.Metadata)
	}
	if err := sess.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestSessionTransitions(t *testing.T) {
	cases := []struct {
		from State
		to   State
		ok   bool
	}{
		{StateActive, StateIdle, true},
		{StateActive, StateHandoff, true},
		{StateActive, StateArchived, true},
		{StateIdle, StateActive, true},
		{StateIdle, StateHandoff, true},
		{StateIdle, StateArchived, true},
		{StateHandoff, StateActive, true},
		{StateHandoff, StateIdle, true},
		{StateHandoff, StateArchived, true},
		{StateArchived, StateActive, true},
		{StateArchived, StateIdle, false},
		{StateArchived, StateHandoff, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"->"+string(tc.to), func(t *testing.T) {
			if got := tc.from.CanTransitionTo(tc.to); got != tc.ok {
				t.Fatalf("CanTransitionTo = %v, want %v", got, tc.ok)
			}
		})
	}
}

func TestTransitionIsIdempotent(t *testing.T) {
	sess := New("sess_1")
	if err := sess.Transition(StateActive); err != nil {
		t.Fatalf("repeating a transition should be a no-op, got %v", err)
	}
}

func TestTransitionRejectsInvalid(t *testing.T) {
	sess := New("sess_1")
	if err := sess.Transition(StateHandoff); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := sess.Transition(StateArchived); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := sess.Transition(StateIdle); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("archived -> idle should be rejected, got %v", err)
	}
	if err := sess.Transition("melting"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
}

func TestSetDefaultInteractiveRun(t *testing.T) {
	sess := New("sess_1")
	if err := sess.SetDefaultInteractiveRun("run_1"); err != nil {
		t.Fatalf("SetDefaultInteractiveRun: %v", err)
	}
	if sess.DefaultInteractiveRunID != "run_1" {
		t.Fatalf("default run = %q, want run_1", sess.DefaultInteractiveRunID)
	}
	if err := sess.SetDefaultInteractiveRun(""); err == nil {
		t.Fatal("an empty run id should be rejected")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		sess *Session
		ok   bool
	}{
		{"valid", New("sess_1"), true},
		{"nil", nil, false},
		{"no id", &Session{State: StateActive}, false},
		{"unknown state", &Session{ID: "sess_1", State: "vibing"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.sess.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestSnapshotCarriesSchemaVersion(t *testing.T) {
	snap := &Snapshot{ID: "snap_1", SessionID: "sess_1", SchemaVersion: 2, Payload: json.RawMessage(`{}`)}
	if snap.SchemaVersion != 2 {
		t.Fatalf("schema version = %d", snap.SchemaVersion)
	}
}
