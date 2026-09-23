package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/thupham/hive/internal/agent"
	"github.com/thupham/hive/internal/apierr"
	"github.com/thupham/hive/internal/command"
	"github.com/thupham/hive/internal/event"
	"github.com/thupham/hive/internal/ids"
	"github.com/thupham/hive/internal/node"
	"github.com/thupham/hive/internal/permission"
	"github.com/thupham/hive/internal/session"
	"github.com/thupham/hive/internal/storage"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// Options configures the Control API service.
type Options struct {
	Store *storage.Store
	Nodes *node.Server
	Log   *slog.Logger

	// Agents are the configured agent ids.
	Agents []string

	// DefaultAgent is used when a request does not name one.
	DefaultAgent string

	// AllowedUsers is the configured allow list of additional user principals.
	AllowedUsers []string

	// Plugins are the trusted plugin identities this coordinator runs.
	Plugins []string
}

// Service implements the Control API operations.
type Service struct {
	store        *storage.Store
	nodes        *node.Server
	log          *slog.Logger
	policy       Policy
	agents       []string
	defaultAgent string
}

// New returns a Control API service.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("control: a store is required")
	case opts.Nodes == nil:
		return nil, errors.New("control: a node server is required")
	}

	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Service{
		store:        opts.Store,
		nodes:        opts.Nodes,
		log:          log,
		policy:       NewPolicy(opts.AllowedUsers, opts.Plugins),
		agents:       opts.Agents,
		defaultAgent: opts.DefaultAgent,
	}, nil
}

// Policy exposes the authorization policy.
func (s *Service) Policy() Policy { return s.policy }

// CreateSession creates a session and its first AgentRun, then starts the
// execution on a node.
//
// The session, the run, and the command commit in one transaction, so a retry
// cannot create a second session and a partial write is impossible. Starting the
// execution is a side effect after that commit: if it fails, a durable session
// and run remain and can be reconciled rather than leaving a half-created
// operation.
func (s *Service) CreateSession(ctx context.Context, principal Principal, params v1.SessionCreateParams) (*v1.SessionCreateResult, error) {
	if err := s.policy.Authorize(principal, v1.MethodSessionCreate); err != nil {
		return nil, err
	}
	if params.CommandID == "" {
		return nil, v1.InvalidParams("commandId is required")
	}

	agentID := params.AgentID
	if agentID == "" {
		agentID = s.defaultAgent
	}
	// A retry of the same logical operation returns what the first call recorded.
	// Checking the node first would fail a retry after the node went away, even
	// though the session was already created.
	if existing, err := s.store.GetCommand(ctx, params.CommandID); err == nil {
		switch existing.State {
		case command.StateCompleted, command.StateFailed:
			return replayCreateResult(existing)
		}
	}

	if !s.hasAgent(agentID) {
		return nil, v1.InvalidParams("unknown agent %q", agentID)
	}

	// A node must be available before anything is written: creating a session
	// that nothing can run would be worse than failing.
	executionNode, err := s.nodeForAgent(agentID)
	if err != nil {
		return nil, err
	}

	// A named workspace is resolved, and registered on first use: naming it is
	// enough, the caller does not have to create it first.
	workspaceID, err := s.resolveWorkspace(ctx, params.Workspace, executionNode.NodeID, "")
	if err != nil {
		return nil, domainError(err)
	}

	sess := session.New(ids.New("sess"))
	sess.WorkspaceID = workspaceID
	if len(params.Metadata) > 0 {
		sess.Metadata = params.Metadata
	}

	run := agent.New(ids.New("run"), sess.ID, agentID, protocolForAgent())
	run.NodeID = executionNode.NodeID

	cmd := command.New(params.CommandID, string(principal), v1.MethodSessionCreate)
	cmd.SourceID = params.SourceID
	cmd.Target = sess.ID
	// Acceptance and the domain mutation are the same commit, so the command is
	// created already accepted.
	cmd.State = command.StateAccepted

	stored, created, err := s.store.AcceptCommandWith(ctx, cmd, func(tx storage.Execer) error {
		// The run this call creates is the session's default interactive run,
		// so the first prompt routes to it instead of creating a second one.
		if err := sess.SetDefaultInteractiveRun(run.ID); err != nil {
			return err
		}
		if err := s.store.InsertSession(ctx, tx, sess); err != nil {
			return err
		}
		return s.store.InsertAgentRun(ctx, tx, run)
	}, nil)
	if err != nil {
		return nil, v1.Unavailable("session could not be created: %s", err.Error())
	}

	if !created {
		return replayCreateResult(stored)
	}

	result := &v1.SessionCreateResult{
		CommandID: stored.ID,
		SessionID: sess.ID,
		RunID:     run.ID,
		AgentID:   agentID,
		NodeID:    executionNode.NodeID,
		RunState:  string(run.State),
	}

	// The run works at the location registered for the node it runs on.
	runPath, err := s.workspacePath(ctx, workspaceID, executionNode.NodeID)
	if err != nil {
		return nil, domainError(err)
	}
	if err := s.startRun(ctx, run, runPath, ""); err != nil {
		s.log.Warn("execution could not be started", "run", run.ID, "node", run.NodeID, "error", err)
	}

	if err := s.completeCommand(ctx, stored.ID, result); err != nil {
		s.log.Warn("command result could not be recorded", "command", stored.ID, "error", err)
	}
	return result, nil
}

