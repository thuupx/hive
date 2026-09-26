package handoff

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/thuupx/hive/internal/event"
)

func TestNewStartsRequested(t *testing.T) {
	h := New("h1", "sess_1", "run_1", "run_2")
	if h.State != StateRequested {
		t.Fatalf("state = %q, want requested", h.State)
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestLifecycleTransitions(t *testing.T) {
	cases := []struct {
		from State
		to   State
		ok   bool
	}{
		{StateRequested, StateContextBuilding, true},
		{StateRequested, StateTargetStarting, false},
		{StateRequested, StateActive, false},
		{StateContextBuilding, StateTargetStarting, true},
		{StateContextBuilding, StateActive, false},
		{StateTargetStarting, StateActive, true},
		{StateTargetStarting, StateFailed, true},
		{StateActive, StateFailed, false},
		{StateFailed, StateActive, false},
		{StateCancelled, StateActive, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"->"+string(tc.to), func(t *testing.T) {
			if got := tc.from.CanTransitionTo(tc.to); got != tc.ok {
				t.Fatalf("CanTransitionTo = %v, want %v", got, tc.ok)
			}
		})
	}
}

// Only an active handoff is a real transfer. A failed one must never be reported
// as success, even though the target AgentRun exists.
func TestOnlyActiveIsUsable(t *testing.T) {
	if !StateActive.IsUsable() {
		t.Error("an active handoff is a transfer")
	}
	for _, state := range []State{StateRequested, StateContextBuilding, StateTargetStarting, StateFailed, StateCancelled} {
		if state.IsUsable() {
			t.Errorf("%s must not count as a transfer", state)
		}
	}
}

func TestTransitionRejectsUnknownStates(t *testing.T) {
	h := New("h1", "sess_1", "run_1", "run_2")

	if err := h.Transition("teleporting"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if err := h.Transition(StateActive); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("requested -> active skips the lifecycle: %v", err)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		h    *Handoff
		ok   bool
	}{
		{"valid", New("h1", "sess_1", "run_1", "run_2"), true},
		{"nil", nil, false},
		{"no id", &Handoff{SessionID: "s", TargetRunID: "r", State: StateRequested}, false},
		{"no session", &Handoff{ID: "h", TargetRunID: "r", State: StateRequested}, false},
		{"no target", &Handoff{ID: "h", SessionID: "s", State: StateRequested}, false},
		{"unknown state", &Handoff{ID: "h", SessionID: "s", TargetRunID: "r", State: "wandering"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.h.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

// An empty context is still a valid handoff: the target starts fresh inside the
// same session, which is better than refusing to transfer.
func TestEmptyContextIsValid(t *testing.T) {
	if !(Context{SessionID: "sess_1"}).IsEmpty() {
		t.Error("a context with nothing in it should report empty")
	}

	full := Context{
		SessionID:     "sess_1",
		Summary:       "design done",
		PriorMessages: []string{"do the thing"},
	}
	if full.IsEmpty() {
		t.Error("a context with content should not report empty")
	}
}

func TestContextCarriesTheRawEvents(t *testing.T) {
	ctx := Context{
		SessionID: "sess_1",
		RecentEvents: []*event.Event{{
			ID:      "ev_1",
			Type:    event.TypeMessage,
			Payload: json.RawMessage(`{"text":"hello"}`),
		}},
	}

	encoded, err := json.Marshal(ctx)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Context
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.RecentEvents) != 1 || decoded.RecentEvents[0].ID != "ev_1" {
		t.Fatalf("recent events = %+v", decoded.RecentEvents)
	}
}
