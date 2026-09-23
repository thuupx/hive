package control

import (
	"context"
	"encoding/json"

	"github.com/thupham/hive/internal/agent"
	"github.com/thupham/hive/internal/command"
	"github.com/thupham/hive/internal/event"
	"github.com/thupham/hive/internal/handoff"
	"github.com/thupham/hive/internal/ids"
	"github.com/thupham/hive/internal/session"
	"github.com/thupham/hive/internal/storage"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// RecentHandoffEvents bounds how much of the session stream travels with a
// handoff. Replaying unbounded history is not the point; the target needs what
// just happened.
const RecentHandoffEvents = 50

// Handoff transfers a session to another agent.
//
// The handoff record and the target AgentRun commit in one transaction, so a
// retry cannot create a second handoff. Everything after that is a sequence of
// explicit lifecycle steps, because a handoff that crosses a failure must leave
// an inspectable state rather than claim the session was transferred.
func (s *Service) Handoff(ctx context.Context, principal Principal, params v1.SessionHandoffParams) (*v1.SessionHandoffResult, error) {
	if err := s.policy.Authorize(principal, v1.MethodSessionHandoff); err != nil {
		return nil, err
	}
	switch {
	case params.CommandID == "":
		return nil, v1.InvalidParams("commandId is required")
	case params.SessionID == "":
		return nil, v1.InvalidParams("sessionId is required")
	case params.AgentID == "":
		return nil, v1.InvalidParams("agentId is required")
	}
	if err := s.authorizeSession(ctx, principal, params.SessionID); err != nil {
		return nil, err
	}
	if !s.hasAgent(params.AgentID) {
		return nil, v1.InvalidParams("unknown agent %q", params.AgentID)
	}

	sess, err := s.store.GetSession(ctx, params.SessionID)
	if err != nil {
		return nil, storageError(err)
	}

	sourceRun, err := s.sourceRunFor(ctx, sess, params.RunID)
	if err != nil {
		return nil, err
	}

	// A node must be able to run the target before anything is written.
	targetNode, err := s.nodeForAgent(params.AgentID)
	if err != nil {
		return nil, err
	}

	targetRun := agent.New(ids.New("run"), sess.ID, params.AgentID, protocolForAgent())
	targetRun.NodeID = targetNode.NodeID

	record := handoff.New(ids.New("handoff"), sess.ID, sourceRun.ID, targetRun.ID)
	record.Summary = params.Summary

	cmd := command.New(params.CommandID, string(principal), v1.MethodSessionHandoff)
	cmd.SourceID = params.SourceID
	cmd.Target = sess.ID
	cmd.State = command.StateAccepted

	stored, created, err := s.store.AcceptCommandWith(ctx, cmd, func(tx storage.Execer) error {
		if err := s.store.InsertHandoff(ctx, tx, record); err != nil {
			return err
		}
		return s.store.InsertAgentRun(ctx, tx, targetRun)
	}, nil)
	if err != nil {
		return nil, domainError(err)
	}
	if !created {
		return replayHandoffResult(stored)
	}

	result := &v1.SessionHandoffResult{
		CommandID:   stored.ID,
		HandoffID:   record.ID,
		SessionID:   sess.ID,
		SourceRunID: sourceRun.ID,
		TargetRunID: targetRun.ID,
		AgentID:     params.AgentID,
		NodeID:      targetNode.NodeID,
		State:       string(handoff.StateRequested),
	}

	if err := s.runHandoff(ctx, sess, record, targetRun, params); err != nil {
		// The handoff is recorded as failed, and the session is returned to a
		// usable state so the source run can continue.
		_ = s.failHandoff(ctx, sess.ID, record.ID)
		_ = s.failCommand(ctx, stored.ID, err)
		return nil, domainError(err)
	}

	result.State = string(handoff.StateActive)
	if err := s.completeCommand(ctx, stored.ID, result); err != nil {
		s.log.Warn("command result could not be recorded", "command", stored.ID, "error", err)
	}
	return result, nil
}

// runHandoff walks the lifecycle: build the context, start the target, then
// activate.
func (s *Service) runHandoff(ctx context.Context, sess *session.Session, record *handoff.Handoff, targetRun *agent.AgentRun, params v1.SessionHandoffParams) error {
	// The session is in handoff while the transfer is in progress, so routing
	// does not send new prompts to a run that is being replaced.
	if _, err := s.store.UpdateSessionWith(ctx, sess.ID, func(current *session.Session) error {
		return current.Transition(session.StateHandoff)
	}); err != nil {
		return err
	}

	if err := s.advanceHandoff(ctx, record.ID, handoff.StateContextBuilding); err != nil {
		return err
	}

	if err := s.buildHandoffContext(ctx, sess, record.ID, params.Summary); err != nil {
		return err
	}

	if err := s.advanceHandoff(ctx, record.ID, handoff.StateTargetStarting); err != nil {
		return err
	}

	// Starting the target is a side effect. If it fails, the handoff is not
	// reported as a transfer.
	if err := s.startRun(ctx, targetRun, params.Workspace); err != nil {
		return err
	}

	return s.activateHandoff(ctx, sess.ID, record.ID, targetRun.ID)
}

// buildHandoffContext assembles the package handed to the target and stores it as
// a versioned snapshot.
//
// The snapshot id is written with a targeted update: the handoff state is moved
// by the lifecycle steps, and persisting a stale copy of the record would undo
// them.
func (s *Service) buildHandoffContext(ctx context.Context, sess *session.Session, handoffID, summary string) error {
	recent, err := s.store.ReadEvents(ctx, sess.ID, 0, RecentHandoffEvents)
	if err != nil {
		return err
	}

	packageContext := handoff.Context{
		SessionID:     sess.ID,
		Summary:       summary,
		RecentEvents:  recent,
		PriorMessages: messagesOf(recent),
		Workspace: handoff.WorkspaceRef{
			WorkspaceID: sess.WorkspaceID,
		},
	}

	payload, err := json.Marshal(packageContext)
	if err != nil {
		return err
	}

	// A snapshot is versioned so a later context schema can migrate or reject it
	// explicitly rather than being read with the wrong shape.
	snapshot := &session.Snapshot{
		ID:            ids.New("snap"),
		SessionID:     sess.ID,
		SchemaVersion: HandoffContextSchemaVersion,
		Sequence:      lastSequence(recent),
		Payload:       payload,
	}
	if err := s.store.WriteTx(ctx, func(tx storage.Execer) error {
		return s.store.PutSnapshot(ctx, tx, snapshot)
	}); err != nil {
		return err
	}

	_, err = s.store.UpdateHandoffWith(ctx, handoffID, func(current *handoff.Handoff) error {
		current.ContextSnapshotID = snapshot.ID
		return nil
	})
	return err
}

// HandoffContextSchemaVersion identifies the context shape written by this build.
const HandoffContextSchemaVersion = 1

// activateHandoff moves the routing pointer and returns the session to active.
//
// The pointer moves only now: the target execution has started, so a prompt that
// arrives next reaches a run that can serve it.
func (s *Service) activateHandoff(ctx context.Context, sessionID, handoffID, targetRunID string) error {
	if _, err := s.store.UpdateSessionWith(ctx, sessionID, func(current *session.Session) error {
		if err := current.SetDefaultInteractiveRun(targetRunID); err != nil {
			return err
		}
		if current.State != session.StateActive {
			return current.Transition(session.StateActive)
		}
		return nil
	}); err != nil {
		return err
	}

	_, err := s.store.UpdateHandoffWith(ctx, handoffID, func(current *handoff.Handoff) error {
		return current.Transition(handoff.StateActive)
	})
	return err
}

// failHandoff records the failure and returns the session to a usable state.
func (s *Service) failHandoff(ctx context.Context, sessionID, handoffID string) error {
	_, err := s.store.UpdateHandoffWith(ctx, handoffID, func(current *handoff.Handoff) error {
		if current.State.IsTerminal() {
			return nil
		}
		return current.Transition(handoff.StateFailed)
	})
	if err != nil {
		return err
	}

	// The session must not stay in handoff: the source run is still the one that
	// can serve prompts.
	_, err = s.store.UpdateSessionWith(ctx, sessionID, func(current *session.Session) error {
		if current.State == session.StateHandoff {
			return current.Transition(session.StateActive)
		}
		return nil
	})
	return err
}

func (s *Service) advanceHandoff(ctx context.Context, handoffID string, next handoff.State) error {
	_, err := s.store.UpdateHandoffWith(ctx, handoffID, func(current *handoff.Handoff) error {
		return current.Transition(next)
	})
	return err
}

// sourceRunFor resolves the run a handoff transfers from.
func (s *Service) sourceRunFor(ctx context.Context, sess *session.Session, explicitRunID string) (*agent.AgentRun, error) {
	runs, err := s.store.ListAgentRuns(ctx, sess.ID)
	if err != nil {
		return nil, v1.Unavailable("runs could not be read: %s", err.Error())
	}

	want := explicitRunID
	if want == "" {
		want = sess.DefaultInteractiveRunID
	}
	if want == "" {
		return nil, v1.Conflict("session %s has no run to hand off", sess.ID)
	}

	for _, run := range runs {
		if run.ID == want {
			if run.State.IsTerminal() {
				return nil, v1.Conflict("agent run %s is %s", run.ID, run.State)
			}
			return run, nil
		}
	}
	return nil, v1.NotFound("agent run %s", want)
}

// messagesOf extracts the user-visible turns from the session stream.
func messagesOf(events []*event.Event) []string {
	var out []string
	for _, ev := range events {
		if ev.Type != event.TypeMessage {
			continue
		}

		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err == nil && payload.Text != "" {
			out = append(out, payload.Text)
		}
	}
	return out
}

func lastSequence(events []*event.Event) int64 {
	var highest int64
	for _, ev := range events {
		if ev.Sequence > highest {
			highest = ev.Sequence
		}
	}
	return highest
}

// replayHandoffResult returns the outcome of a handoff that already ran.
func replayHandoffResult(stored *command.Command) (*v1.SessionHandoffResult, error) {
	switch {
	case stored.State == command.StateFailed:
		return nil, v1.Unavailable("session.handoff %s failed: %s", stored.ID, stored.Error)
	case len(stored.Result) == 0:
		return nil, v1.Retryable("session.handoff %s has not recorded its outcome yet", stored.ID)
	}

	var result v1.SessionHandoffResult
	if err := json.Unmarshal(stored.Result, &result); err != nil {
		return nil, v1.Internal("stored session.handoff result is unreadable")
	}
	return &result, nil
}