// Prompt sends a prompt to an AgentRun, creating one when routing requires it.
func (s *Service) Prompt(ctx context.Context, principal Principal, params v1.SessionPromptParams) (*v1.SessionPromptResult, error) {
	if err := s.policy.Authorize(principal, v1.MethodSessionPrompt); err != nil {
		return nil, err
	}
	switch {
	case params.CommandID == "":
		return nil, v1.InvalidParams("commandId is required")
	case params.SessionID == "":
		return nil, v1.InvalidParams("sessionId is required")
	case params.Text == "":
		return nil, v1.InvalidParams("text is required")
	}
	if err := s.authorizeSession(ctx, principal, params.SessionID); err != nil {
		return nil, err
	}

	sess, err := s.store.GetSession(ctx, params.SessionID)
	if err != nil {
		return nil, storageError(err)
	}

	cmd := command.New(params.CommandID, string(principal), v1.MethodSessionPrompt)
	cmd.SourceID = params.SourceID
	cmd.Target = params.SessionID
	cmd.State = command.StateAccepted

	// Routing runs inside the transaction so the target is fixed when the
	// command is accepted: a retry of the same command id resolves to the same
	// logical target.
	var (
		runID      string
		createdRun bool
	)
	stored, created, err := s.store.AcceptCommandWith(ctx, cmd, func(tx storage.Execer) error {
		target, err := s.resolvePromptTarget(ctx, sess, params.RunID)
		if err != nil {
			return err
		}

		if target.NewRun {
			// A new run continues the same conversation, so it uses the agent the
			// session was already talking to. Silently switching to the configured
			// default would surprise a user who chose an agent.
			agentID := s.agentForNewRun(ctx, sess)
			if agentID == "" {
				return v1.InvalidParams("no agent is configured")
			}

			newRun := agent.New(ids.New("run"), sess.ID, agentID, protocolForAgent())
			executionNode, err := s.nodeForAgent(agentID)
			if err != nil {
				return err
			}
			newRun.NodeID = executionNode.NodeID
			if err := s.store.InsertAgentRun(ctx, tx, newRun); err != nil {
				return err
			}
			runID, createdRun = newRun.ID, true
			return nil
		}

		runID = target.RunID
		return nil
	}, nil)
	if err != nil {
		return nil, domainError(err)
	}

	result := &v1.SessionPromptResult{
		CommandID:  stored.ID,
		RunID:      runID,
		CreatedRun: createdRun,
	}
	if !created {
		return replayPromptResult(stored)
	}

	// A run created by routing has to be started, exactly as a run created by
	// session.create is. Without this it would exist with no execution, and the
	// prompt would have nowhere to go.
	if createdRun {
		newRun, err := s.store.GetAgentRun(ctx, runID)
		if err != nil {
			_ = s.failCommand(ctx, stored.ID, err)
			return nil, domainError(err)
		}

		path, err := s.workspacePath(ctx, sess.WorkspaceID, newRun.NodeID)
		if err != nil {
			_ = s.failCommand(ctx, stored.ID, err)
			return nil, domainError(err)
		}

		if err := s.startRun(ctx, newRun, path, ""); err != nil {
			_ = s.failCommand(ctx, stored.ID, err)
			return nil, domainError(err)
		}
	}

	// The turn runs in the background.
	//
	// A command's lifetime is not a request's lifetime: a turn can take minutes,
	// and holding the request open would tie the operation to a connection that
	// may go away. The caller follows the operation through command.get, which is
	// what the durable command record is for.
	go s.runTurn(stored.ID, runID, params.Text)

	return result, nil
}

