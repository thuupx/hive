package control

import (
	"context"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// A session has one turn at a time.
//
// A prompt that arrives while a turn is running waits behind it rather than
// racing into the same agent session. Two prompts in flight are answered by the
// agent at once, and the two answers interleave into one — which reads as a
// garbled reply rather than as two.
type turnQueue struct {
	// running is the command id of the turn in flight, empty when none is.
	running string

	// waiting are the prompts taken while it ran, in the order they arrived.
	waiting []waitingTurn

	// known holds the command ids currently running or waiting, so a repeated
	// command is the same turn rather than a second one.
	known map[string]bool
}

// waitingTurn is a prompt held until the session's turn slot is free.
type waitingTurn struct {
	principal Principal
	params    v1.SessionPromptParams
}

// claimTurn reserves a session's turn slot.
//
// It reports whether the caller may run its turn now. When it may not, the prompt
// has been queued behind the running one, and its place in the queue is returned.
func (s *Service) claimTurn(sessionID string, principal Principal, params v1.SessionPromptParams) (bool, int) {
	s.turnsMu.Lock()
	defer s.turnsMu.Unlock()

	q := s.turns[sessionID]
	if q == nil {
		q = &turnQueue{known: map[string]bool{}}
		s.turns[sessionID] = q
	}

	// A command id that is already running or waiting is the same turn, so a
	// caller's retry does not run the prompt twice.
	if q.known[params.CommandID] {
		return false, len(q.waiting)
	}
	q.known[params.CommandID] = true

	if q.running == "" {
		q.running = params.CommandID
		return true, 0
	}

	q.waiting = append(q.waiting, waitingTurn{principal: principal, params: params})
	return false, len(q.waiting)
}

// takeQueued hands the session's turn slot to the next waiting prompt.
//
// The slot is handed over rather than released and re-taken: a prompt arriving in
// between would otherwise jump the queue.
func (s *Service) takeQueued(sessionID string) (waitingTurn, bool) {
	s.turnsMu.Lock()
	defer s.turnsMu.Unlock()

	q := s.turns[sessionID]
	if q == nil || len(q.waiting) == 0 {
		return waitingTurn{}, false
	}

	next := q.waiting[0]
	q.waiting = q.waiting[1:]
	delete(q.known, q.running)
	q.running = next.params.CommandID
	return next, true
}

// releaseTurn frees a session's turn slot, when nothing is waiting for it.
func (s *Service) releaseTurn(sessionID string) {
	s.turnsMu.Lock()
	defer s.turnsMu.Unlock()

	q := s.turns[sessionID]
	if q == nil || len(q.waiting) > 0 {
		return
	}
	delete(q.known, q.running)
	delete(s.turns, sessionID)
}

// queuedTimeout bounds starting a turn that was queued.
//
// Starting a turn includes launching the agent, which is why it is generous.
const queuedTimeout = TurnTimeout

// drainTurn runs the next prompt queued behind a finished turn.
//
// A queued prompt is routed afresh rather than sent to the run it was taken for:
// a run completes at the end of its turn, so the conversation continues in a new
// one. That is also why the prompt is only accepted here — routing fixes the
// command's target when it is accepted, and the target cannot be known while the
// turn in front of it is still running.
func (s *Service) drainTurn(sessionID string) {
	for {
		next, ok := s.takeQueued(sessionID)
		if !ok {
			s.releaseTurn(sessionID)
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), queuedTimeout)
		sess, err := s.store.GetSession(ctx, sessionID)
		if err == nil {
			_, err = s.startTurn(ctx, sess, next.principal, next.params)
		}
		cancel()

		if err != nil {
			// The queued prompt was never accepted, so there is no durable record
			// of it to fail: saying so is all that is left.
			s.log.Warn("a queued turn could not be started",
				"session", sessionID, "command", next.params.CommandID, "error", err)
			continue
		}
		return
	}
}
