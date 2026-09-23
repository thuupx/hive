package control

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/thupham/hive/internal/command"
	"github.com/thupham/hive/internal/event"
	"github.com/thupham/hive/internal/node"
	"github.com/thupham/hive/internal/session"
	"github.com/thupham/hive/internal/storage"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

func newService(t *testing.T, allowedUsers ...string) (*Service, *storage.Store) {
	t.Helper()

	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	service, err := New(Options{
		Store:        store,
		Nodes:        node.NewServer(context.Background(), nil),
		Agents:       []string{"claude"},
		DefaultAgent: "claude",
		AllowedUsers: allowedUsers,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service, store
}

func seedSession(t *testing.T, store *storage.Store, id string, principals ...string) {
	t.Helper()
	ctx := context.Background()

	if err := store.WriteTx(ctx, func(tx storage.Execer) error {
		return store.InsertSession(ctx, tx, session.New(id))
	}); err != nil {
		t.Fatalf("InsertSession %s: %v", id, err)
	}

	for i, principal := range principals {
		binding := &storage.Binding{
			ID:             id + "-b" + string(rune('0'+i)),
			SessionID:      id,
			Transport:      "slack",
			ConversationID: id + "-c" + string(rune('0'+i)),
			Principal:      principal,
		}
		if err := store.WriteTx(ctx, func(tx storage.Execer) error {
			return store.UpsertBinding(ctx, tx, binding)
		}); err != nil {
			t.Fatalf("UpsertBinding: %v", err)
		}
	}
}

func TestStatusRejectsAnUnknownSession(t *testing.T) {
	service, _ := newService(t)

	_, err := service.Status(context.Background(), OwnerPrincipal, "sess_missing")
	if err == nil {
		t.Fatal("expected an unknown session to be rejected")
	}
	if e := v1.AsError(err); e.Code != v1.CodeNotFound {
		t.Fatalf("code = %d, want %d", e.Code, v1.CodeNotFound)
	}
}

// Session isolation: a non-owner reaches a session only through a binding it
// holds, and learns nothing about the sessions it cannot reach.
func TestSessionIsolation(t *testing.T) {
	ctx := context.Background()
	service, store := newService(t, "slack:U1", "slack:U2")

	seedSession(t, store, "sess_a", "slack:U1")
	seedSession(t, store, "sess_b", "slack:U2")

	if _, err := service.Status(ctx, "slack:U1", "sess_a"); err != nil {
		t.Fatalf("U1 must reach its own session: %v", err)
	}

	_, err := service.Status(ctx, "slack:U1", "sess_b")
	if err == nil {
		t.Fatal("U1 must not reach another principal's session")
	}
	if e := v1.AsError(err); e.Code != v1.CodeNotFound {
		t.Fatalf("code = %d, want %d so the session's existence is not revealed", e.Code, v1.CodeNotFound)
	}

	list, err := service.ListSessions(ctx, "slack:U1", 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].SessionID != "sess_a" {
		t.Fatalf("sessions = %+v, want only sess_a", list.Sessions)
	}

	// The owner sees everything.
	all, err := service.ListSessions(ctx, OwnerPrincipal, 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(all.Sessions) != 2 {
		t.Fatalf("owner sessions = %d, want 2", len(all.Sessions))
	}
}

func TestCommandStatusIsScopedToItsIssuer(t *testing.T) {
	ctx := context.Background()
	service, store := newService(t, "slack:U1", "slack:U2")

	cmd := command.New("cmd_1", "slack:U1", v1.MethodSessionCreate)
	cmd.State = command.StateAccepted
	if _, _, err := store.AcceptCommand(ctx, cmd, nil); err != nil {
		t.Fatalf("AcceptCommand: %v", err)
	}

	if _, err := service.GetCommand(ctx, "slack:U1", "cmd_1"); err != nil {
		t.Fatalf("the issuer must read its own command: %v", err)
	}

	_, err := service.GetCommand(ctx, "slack:U2", "cmd_1")
	if err == nil {
		t.Fatal("another principal must not read the command")
	}
	if e := v1.AsError(err); e.Code != v1.CodeNotFound {
		t.Fatalf("code = %d, want %d", e.Code, v1.CodeNotFound)
	}
}

// Authorization is evaluated before anything else, so a denied caller cannot
// reach a handler at all.
func TestAuthorizationRunsFirst(t *testing.T) {
	ctx := context.Background()
	service, store := newService(t)

	seedSession(t, store, "sess_a")

	_, err := service.Status(ctx, "stranger", "sess_a")
	if err == nil {
		t.Fatal("expected denial")
	}
	if e := v1.AsError(err); e.Code != v1.CodeUnauthorized {
		t.Fatalf("code = %d, want %d", e.Code, v1.CodeUnauthorized)
	}
}

func TestCreateSessionRequiresANode(t *testing.T) {
	service, _ := newService(t)

	// No node is connected, so creating a session that nothing can run must
	// fail rather than leave a dangling session behind.
	_, err := service.CreateSession(context.Background(), OwnerPrincipal, v1.SessionCreateParams{
		CommandID: "cmd_1",
	})
	if err == nil {
		t.Fatal("expected session.create to fail without a node")
	}
	if e := v1.AsError(err); e.Code != v1.CodeUnavailable {
		t.Fatalf("code = %d, want %d", e.Code, v1.CodeUnavailable)
	}
}

func TestCreateSessionRejectsAnUnknownAgent(t *testing.T) {
	service, _ := newService(t)

	_, err := service.CreateSession(context.Background(), OwnerPrincipal, v1.SessionCreateParams{
		CommandID: "cmd_1",
		AgentID:   "nobody",
	})
	if err == nil {
		t.Fatal("expected an unknown agent to be rejected")
	}
	if e := v1.AsError(err); e.Code != v1.CodeInvalidParams {
		t.Fatalf("code = %d, want %d", e.Code, v1.CodeInvalidParams)
	}
}

func TestCreateSessionRequiresACommandID(t *testing.T) {
	service, _ := newService(t)

	_, err := service.CreateSession(context.Background(), OwnerPrincipal, v1.SessionCreateParams{})
	if err == nil {
		t.Fatal("expected a missing command id to be rejected")
	}
}

func TestReplayReportsACursorGap(t *testing.T) {
	ctx := context.Background()
	service, store := newService(t)
	seedSession(t, store, "sess_a")

	// Prune an event stream so the cursor is no longer servable.
	appendEvent(t, store, "sess_a", "ev_1")
	appendEvent(t, store, "sess_a", "ev_2")
	if err := store.MarkPublishedBatch(ctx, "ev_1", "ev_2"); err != nil {
		t.Fatalf("MarkPublishedBatch: %v", err)
	}
	if _, err := store.PruneEvents(ctx, time.Now().Add(time.Hour), 100); err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}

	result, err := service.Replay(ctx, OwnerPrincipal, v1.EventReplayParams{
		SessionID:    "sess_a",
		FromSequence: 0,
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result.Gap == nil {
		t.Fatal("expected a cursor gap rather than an empty stream")
	}
	if result.Gap.NextSequence != 3 {
		t.Fatalf("next sequence = %d, want 3", result.Gap.NextSequence)
	}
}

func TestListAgentsAndNodes(t *testing.T) {
	ctx := context.Background()
	service, _ := newService(t)

	agents, err := service.ListAgents(ctx, OwnerPrincipal)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents.Agents) != 1 || agents.Agents[0].ID != "claude" {
		t.Fatalf("agents = %+v", agents.Agents)
	}

	nodes, err := service.ListNodes(ctx, OwnerPrincipal)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes.Nodes) != 0 {
		t.Fatalf("nodes = %+v, want none", nodes.Nodes)
	}
}

func appendEvent(t *testing.T, store *storage.Store, sessionID, eventID string) {
	t.Helper()
	ctx := context.Background()

	ev := &event.Event{
		ID:        eventID,
		SessionID: sessionID,
		Type:      event.TypeMessage,
		Version:   1,
		Payload:   json.RawMessage(`{}`),
	}
	if err := store.WriteTx(ctx, func(tx storage.Execer) error {
		_, err := store.AppendEvents(ctx, tx, ev)
		return err
	}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
}