// TurnTimeout bounds one agent turn.
//
// It is generous because an agent may explore a repository, run tools, and think
// for a while. It exists so a wedged agent cannot pin a run forever.
const TurnTimeout = 30 * time.Minute

// runTurn dispatches a prompt and records the outcome.
func (s *Service) runTurn(commandID, runID, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), TurnTimeout)
	defer cancel()

	if err := s.dispatchPrompt(ctx, runID, text); err != nil {
		s.log.Warn("turn could not be dispatched", "run", runID, "error", err)
		_ = s.failCommand(ctx, commandID, err)
		return
	}

	// The turn ended, so the command is complete. The agent's answer travels as
	// an event, which is what a client reads.
	if err := s.completeCommand(ctx, commandID, map[string]any{"runId": runID}); err != nil {
		s.log.Warn("command result could not be recorded", "command", commandID, "error", err)
	}
}

// Cancel cancels the current turn of an AgentRun.
func (s *Service) Cancel(ctx context.Context, principal Principal, params v1.SessionCancelParams) error {
	if err := s.policy.Authorize(principal, v1.MethodSessionCancel); err != nil {
		return err
	}
	if params.SessionID == "" {
		return v1.InvalidParams("sessionId is required")
	}
	if err := s.authorizeSession(ctx, principal, params.SessionID); err != nil {
		return err
	}

	run, err := s.runForCancel(ctx, params)
	if err != nil {
		return err
	}

	n, ok := s.nodes.Node(run.NodeID)
	if !ok || !n.Connected() {
		return v1.Unavailable("node %s is not connected", run.NodeID)
	}

	err = n.Call(ctx, v1.MethodExecutionCancel, v1.ExecutionCancelParams{
		AgentRunID: run.ID,
		AgentID:    run.AgentID,
		Generation: run.ExecutionGeneration,
	}, nil)
	if err != nil {
		return domainError(err)
	}
	return nil
}

// Status returns one session and its runs.
func (s *Service) Status(ctx context.Context, principal Principal, sessionID string) (*v1.SessionStatusResult, error) {
	if err := s.policy.Authorize(principal, v1.MethodSessionStatus); err != nil {
		return nil, err
	}
	if err := s.authorizeSession(ctx, principal, sessionID); err != nil {
		return nil, err
	}

	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, storageError(err)
	}
	runs, err := s.store.ListAgentRuns(ctx, sessionID)
	if err != nil {
		return nil, v1.Unavailable("runs could not be read: %s", err.Error())
	}

	result := &v1.SessionStatusResult{
		SessionID:    sess.ID,
		State:        string(sess.State),
		Workspace:    sess.WorkspaceID,
		DefaultRunID: sess.DefaultInteractiveRunID,
		Runs:         make([]v1.RunSummary, 0, len(runs)),
	}
	for _, run := range runs {
		result.Runs = append(result.Runs, runSummary(run))
	}
	return result, nil
}

