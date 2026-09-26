package control

import (
	"testing"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// A session has one turn at a time: a second prompt waits behind the first.
func TestASecondPromptQueuesBehindTheRunningTurn(t *testing.T) {
	s := &Service{turns: map[string]*turnQueue{}}

	first := v1.SessionPromptParams{SessionID: "sess_1", CommandID: "cmd_1", Text: "one"}
	second := v1.SessionPromptParams{SessionID: "sess_1", CommandID: "cmd_2", Text: "two"}

	if running, _ := s.claimTurn("sess_1", Principal("slack:U1"), first); !running {
		t.Fatal("the first prompt should run")
	}
	running, position := s.claimTurn("sess_1", Principal("slack:U1"), second)
	if running {
		t.Fatal("the second prompt should not run while the first is")
	}
	if position != 1 {
		t.Fatalf("position = %d, want 1", position)
	}

	// The slot is handed to the queued prompt, in order.
	next, ok := s.takeQueued("sess_1")
	if !ok || next.params.CommandID != "cmd_2" {
		t.Fatalf("next = %+v, want the queued prompt", next)
	}
	if _, ok := s.takeQueued("sess_1"); ok {
		t.Fatal("nothing else was queued")
	}

	// Nothing is waiting, so the slot is free again.
	s.releaseTurn("sess_1")
	if running, _ := s.claimTurn("sess_1", Principal("slack:U1"),
		v1.SessionPromptParams{SessionID: "sess_1", CommandID: "cmd_3"}); !running {
		t.Fatal("the slot should be free once the queue is empty")
	}
}

// Queued prompts run in the order they arrived.
func TestQueuedPromptsKeepTheirOrder(t *testing.T) {
	s := &Service{turns: map[string]*turnQueue{}}

	s.claimTurn("sess_1", Principal("slack:U1"), v1.SessionPromptParams{SessionID: "sess_1", CommandID: "cmd_1"})
	for _, id := range []string{"cmd_2", "cmd_3", "cmd_4"} {
		s.claimTurn("sess_1", Principal("slack:U1"), v1.SessionPromptParams{SessionID: "sess_1", CommandID: id})
	}

	for _, want := range []string{"cmd_2", "cmd_3", "cmd_4"} {
		next, ok := s.takeQueued("sess_1")
		if !ok || next.params.CommandID != want {
			t.Fatalf("next = %+v, want %s", next, want)
		}
	}
}

// A repeated command is the same turn, so a retry does not run the prompt twice.
func TestARepeatedCommandIsNotQueuedTwice(t *testing.T) {
	s := &Service{turns: map[string]*turnQueue{}}

	repeat := v1.SessionPromptParams{SessionID: "sess_1", CommandID: "cmd_2"}
	s.claimTurn("sess_1", Principal("slack:U1"), v1.SessionPromptParams{SessionID: "sess_1", CommandID: "cmd_1"})
	s.claimTurn("sess_1", Principal("slack:U1"), repeat)
	if running, _ := s.claimTurn("sess_1", Principal("slack:U1"), repeat); running {
		t.Fatal("a repeated command should not run")
	}

	next, ok := s.takeQueued("sess_1")
	if !ok || next.params.CommandID != "cmd_2" {
		t.Fatalf("next = %+v", next)
	}
	if _, ok := s.takeQueued("sess_1"); ok {
		t.Fatal("the repeat was queued twice")
	}
}

// A session that goes idle keeps no state.
func TestAnIdleSessionIsForgotten(t *testing.T) {
	s := &Service{turns: map[string]*turnQueue{}}

	s.claimTurn("sess_1", Principal("slack:U1"), v1.SessionPromptParams{SessionID: "sess_1", CommandID: "cmd_1"})
	s.releaseTurn("sess_1")

	if len(s.turns) != 0 {
		t.Fatalf("turns = %v, want none", s.turns)
	}
}

// One session's turn does not block another's.
func TestSessionsDoNotBlockEachOther(t *testing.T) {
	s := &Service{turns: map[string]*turnQueue{}}

	if running, _ := s.claimTurn("sess_1", Principal("slack:U1"),
		v1.SessionPromptParams{SessionID: "sess_1", CommandID: "cmd_1"}); !running {
		t.Fatal("the first session's prompt should run")
	}
	if running, _ := s.claimTurn("sess_2", Principal("slack:U2"),
		v1.SessionPromptParams{SessionID: "sess_2", CommandID: "cmd_2"}); !running {
		t.Fatal("another session's prompt should run")
	}
}
