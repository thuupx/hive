package control

import (
	"context"

	"github.com/thupham/hive/internal/agent"
	"github.com/thupham/hive/internal/session"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// SessionConfig reads or changes a session's agent selectors.
//
// Hive never interprets a selector. The agent declares what it offers, the user
// picks a value, and Hive stores the choice on the session so a new AgentRun
// continues with it instead of silently reverting.
func (s *Service) SessionConfig(ctx context.Context, principal Principal, params v1.SessionConfigParams) (*v1.SessionConfigResult, error) {
	if err := s.policy.Authorize(principal, v1.MethodSessionConfig); err != nil {
		return nil, err
	}
	if params.SessionID == "" {
		return nil, v1.InvalidParams("sessionId is required")
	}
	if err := s.authorizeSession(ctx, principal, params.SessionID); err != nil {
		return nil, err
	}

	sess, err := s.store.GetSession(ctx, params.SessionID)
	if err != nil {
		return nil, storageError(err)
	}

	run, err := s.configRun(ctx, sess, params.RunID)
	if err != nil {
		return nil, err
	}

	// An agent session lives in the agent process, so it does not survive a
	// restart. Saying so is more useful than a bare failure.
	// A run with no live session is brought up below rather than refused. Asking to
	// see the settings is a request to have the agent there, and a conversation that
	// has not been spoken in yet still has settings to choose.
	// A config id with no value is a read of that selector, not a change to it.
	// Recording the empty value would poison every later run: the agent would be
	// asked to apply a value that is not one.
	if params.ConfigID != "" && params.Value != "" {
		// The change is recorded before it is applied, so a run that starts next
		// keeps the choice even if the live session refuses it. Refusing the live
		// change then surfaces as an error, which is the honest outcome: the user
		// picked something the running agent would not accept.
		if err := s.recordConfig(ctx, sess.ID, params.ConfigID, params.Value); err != nil {
			return nil, domainError(err)
		}
	}

	result, err := s.liveConfig(ctx, run, params)
	if err == nil || v1.AsError(err).Code != v1.CodeNotFound {
		return result, err
	}

	// The agent session lives in the agent process, so a restart leaves the run
	// with no execution. Bringing it up is more useful than telling the user to
	// send a message first: asking to see the models is a request to have the
	// agent there.
	if startErr := s.ensureExecution(ctx, run); startErr != nil {
		return nil, v1.Unavailable(
			"session %s has no live agent session and it could not be started: %s",
			params.SessionID, startErr.Error())
	}
	return s.liveConfig(ctx, run, params)
}

// ensureExecution starts a run that has no execution, restoring its agent session
// where the agent can.
func (s *Service) ensureExecution(ctx context.Context, run *agent.AgentRun) error {
	executionNode, ok := s.nodes.Node(run.NodeID)
	if !ok || !executionNode.Connected() {
		return v1.Unavailable("node %s is not connected", run.NodeID)
	}

	sess, err := s.store.GetSession(ctx, run.SessionID)
	if err != nil {
		return err
	}

	path, err := s.workspacePath(ctx, sess.WorkspaceID, run.NodeID)
	if err != nil {
		return err
	}
	return s.startRun(ctx, run, path, "", sess.AgentConfig)
}

// configRun resolves which run carries the live agent session.
func (s *Service) configRun(ctx context.Context, sess *session.Session, explicitRunID string) (*agent.AgentRun, error) {
	runs, err := s.store.ListAgentRuns(ctx, sess.ID)
	if err != nil {
		return nil, v1.Unavailable("runs could not be read: %s", err.Error())
	}
	if len(runs) == 0 {
		return nil, v1.Conflict("session %s has no run", sess.ID)
	}

	want := explicitRunID
	if want == "" {
		want = sess.DefaultInteractiveRunID
	}
	if want == "" {
		want = runs[len(runs)-1].ID
	}

	for _, run := range runs {
		if run.ID == want {
			return run, nil
		}
	}
	return nil, v1.NotFound("agent run %s", want)
}

// liveConfig reads or changes the selectors of a live execution.
func (s *Service) liveConfig(ctx context.Context, run *agent.AgentRun, params v1.SessionConfigParams) (*v1.SessionConfigResult, error) {
	executionNode, ok := s.nodes.Node(run.NodeID)
	if !ok || !executionNode.Connected() {
		return nil, v1.Unavailable("node %s is not connected", run.NodeID)
	}

	var out v1.ExecutionConfigResult
	err := executionNode.Call(ctx, v1.MethodExecutionConfig, v1.ExecutionConfigParams{
		AgentRunID: run.ID,
		AgentID:    run.AgentID,
		ConfigID:   params.ConfigID,
		Value:      params.Value,
	}, &out)
	if err != nil {
		return nil, domainError(err)
	}

	return &v1.SessionConfigResult{
		SessionID: params.SessionID,
		AgentID:   run.AgentID,
		Options:   out.Options,
	}, nil
}

// recordConfig stores the choice on the session.
func (s *Service) recordConfig(ctx context.Context, sessionID, configID, value string) error {
	_, err := s.store.UpdateSessionWith(ctx, sessionID, func(current *session.Session) error {
		current.SetAgentConfig(configID, value)
		return nil
	})
	return err
}