// ListSessions returns the sessions a principal may see.
//
// The owner sees every session. Any other principal sees only the sessions it is
// bound to through a transport conversation.
func (s *Service) ListSessions(ctx context.Context, principal Principal, limit int) (*v1.SessionListResult, error) {
	if err := s.policy.Authorize(principal, v1.MethodSessionList); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}

	sessions, err := s.visibleSessions(ctx, principal)
	if err != nil {
		return nil, err
	}
	if len(sessions) > limit {
		sessions = sessions[:limit]
	}

	result := &v1.SessionListResult{Sessions: make([]v1.SessionSummary, 0, len(sessions))}
	for _, sess := range sessions {
		runs, err := s.store.ListAgentRuns(ctx, sess.ID)
		if err != nil {
			return nil, v1.Unavailable("runs could not be read: %s", err.Error())
		}
		result.Sessions = append(result.Sessions, v1.SessionSummary{
			SessionID: sess.ID,
			State:     string(sess.State),
			Runs:      len(runs),
			UpdatedAt: sess.UpdatedAt,
		})
	}
	return result, nil
}

// GetCommand returns the durable status of a command.
func (s *Service) GetCommand(ctx context.Context, principal Principal, commandID string) (*v1.CommandResult, error) {
	if err := s.policy.Authorize(principal, v1.MethodCommandGet); err != nil {
		return nil, err
	}
	if commandID == "" {
		return nil, v1.InvalidParams("commandId is required")
	}

	cmd, err := s.store.GetCommand(ctx, commandID)
	if err != nil {
		return nil, storageError(err)
	}
	// A principal may only inspect the commands it issued.
	if cmd.Actor != string(principal) && principal != s.policy.Owner {
		return nil, v1.NotFound("command %s", commandID)
	}

	return &v1.CommandResult{
		CommandID: cmd.ID,
		Method:    cmd.Method,
		State:     string(cmd.State),
		Target:    cmd.Target,
		Result:    cmd.Result,
		Error:     cmd.Error,
		CreatedAt: cmd.CreatedAt,
		UpdatedAt: cmd.UpdatedAt,
	}, nil
}

// Replay returns a session event stream from a cursor.
func (s *Service) Replay(ctx context.Context, principal Principal, params v1.EventReplayParams) (*v1.EventReplayResult, error) {
	if err := s.policy.Authorize(principal, v1.MethodSessionEvents); err != nil {
		return nil, err
	}
	if params.SessionID == "" {
		return nil, v1.InvalidParams("sessionId is required")
	}
	if err := s.authorizeSession(ctx, principal, params.SessionID); err != nil {
		return nil, err
	}

	replay, err := s.store.Replay(ctx, params.SessionID, params.FromSequence, params.Limit)
	if err != nil {
		return nil, v1.Unavailable("events could not be read: %s", err.Error())
	}

	result := &v1.EventReplayResult{Events: make([]v1.Event, 0, len(replay.Events))}
	for _, ev := range replay.Events {
		result.Events = append(result.Events, wireEvent(ev))
	}
	if replay.Gap != nil {
		result.Gap = &v1.CursorGap{
			Requested:        replay.Gap.Requested,
			NextSequence:     replay.Gap.NextSequence,
			SnapshotID:       replay.Gap.SnapshotID,
			SnapshotSequence: replay.Gap.SnapshotSequence,
		}
	}
	return result, nil
}

// ListPermissions returns pending permission requests.
//
// The owner sees every session. Any other principal sees only the requests of
// the sessions it is bound to.
func (s *Service) ListPermissions(ctx context.Context, principal Principal, params v1.PermissionListParams) (*v1.PermissionListResult, error) {
	if err := s.policy.Authorize(principal, v1.MethodPermissionList); err != nil {
		return nil, err
	}

	if params.SessionID != "" {
		if err := s.authorizeSession(ctx, principal, params.SessionID); err != nil {
			return nil, err
		}
		open, err := s.store.ListOpenPermissionRequests(ctx, params.SessionID)
		if err != nil {
			return nil, storageError(err)
		}
		return permissionList(open), nil
	}

	open, err := s.store.ListAllOpenPermissionRequests(ctx, params.Limit)
	if err != nil {
		return nil, storageError(err)
	}

	// A non-owner must not learn about another principal's pending work.
	if principal != s.policy.Owner {
		visible, err := s.store.ListSessionsForPrincipal(ctx, string(principal))
		if err != nil {
			return nil, storageError(err)
		}
		allowed := make(map[string]bool, len(visible))
		for _, id := range visible {
			allowed[id] = true
		}

		kept := open[:0]
		for _, req := range open {
			if allowed[req.SessionID] {
				kept = append(kept, req)
			}
		}
		open = kept
	}

	return permissionList(open), nil
}

