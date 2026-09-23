package storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/thupham/hive/internal/agent"
	"github.com/thupham/hive/internal/command"
	"github.com/thupham/hive/internal/event"
	"github.com/thupham/hive/internal/session"
	"github.com/thupham/hive/internal/storage"
)

const testSession = "sess_1"

func openStore(t *testing.T) *storage.Store {
	t.Helper()
	return openStoreAt(t, filepath.Join(t.TempDir(), "coordinator.db"))
}

func openStoreAt(t *testing.T, path string) *storage.Store {
	t.Helper()
	s, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func createSession(t *testing.T, s *storage.Store, id string) *session.Session {
	t.Helper()
	ctx := context.Background()
	sess := session.New(id)
	err := s.WriteTx(ctx, func(tx storage.Execer) error {
		return s.InsertSession(ctx, tx, sess)
	})
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
	return sess
}

func newEvent(sessionID, id string) *event.Event {
	return &event.Event{
		ID:        id,
		SessionID: sessionID,
		Type:      event.TypeMessage,
		Version:   1,
		Payload:   json.RawMessage(`{"text":"hi"}`),
	}
}

func appendEvents(t *testing.T, s *storage.Store, evs ...*event.Event) int {
	t.Helper()
	ctx := context.Background()
	inserted := 0
	err := s.WriteTx(ctx, func(tx storage.Execer) error {
		n, err := s.AppendEvents(ctx, tx, evs...)
		inserted = n
		return err
	})
	if err != nil {
		t.Fatalf("append events: %v", err)
	}
	return inserted
}

func TestOpenAppliesMigrations(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	version, err := s.MigrationVersion(ctx)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if version != 2 {
		t.Fatalf("migration version = %d, want 2", version)
	}

	tables, err := s.Tables(ctx)
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	want := []string{
		"agent_runs", "capability_registrations", "commands", "conversation_bindings",
		"events", "handoffs", "outbox", "permission_requests", "plugin_instances",
		"plugins", "schema_migrations", "session_snapshots", "sessions",
		"stream_watermarks", "workspace_locations", "workspaces",
	}
	if len(tables) != len(want) {
		t.Fatalf("tables = %v, want %v", tables, want)
	}
	for i := range want {
		if tables[i] != want[i] {
			t.Fatalf("tables = %v, want %v", tables, want)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coordinator.db")

	first := openStoreAt(t, path)
	tablesBefore, err := first.Tables(context.Background())
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := openStoreAt(t, path)
	version, err := second.MigrationVersion(context.Background())
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if version != 2 {
		t.Fatalf("migration version after reopen = %d, want 2", version)
	}
	tablesAfter, err := second.Tables(context.Background())
	if err != nil {
		t.Fatalf("Tables: %v", err)
	}
	if len(tablesAfter) != len(tablesBefore) {
		t.Fatalf("tables after reopen = %v, want %v", tablesAfter, tablesBefore)
	}
}

func TestEventSequenceIsMonotonic(t *testing.T) {
	s := openStore(t)
	createSession(t, s, testSession)

	evs := []*event.Event{
		newEvent(testSession, "ev_1"),
		newEvent(testSession, "ev_2"),
		newEvent(testSession, "ev_3"),
	}
	if got := appendEvents(t, s, evs...); got != 3 {
		t.Fatalf("inserted = %d, want 3", got)
	}
	for i, ev := range evs {
		if ev.Sequence != int64(i+1) {
			t.Errorf("event %s sequence = %d, want %d", ev.ID, ev.Sequence, i+1)
		}
	}

	stored, err := s.ReadEvents(context.Background(), testSession, 0, 10)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(stored) != 3 {
		t.Fatalf("stored = %d events, want 3", len(stored))
	}
	for i, ev := range stored {
		if ev.Sequence != int64(i+1) {
			t.Errorf("stored event %d sequence = %d, want %d", i, ev.Sequence, i+1)
		}
	}
}

func TestEventIdempotency(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	if got := appendEvents(t, s, newEvent(testSession, "ev_1"), newEvent(testSession, "ev_2")); got != 2 {
		t.Fatalf("first append inserted = %d, want 2", got)
	}

	// ev_2 is a redelivery: it must not become a second durable event and
	// must not consume a sequence number.
	third := newEvent(testSession, "ev_3")
	if got := appendEvents(t, s, newEvent(testSession, "ev_2"), third); got != 1 {
		t.Fatalf("second append inserted = %d, want 1", got)
	}
	if third.Sequence != 3 {
		t.Fatalf("ev_3 sequence = %d, want 3", third.Sequence)
	}

	count, err := s.EventCount(ctx, testSession)
	if err != nil {
		t.Fatalf("EventCount: %v", err)
	}
	if count != 3 {
		t.Fatalf("event count = %d, want 3", count)
	}

	pending, err := s.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("outbox = %d records, want 3 (a duplicate must not enqueue twice)", len(pending))
	}
}

func TestEventSequenceIsMonotonicUnderConcurrency(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	const (
		workers   = 8
		perWorker = 25
		total     = workers * perWorker
	)

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				ev := newEvent(testSession, fmt.Sprintf("ev_%d_%d", w, i))
				err := s.WriteTx(ctx, func(tx storage.Execer) error {
					_, err := s.AppendEvents(ctx, tx, ev)
					return err
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}

	count, err := s.EventCount(ctx, testSession)
	if err != nil {
		t.Fatalf("EventCount: %v", err)
	}
	if count != total {
		t.Fatalf("event count = %d, want %d", count, total)
	}

	stored, err := s.ReadEvents(ctx, testSession, 0, total+10)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(stored) != total {
		t.Fatalf("read %d events, want %d", len(stored), total)
	}

	seen := make([]int64, 0, total)
	for _, ev := range stored {
		seen = append(seen, ev.Sequence)
	}
	if !sort.SliceIsSorted(seen, func(i, j int) bool { return seen[i] < seen[j] }) {
		t.Fatal("events were not returned in stream order")
	}
	for i, seq := range seen {
		if seq != int64(i+1) {
			t.Fatalf("sequence at position %d = %d, want %d (gap or duplicate)", i, seq, i+1)
		}
	}
}

func TestAcceptCommandIsIdempotent(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	cmd := command.New("cmd_1", "slack:U1", "session.create")
	cmd.SourceID = "slack:ev:1"
	stored, created, err := s.AcceptCommand(ctx, cmd, []*event.Event{newEvent(testSession, "ev_1")})
	if err != nil {
		t.Fatalf("AcceptCommand: %v", err)
	}
	if !created {
		t.Fatal("first AcceptCommand should create the operation")
	}
	if stored.ID != "cmd_1" || stored.State != command.StateReceived {
		t.Fatalf("stored = %+v", stored)
	}

	// A retry of the same logical operation must not create a second one and
	// must not append its events.
	retry := command.New("cmd_1", "slack:U1", "session.create")
	again, created, err := s.AcceptCommand(ctx, retry, []*event.Event{newEvent(testSession, "ev_2")})
	if err != nil {
		t.Fatalf("retry AcceptCommand: %v", err)
	}
	if created {
		t.Fatal("retry must not create a second operation")
	}
	if again.ID != "cmd_1" {
		t.Fatalf("retry returned %q, want cmd_1", again.ID)
	}

	count, err := s.EventCount(ctx, testSession)
	if err != nil {
		t.Fatalf("EventCount: %v", err)
	}
	if count != 1 {
		t.Fatalf("event count = %d, want 1 (retry must not append events)", count)
	}
}

func TestAcceptCommandRollsBackOnFailure(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	// An event with no id fails validation after the command row was written
	// in the same transaction, so the whole transaction must roll back.
	bad := &event.Event{SessionID: testSession, Type: event.TypeMessage, Version: 1}
	_, _, err := s.AcceptCommand(ctx, command.New("cmd_2", "a", "m"), []*event.Event{bad})
	if err == nil {
		t.Fatal("expected AcceptCommand to fail")
	}

	if _, err := s.GetCommand(ctx, "cmd_2"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("command survived rollback: err = %v", err)
	}
	count, err := s.EventCount(ctx, testSession)
	if err != nil {
		t.Fatalf("EventCount: %v", err)
	}
	if count != 0 {
		t.Fatalf("event count = %d, want 0 after rollback", count)
	}
}

func TestCommandStateTransition(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	if _, _, err := s.AcceptCommand(ctx, command.New("cmd_3", "a", "m"), nil); err != nil {
		t.Fatalf("AcceptCommand: %v", err)
	}

	result := json.RawMessage(`{"session_id":"sess_1"}`)
	err := s.WriteTx(ctx, func(tx storage.Execer) error {
		return s.SetCommandState(ctx, tx, "cmd_3", command.StateCompleted, result, nil)
	})
	if err != nil {
		t.Fatalf("SetCommandState: %v", err)
	}

	got, err := s.GetCommand(ctx, "cmd_3")
	if err != nil {
		t.Fatalf("GetCommand: %v", err)
	}
	if got.State != command.StateCompleted {
		t.Errorf("state = %q, want completed", got.State)
	}
	if string(got.Result) != string(result) {
		t.Errorf("result = %s, want %s", got.Result, result)
	}
	if !got.IsTerminal() {
		t.Error("completed command should be terminal")
	}

	err = s.WriteTx(ctx, func(tx storage.Execer) error {
		return s.SetCommandState(ctx, tx, "absent", command.StateFailed, nil, nil)
	})
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSnapshotSchemaVersion(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	snap := &session.Snapshot{
		ID:            "snap_1",
		SessionID:     testSession,
		SchemaVersion: 3,
		Sequence:      7,
		Payload:       json.RawMessage(`{"summary":"design done"}`),
	}
	err := s.WriteTx(ctx, func(tx storage.Execer) error {
		return s.PutSnapshot(ctx, tx, snap)
	})
	if err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}

	got, err := s.LatestSnapshot(ctx, testSession)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if got.SchemaVersion != 3 || got.Sequence != 7 {
		t.Fatalf("snapshot = %+v", got)
	}

	// A snapshot must identify its context schema version.
	err = s.WriteTx(ctx, func(tx storage.Execer) error {
		return s.PutSnapshot(ctx, tx, &session.Snapshot{ID: "snap_2", SessionID: testSession, Sequence: 8})
	})
	if err == nil {
		t.Fatal("expected a snapshot without a schema version to be rejected")
	}

	if _, err := s.LatestSnapshot(ctx, "sess_absent"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestOutboxLifecycle(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	appendEvents(t, s, newEvent(testSession, "ev_1"), newEvent(testSession, "ev_2"))

	pending, err := s.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d, want 2", len(pending))
	}

	err = s.WriteTx(ctx, func(tx storage.Execer) error {
		return s.MarkPublished(ctx, tx, pending[0].ID, pending[1].ID)
	})
	if err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}

	pending, err = s.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %d after publish, want 0", len(pending))
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	// The session does not exist, so the event insert must fail. This also
	// proves the connection pragmas were applied to a pooled connection.
	err := s.WriteTx(ctx, func(tx storage.Execer) error {
		_, err := s.AppendEvents(ctx, tx, newEvent("sess_missing", "ev_1"))
		return err
	})
	if err == nil {
		t.Fatal("expected a foreign key violation")
	}
}

func TestAgentRunPersistsExecutionGeneration(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	run := agent.New("run_1", testSession, "devin", "acp")
	run.NodeID = "node_a"
	if err := s.WriteTx(ctx, func(tx storage.Execer) error {
		return s.InsertAgentRun(ctx, tx, run)
	}); err != nil {
		t.Fatalf("InsertAgentRun: %v", err)
	}

	loaded, err := s.GetAgentRun(ctx, "run_1")
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if loaded.ExecutionGeneration != 1 || loaded.State != agent.StateCreated {
		t.Fatalf("loaded = %+v", loaded)
	}

	// Interrupt, then recover: the generation must be durable and bump.
	if err := loaded.Transition(agent.StateStarting); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := loaded.Transition(agent.StateInterrupted); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := loaded.Recover("node_exec_2"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if err := s.WriteTx(ctx, func(tx storage.Execer) error {
		return s.UpdateAgentRun(ctx, tx, loaded)
	}); err != nil {
		t.Fatalf("UpdateAgentRun: %v", err)
	}

	reloaded, err := s.GetAgentRun(ctx, "run_1")
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if reloaded.ExecutionGeneration != 2 {
		t.Fatalf("generation = %d, want 2", reloaded.ExecutionGeneration)
	}
	if reloaded.NodeExecutionID != "node_exec_2" {
		t.Fatalf("node execution id = %q", reloaded.NodeExecutionID)
	}
	if err := reloaded.CheckGeneration(1); !errors.Is(err, agent.ErrStaleGeneration) {
		t.Fatalf("old generation was not fenced: %v", err)
	}

	runs, err := s.ListAgentRuns(ctx, testSession)
	if err != nil {
		t.Fatalf("ListAgentRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
}

func TestUpdateSessionPersistsRoutingPointer(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	sess := createSession(t, s, testSession)

	if err := sess.SetDefaultInteractiveRun("run_1"); err != nil {
		t.Fatalf("SetDefaultInteractiveRun: %v", err)
	}
	if err := sess.Transition(session.StateIdle); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if err := s.WriteTx(ctx, func(tx storage.Execer) error {
		return s.UpdateSession(ctx, tx, sess)
	}); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}

	loaded, err := s.GetSession(ctx, testSession)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if loaded.DefaultInteractiveRunID != "run_1" {
		t.Errorf("default run = %q, want run_1", loaded.DefaultInteractiveRunID)
	}
	if loaded.State != session.StateIdle {
		t.Errorf("state = %q, want idle", loaded.State)
	}

	err = s.WriteTx(ctx, func(tx storage.Execer) error {
		return s.UpdateSession(ctx, tx, session.New("sess_absent"))
	})
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestSessionContinuityAcrossRuns checks the central claim of the session
// model: a completed AgentRun does not destroy or end the Hive Session.
func TestSessionContinuityAcrossRuns(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	for _, id := range []string{"run_1", "run_2"} {
		run := agent.New(id, testSession, "devin", "acp")
		if err := s.WriteTx(ctx, func(tx storage.Execer) error {
			return s.InsertAgentRun(ctx, tx, run)
		}); err != nil {
			t.Fatalf("InsertAgentRun %s: %v", id, err)
		}

		for _, next := range []agent.State{agent.StateStarting, agent.StateRunning, agent.StateCompleted} {
			if err := run.Transition(next); err != nil {
				t.Fatalf("run %s transition to %s: %v", id, next, err)
			}
		}
		if err := s.WriteTx(ctx, func(tx storage.Execer) error {
			return s.UpdateAgentRun(ctx, tx, run)
		}); err != nil {
			t.Fatalf("UpdateAgentRun %s: %v", id, err)
		}
	}

	sess, err := s.GetSession(ctx, testSession)
	if err != nil {
		t.Fatalf("session did not survive completed runs: %v", err)
	}
	if sess.State != session.StateActive {
		t.Errorf("session state = %q, want active", sess.State)
	}

	runs, err := s.ListAgentRuns(ctx, testSession)
	if err != nil {
		t.Fatalf("ListAgentRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	for _, run := range runs {
		if !run.IsTerminal() {
			t.Errorf("run %s state = %q, want terminal", run.ID, run.State)
		}
	}
}
