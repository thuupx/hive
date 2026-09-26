package daemon_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/thuupx/hive/internal/agent"
	"github.com/thuupx/hive/internal/client"
	"github.com/thuupx/hive/internal/control"
	"github.com/thuupx/hive/internal/event"
	"github.com/thuupx/hive/internal/ids"
	"github.com/thuupx/hive/internal/permission"
	"github.com/thuupx/hive/internal/session"
	"github.com/thuupx/hive/internal/storage"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

func sessionFor(_ *testing.T, id string) *session.Session {
	return session.New(id)
}

func permissionRequestFor(_ *testing.T, sessionID, runID, agentRequestID string, expiresAt time.Time) *permission.Request {
	return permission.New(ids.New("perm"), sessionID, runID, agentRequestID,
		json.RawMessage(`{"toolCall":{"title":"Write file"}}`), expiresAt)
}

// These tests exercise the correctness properties in the design's section 42
// through the real stack, rather than asserting them in prose.

// Retrying the same logical operation must not start a second execution.
//
// A start is a side effect after commit, so the coordinator can lose its
// acknowledgement. What must never happen is a second execution for the same
// AgentRun generation.
func TestRetriedCreateDoesNotStartASecondExecution(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	params := v1.SessionCreateParams{CommandID: "cmd_1"}

	first, err := c.CreateSession(ctx, params)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	second, err := c.CreateSession(ctx, params)
	if err != nil {
		t.Fatalf("retry CreateSession: %v", err)
	}

	if second.SessionID != first.SessionID || second.RunID != first.RunID {
		t.Fatalf("retry returned %+v, want %+v", second, first)
	}

	sessions, err := store.ListSessions(ctx, 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}

	runs, err := store.ListAgentRuns(ctx, first.SessionID)
	if err != nil {
		t.Fatalf("ListAgentRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0].ExecutionGeneration != 1 {
		t.Fatalf("generation = %d, want 1", runs[0].ExecutionGeneration)
	}
}

// The same event id is persisted at most once, whatever delivers it twice.
func TestDuplicateEventIsPersistedOnce(t *testing.T) {
	ctx := context.Background()
	_, store, _, _ := startStack(t)

	if err := store.WriteTx(ctx, func(tx storage.Execer) error {
		return store.InsertSession(ctx, tx, sessionFor(t, "sess_dup"))
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	duplicate := &event.Event{
		ID:        "ev_1",
		SessionID: "sess_dup",
		Type:      event.TypeMessage,
		Version:   1,
		Payload:   json.RawMessage(`{"text":"hello"}`),
	}

	for i := 0; i < 3; i++ {
		if err := store.WriteTx(ctx, func(tx storage.Execer) error {
			_, err := store.AppendEvents(ctx, tx, duplicate)
			return err
		}); err != nil {
			t.Fatalf("AppendEvents: %v", err)
		}
	}

	count, err := store.EventCount(ctx, "sess_dup")
	if err != nil {
		t.Fatalf("EventCount: %v", err)
	}
	if count != 1 {
		t.Fatalf("events = %d, want 1", count)
	}
	if duplicate.Sequence != 1 {
		t.Fatalf("sequence = %d, want 1: a duplicate consumes no sequence", duplicate.Sequence)
	}
}

// A node that reconnects must not produce a second AgentRun, and the coordinator
// must keep serving while it is away.
func TestNodeReconnectCreatesNoDuplicateRun(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{CommandID: "cmd_1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	waitFor(t, "the run to start", func() bool {
		run, err := store.GetAgentRun(ctx, created.RunID)
		return err == nil && run.State == agent.StateStarting
	})

	// The coordinator keeps answering while the node is present.
	nodes, err := c.ListNodes(ctx)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes.Nodes) != 1 {
		t.Fatalf("nodes = %+v", nodes.Nodes)
	}
	if nodes.Nodes[0].ConnectionGeneration < 1 {
		t.Fatalf("generation = %d", nodes.Nodes[0].ConnectionGeneration)
	}

	// Whatever happened to the link, exactly one run exists for the session.
	runs, err := store.ListAgentRuns(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("ListAgentRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if runs[0].ID != created.RunID {
		t.Fatalf("run = %q, want %q", runs[0].ID, created.RunID)
	}
}

// A report from a superseded execution generation must not move the current one.
func TestStaleExecutionGenerationIsRejected(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{CommandID: "cmd_1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	waitFor(t, "the run to start", func() bool {
		run, err := store.GetAgentRun(ctx, created.RunID)
		return err == nil && run.State == agent.StateStarting
	})

	// Recover the run, which is the only way a new generation appears.
	recovered, err := store.UpdateAgentRunWith(ctx, created.RunID, func(run *agent.AgentRun) error {
		if err := run.Transition(agent.StateInterrupted); err != nil {
			return err
		}
		return run.Recover("node-exec-2")
	})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if recovered.ExecutionGeneration != 2 {
		t.Fatalf("generation = %d, want 2", recovered.ExecutionGeneration)
	}

	// A late report from the first generation must be refused.
	err = recovered.CheckGeneration(1)
	if err == nil {
		t.Fatal("a stale generation was accepted")
	}
	if err := recovered.CheckGeneration(2); err != nil {
		t.Fatalf("the current generation must be accepted: %v", err)
	}
}

// A permission request that cannot be answered must never become an implicit
// approval.
func TestPendingPermissionNeverBecomesApproval(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{CommandID: "cmd_1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// The agent asks for permission, and nobody answers.
	request := permissionRequestFor(t, created.SessionID, created.RunID, "agent-req-1", time.Now().UTC().Add(-time.Minute))
	if err := store.WriteTx(ctx, func(tx storage.Execer) error {
		return store.InsertPermissionRequest(ctx, tx, request)
	}); err != nil {
		t.Fatalf("InsertPermissionRequest: %v", err)
	}

	pending, err := c.ListPermissions(ctx, v1.PermissionListParams{})
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	if len(pending.Permissions) != 1 {
		t.Fatalf("permissions = %+v", pending.Permissions)
	}

	// Expiry is a safe terminal state, and it is never an approval.
	expired, err := store.ExpirePermissionRequests(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("ExpirePermissionRequests: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired = %d, want 1", expired)
	}

	resolved, err := store.GetPermissionRequest(ctx, request.ID)
	if err != nil {
		t.Fatalf("GetPermissionRequest: %v", err)
	}
	if resolved.State != "expired" {
		t.Fatalf("state = %q, want expired", resolved.State)
	}
	if resolved.State == "approved" {
		t.Fatal("expiry became an approval")
	}

	// A late response observes the resolved state rather than winning.
	_, won, err := store.ResolvePermissionRequest(ctx, request.ID, "approved")
	if err != nil {
		t.Fatalf("ResolvePermissionRequest: %v", err)
	}
	if won {
		t.Fatal("a late response won after expiry")
	}
}

// The coordinator serves unrelated work while a plugin is dead.
func TestPluginFailureDoesNotStopTheCoordinator(t *testing.T) {
	ctx := context.Background()
	_, _, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	// The agent is not installed in the test environment, so the start fails. The
	// session and the run survive, and the coordinator keeps answering.
	created, err := c.CreateSession(ctx, v1.SessionCreateParams{CommandID: "cmd_1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	agents, err := c.ListAgents(ctx)
	if err != nil {
		t.Fatalf("the coordinator stopped answering: %v", err)
	}
	if len(agents.Agents) != 1 {
		t.Fatalf("agents = %+v", agents.Agents)
	}

	status, err := c.Status(ctx, created.SessionID)
	if err != nil {
		t.Fatalf("the session did not survive: %v", err)
	}
	if status.SessionID != created.SessionID {
		t.Fatalf("status = %+v", status)
	}
}

// Answering a pending permission request relays the decision to the agent and
// records it.
//
// Without a way to answer, a turn that asks for permission hangs forever: the
// request fails closed, which is correct, but nothing can resolve it.
func TestPermissionResponseIsRelayedAndRecorded(t *testing.T) {
	ctx := context.Background()
	_, store, _, socketPath := startStack(t)

	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	created, err := c.CreateSession(ctx, v1.SessionCreateParams{CommandID: "cmd_1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// The agent's own request id is a JSON value, so a string id is stored quoted.
	request := permissionRequestFor(t, created.SessionID, created.RunID,
		`"agent-req-1"`, time.Now().UTC().Add(time.Hour))
	if err := store.WriteTx(ctx, func(tx storage.Execer) error {
		return store.InsertPermissionRequest(ctx, tx, request)
	}); err != nil {
		t.Fatalf("InsertPermissionRequest: %v", err)
	}

	pending, err := c.ListPermissions(ctx, v1.PermissionListParams{})
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	if len(pending.Permissions) != 1 {
		t.Fatalf("permissions = %+v", pending.Permissions)
	}

	// A caller types the id the way it is displayed, without the JSON quoting.
	if err := c.RespondToPermission(ctx, v1.PermissionRespondParams{
		AgentRequestID: control.UnquoteRequestID(pending.Permissions[0].AgentRequestID),
		SessionID:      created.SessionID,
		Approved:       true,
	}); err != nil {
		t.Fatalf("RespondToPermission: %v", err)
	}

	resolved, err := store.GetPermissionRequest(ctx, request.ID)
	if err != nil {
		t.Fatalf("GetPermissionRequest: %v", err)
	}
	if resolved.State != "approved" {
		t.Fatalf("state = %q, want approved", resolved.State)
	}

	// The request no longer appears as pending.
	after, err := c.ListPermissions(ctx, v1.PermissionListParams{})
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	if len(after.Permissions) != 0 {
		t.Fatalf("permissions = %+v, want none", after.Permissions)
	}

	// A second answer observes the resolved state instead of authorizing twice.
	if _, won, err := store.ResolvePermissionRequest(ctx, request.ID, "denied"); err != nil {
		t.Fatalf("ResolvePermissionRequest: %v", err)
	} else if won {
		t.Fatal("a second answer won after the request was resolved")
	}
}

// A caller may pass the agent request id with or without the JSON quoting.
func TestPermissionRequestIDQuotingIsForgiving(t *testing.T) {
	cases := map[string]string{
		`"abc"`:     "abc",
		`abc`:       "abc",
		`  "abc"  `: "abc",
		`123`:       "123",
		``:          "",
	}
	for stored, want := range cases {
		if got := control.UnquoteRequestID(stored); got != want {
			t.Errorf("UnquoteRequestID(%q) = %q, want %q", stored, got, want)
		}
	}
}