func permissionList(open []*permission.Request) *v1.PermissionListResult {
	result := &v1.PermissionListResult{Permissions: make([]v1.PermissionSummary, 0, len(open))}
	for _, req := range open {
		result.Permissions = append(result.Permissions, v1.PermissionSummary{
			PermissionID:   req.ID,
			SessionID:      req.SessionID,
			RunID:          req.RunID,
			AgentRequestID: req.AgentRequestID,
			CreatedAt:      req.CreatedAt,
			ExpiresAt:      req.ExpiresAt,
		})
	}
	return result
}

// ListAgents returns the configured agents.
func (s *Service) ListAgents(ctx context.Context, principal Principal) (*v1.AgentListResult, error) {
	if err := s.policy.Authorize(principal, v1.MethodAgentList); err != nil {
		return nil, err
	}

	result := &v1.AgentListResult{Agents: make([]v1.AgentSummary, 0, len(s.agents))}
	for _, id := range s.agents {
		result.Agents = append(result.Agents, v1.AgentSummary{ID: id, Protocol: protocolForAgent()})
	}
	return result, nil
}

// ListNodes returns the nodes the coordinator has heard from.
func (s *Service) ListNodes(ctx context.Context, principal Principal) (*v1.NodeListResult, error) {
	if err := s.policy.Authorize(principal, v1.MethodNodeList); err != nil {
		return nil, err
	}

	nodes := s.nodes.Nodes()
	result := &v1.NodeListResult{Nodes: make([]v1.NodeSummary, 0, len(nodes))}
	for _, n := range nodes {
		result.Nodes = append(result.Nodes, v1.NodeSummary{
			NodeID:               n.NodeID,
			Version:              n.Version,
			ConnectionGeneration: n.ConnectionGeneration,
			Connected:            n.Connected(),
			Executions:           len(n.Executions()),
			LastSeen:             n.LastSeen(),
		})
	}
	return result, nil
}

func (s *Service) hasAgent(agentID string) bool {
	for _, id := range s.agents {
		if id == agentID {
			return true
		}
	}
	return false
}

// nodeForAgent picks a connected node that declared it can run the agent.
func (s *Service) nodeForAgent(agentID string) (*node.Node, error) {
	for _, n := range s.nodes.Nodes() {
		if n.Connected() && n.RunsAgent(agentID) {
			return n, nil
		}
	}
	return nil, v1.Unavailable("no connected node runs agent %q", agentID)
}

func (s *Service) startRun(ctx context.Context, run *agent.AgentRun, workspacePath, preamble string) error {
	n, ok := s.nodes.Node(run.NodeID)
	if !ok || !n.Connected() {
		return v1.Unavailable("node %s is not connected", run.NodeID)
	}

	var out struct {
		RuntimeSessionID string `json:"runtimeSessionId"`
	}
	err := n.Call(ctx, v1.MethodExecutionStart, v1.ExecutionStartParams{
		AgentRunID:    run.ID,
		SessionID:     run.SessionID,
		AgentID:       run.AgentID,
		Generation:    run.ExecutionGeneration,
		WorkspacePath: workspacePath,
		Context:       preamble,
	}, &out)
	if err != nil {
		return err
	}

	// Record the runtime session id from the authoritative response, in case the
	// node's own report is lost.
	//
	// This is a targeted read-modify-write inside one transaction: the node may
	// already have reported a state change, and writing a stale copy of the run
	// would silently overwrite it.
	_, err = s.store.UpdateAgentRunWith(ctx, run.ID, func(current *agent.AgentRun) error {
		current.RuntimeSessionID = out.RuntimeSessionID
		return nil
	})
	return err
}

func (s *Service) dispatchPrompt(ctx context.Context, runID, text string) error {
	run, err := s.store.GetAgentRun(ctx, runID)
	if err != nil {
		return storageError(err)
	}
	if run.State.IsTerminal() {
		return v1.Conflict("agent run %s is %s", run.ID, run.State)
	}

	n, ok := s.nodes.Node(run.NodeID)
	if !ok || !n.Connected() {
		return v1.Unavailable("node %s is not connected", run.NodeID)
	}

	err = n.Call(ctx, v1.MethodExecutionPrompt, v1.ExecutionPromptParams{
		AgentRunID: run.ID,
		AgentID:    run.AgentID,
		Generation: run.ExecutionGeneration,
		Text:       text,
	}, nil)
	if err != nil {
		return domainError(err)
	}
	return nil
}

// resolvePromptTarget applies the deterministic routing rule.
func (s *Service) resolvePromptTarget(ctx context.Context, sess *session.Session, explicitRunID string) (session.Target, error) {
	runs, err := s.store.ListAgentRuns(ctx, sess.ID)
	if err != nil {
		return session.Target{}, err
	}

	byID := make(map[string]*agent.AgentRun, len(runs))
	for _, run := range runs {
		byID[run.ID] = run
	}
	lookup := func(id string) (*agent.AgentRun, bool) {
		run, ok := byID[id]
		return run, ok
	}

	return session.RoutePrompt(explicitRunID, sess, lookup)
}

func (s *Service) runForCancel(ctx context.Context, params v1.SessionCancelParams) (*agent.AgentRun, error) {
	runs, err := s.store.ListAgentRuns(ctx, params.SessionID)
	if err != nil {
		return nil, v1.Unavailable("runs could not be read: %s", err.Error())
	}

	if params.RunID != "" {
		for _, run := range runs {
			if run.ID == params.RunID {
				return run, nil
			}
		}
		return nil, v1.NotFound("agent run %s", params.RunID)
	}

	// Without an explicit run, cancel the session's current interactive run.
	sess, err := s.store.GetSession(ctx, params.SessionID)
	if err != nil {
		return nil, storageError(err)
	}
	for _, run := range runs {
		if run.ID == sess.DefaultInteractiveRunID && !run.State.IsTerminal() {
			return run, nil
		}
	}
	return nil, v1.Conflict("session %s has no run to cancel", params.SessionID)
}

// authorizeSession enforces session isolation.
func (s *Service) authorizeSession(ctx context.Context, principal Principal, sessionID string) error {
	if principal == s.policy.Owner {
		return nil
	}

	visible, err := s.store.ListSessionsForPrincipal(ctx, string(principal))
	if err != nil {
		return v1.Unavailable("session access could not be checked: %s", err.Error())
	}
	for _, id := range visible {
		if id == sessionID {
			return nil
		}
	}
	// Not found rather than unauthorized: a principal must not learn that a
	// session it cannot reach exists.
	return v1.NotFound("session %s", sessionID)
}

func (s *Service) visibleSessions(ctx context.Context, principal Principal) ([]*session.Session, error) {
	if principal == s.policy.Owner {
		ids, err := s.store.ListSessions(ctx, 0)
		if err != nil {
			return nil, v1.Unavailable("sessions could not be read: %s", err.Error())
		}
		return ids, nil
	}

	ids, err := s.store.ListSessionsForPrincipal(ctx, string(principal))
	if err != nil {
		return nil, v1.Unavailable("sessions could not be read: %s", err.Error())
	}

	out := make([]*session.Session, 0, len(ids))
	for _, id := range ids {
		sess, err := s.store.GetSession(ctx, id)
		if err != nil {
			continue
		}
		out = append(out, sess)
	}
	return out, nil
}

func (s *Service) completeCommand(ctx context.Context, id string, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return s.store.WriteTx(ctx, func(tx storage.Execer) error {
		return s.store.SetCommandState(ctx, tx, id, command.StateCompleted, raw, nil)
	})
}

func (s *Service) failCommand(ctx context.Context, id string, cause error) error {
	raw, err := json.Marshal(map[string]string{"message": cause.Error()})
	if err != nil {
		return err
	}
	return s.store.WriteTx(ctx, func(tx storage.Execer) error {
		return s.store.SetCommandState(ctx, tx, id, command.StateFailed, nil, raw)
	})
}

// replayCreateResult returns the outcome of an operation that already ran.
//
// A retry of the same command id must resolve to the same logical operation
// rather than create a second session.
func replayCreateResult(stored *command.Command) (*v1.SessionCreateResult, error) {
	if stored.State == command.StateFailed {
		return nil, v1.Unavailable("session.create %s failed: %s", stored.ID, stored.Error)
	}
	if len(stored.Result) == 0 {
		return nil, v1.Retryable("session.create %s has not recorded its outcome yet", stored.ID)
	}

	var result v1.SessionCreateResult
	if err := json.Unmarshal(stored.Result, &result); err != nil {
		return nil, v1.Internal("stored session.create result is unreadable")
	}
	return &result, nil
}

func replayPromptResult(stored *command.Command) (*v1.SessionPromptResult, error) {
	if stored.State == command.StateFailed {
		return nil, v1.Unavailable("session.prompt %s failed: %s", stored.ID, stored.Error)
	}
	if len(stored.Result) == 0 {
		return nil, v1.Retryable("session.prompt %s has not recorded its outcome yet", stored.ID)
	}

	var result v1.SessionPromptResult
	if err := json.Unmarshal(stored.Result, &result); err != nil {
		return nil, v1.Internal("stored session.prompt result is unreadable")
	}
	return &result, nil
}

func runSummary(run *agent.AgentRun) v1.RunSummary {
	return v1.RunSummary{
		RunID:               run.ID,
		AgentID:             run.AgentID,
		NodeID:              run.NodeID,
		State:               string(run.State),
		ExecutionGeneration: run.ExecutionGeneration,
		RuntimeSessionID:    run.RuntimeSessionID,
	}
}

func wireEvent(ev *event.Event) v1.Event {
	return v1.Event{
		ID:         ev.ID,
		Sequence:   ev.Sequence,
		SessionID:  ev.SessionID,
		RunID:      ev.RunID,
		OriginNode: ev.OriginNode,
		Timestamp:  ev.Timestamp,
		Type:       ev.Type,
		Version:    ev.Version,
		Protocol:   ev.Protocol,
		Method:     ev.Method,
		Payload:    ev.Payload,
	}
}

// protocolForAgent is the agent protocol v1 speaks. Hive core holds no vendor
// knowledge, so every agent uses the same protocol and is distinguished by
// configuration.
func protocolForAgent() string { return "acp" }

// storageError maps a storage error onto a protocol error.
func storageError(err error) error { return apierr.From(err) }

// domainError maps an operation error onto a protocol error.
func domainError(err error) error { return apierr.From(err) }

// agentForNewRun picks the agent a routing-created run should use.
//
// The session's previous default run names the agent the conversation was using,
// which is the honest choice: a user who chose an agent should keep it.
func (s *Service) agentForNewRun(ctx context.Context, sess *session.Session) string {
	if sess.DefaultInteractiveRunID != "" {
		if previous, err := s.store.GetAgentRun(ctx, sess.DefaultInteractiveRunID); err == nil && previous.AgentID != "" {
			return previous.AgentID
		}
	}
	return s.defaultAgent
}

// workspacePath returns the location a workspace has on a node.
//
// A workspace may live at different paths on different machines, so the path is a
// property of the location rather than of the workspace.
func (s *Service) workspacePath(ctx context.Context, workspaceID, nodeID string) (string, error) {
	if workspaceID == "" {
		return "", nil
	}

	locations, err := s.store.ListWorkspaceLocations(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	for _, loc := range locations {
		if loc.NodeID == nodeID {
			return loc.Path, nil
		}
	}
	return "", nil
}
