package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/thuupx/hive/internal/event"
	"github.com/thuupx/hive/internal/session"
	"github.com/thuupx/hive/internal/storage"
)

func TestEarliestSequence(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	if got, err := s.EarliestSequence(ctx, testSession); err != nil || got != 0 {
		t.Fatalf("empty stream earliest = %d, %v; want 0, nil", got, err)
	}

	appendEvents(t, s, newEvent(testSession, "ev_1"), newEvent(testSession, "ev_2"))
	if got, err := s.EarliestSequence(ctx, testSession); err != nil || got != 1 {
		t.Fatalf("earliest = %d, %v; want 1, nil", got, err)
	}
}

func TestReplayReturnsEventsAfterCursor(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	appendEvents(t, s,
		newEvent(testSession, "ev_1"),
		newEvent(testSession, "ev_2"),
		newEvent(testSession, "ev_3"),
		newEvent(testSession, "ev_4"),
	)

	result, err := s.Replay(ctx, testSession, 2, 10)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result.Gap != nil {
		t.Fatalf("unexpected gap: %+v", result.Gap)
	}
	if len(result.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(result.Events))
	}
	if result.Events[0].ID != "ev_3" || result.Events[1].ID != "ev_4" {
		t.Fatalf("events = %s, %s", result.Events[0].ID, result.Events[1].ID)
	}
}

func TestReplayReportsCursorGapWhenPruned(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	evs := []*event.Event{
		newEvent(testSession, "ev_1"),
		newEvent(testSession, "ev_2"),
		newEvent(testSession, "ev_3"),
	}
	appendEvents(t, s, evs...)

	// Publish the first two so they become eligible for pruning, then prune.
	err := s.MarkPublishedBatch(ctx, "ev_1", "ev_2")
	if err != nil {
		t.Fatalf("MarkPublishedBatch: %v", err)
	}
	deleted, err := s.PruneEvents(ctx, time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("pruned = %d, want 2", deleted)
	}
	if earliest, err := s.EarliestSequence(ctx, testSession); err != nil || earliest != 3 {
		t.Fatalf("earliest = %d, %v; want 3, nil", earliest, err)
	}

	// A client sitting at cursor 0 can no longer be served from history.
	result, err := s.Replay(ctx, testSession, 0, 10)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result.Gap == nil {
		t.Fatal("expected a cursor gap instead of silently skipped history")
	}
	if result.Gap.Requested != 0 || result.Gap.NextSequence != 3 {
		t.Fatalf("gap = %+v", result.Gap)
	}
	if len(result.Events) != 0 {
		t.Fatalf("events = %d, want 0 alongside a gap", len(result.Events))
	}

	// A cursor that is still valid is served normally.
	result, err = s.Replay(ctx, testSession, 3, 10)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result.Gap != nil {
		t.Fatalf("unexpected gap at a valid cursor: %+v", result.Gap)
	}
}

func TestReplayGapCarriesSnapshotBoundary(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	appendEvents(t, s, newEvent(testSession, "ev_1"), newEvent(testSession, "ev_2"))
	err := s.WriteTx(ctx, func(tx storage.Execer) error {
		return s.PutSnapshot(ctx, tx, &session.Snapshot{
			ID:            "snap_1",
			SessionID:     testSession,
			SchemaVersion: 1,
			Sequence:      2,
			Payload:       []byte(`{"summary":"so far"}`),
		})
	})
	if err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}

	if err := s.MarkPublishedBatch(ctx, "ev_1", "ev_2"); err != nil {
		t.Fatalf("MarkPublishedBatch: %v", err)
	}
	if _, err := s.PruneEvents(ctx, time.Now().Add(time.Hour), 100); err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}

	result, err := s.Replay(ctx, testSession, 0, 10)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result.Gap == nil {
		t.Fatal("expected a cursor gap")
	}
	if result.Gap.SnapshotID != "snap_1" || result.Gap.SnapshotSequence != 2 {
		t.Fatalf("gap = %+v, want the snapshot boundary", result.Gap)
	}
	if result.Gap.NextSequence != 3 {
		t.Fatalf("next sequence = %d, want 3", result.Gap.NextSequence)
	}
}

// Dropping an unpublished event would lose realtime delivery that has not
// happened yet.
func TestPruneEventsSkipsUnpublished(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	appendEvents(t, s,
		newEvent(testSession, "ev_1"),
		newEvent(testSession, "ev_2"),
		newEvent(testSession, "ev_3"),
	)
	if err := s.MarkPublishedBatch(ctx, "ev_1"); err != nil {
		t.Fatalf("MarkPublishedBatch: %v", err)
	}

	deleted, err := s.PruneEvents(ctx, time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("pruned = %d, want 1 (only the published event)", deleted)
	}

	count, err := s.EventCount(ctx, testSession)
	if err != nil {
		t.Fatalf("EventCount: %v", err)
	}
	if count != 2 {
		t.Fatalf("events = %d, want 2", count)
	}

	pending, err := s.PendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("PendingOutbox: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d, want 2", len(pending))
	}
}

// A fully pruned stream must not look like an empty stream: the client needs
// an explicit cursor_expired so it knows to rehydrate.
func TestReplayReportsGapWhenWholeStreamIsPruned(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	appendEvents(t, s, newEvent(testSession, "ev_1"), newEvent(testSession, "ev_2"))
	if err := s.MarkPublishedBatch(ctx, "ev_1", "ev_2"); err != nil {
		t.Fatalf("MarkPublishedBatch: %v", err)
	}
	deleted, err := s.PruneEvents(ctx, time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("pruned = %d, want 2", deleted)
	}

	if got, err := s.EarliestSequence(ctx, testSession); err != nil || got != 0 {
		t.Fatalf("earliest = %d, %v; want 0 because no events remain", got, err)
	}
	prunedThrough, err := s.PrunedThrough(ctx, testSession)
	if err != nil {
		t.Fatalf("PrunedThrough: %v", err)
	}
	if prunedThrough != 2 {
		t.Fatalf("pruned through = %d, want 2", prunedThrough)
	}

	result, err := s.Replay(ctx, testSession, 0, 10)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result.Gap == nil {
		t.Fatal("a fully pruned stream must report a cursor gap, not an empty stream")
	}
	if result.Gap.NextSequence != 3 {
		t.Fatalf("next sequence = %d, want 3", result.Gap.NextSequence)
	}
}

// A stream that was never pruned is legitimately empty and is not a gap.
func TestReplayOnEmptyStreamIsNotAGap(t *testing.T) {
	s := openStore(t)
	createSession(t, s, testSession)

	result, err := s.Replay(context.Background(), testSession, 0, 10)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result.Gap != nil {
		t.Fatalf("unexpected gap on an empty stream: %+v", result.Gap)
	}
	if len(result.Events) != 0 {
		t.Fatalf("events = %d, want 0", len(result.Events))
	}
}

func TestPruneEventsRespectsCutoff(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	appendEvents(t, s, newEvent(testSession, "ev_1"))
	if err := s.MarkPublishedBatch(ctx, "ev_1"); err != nil {
		t.Fatalf("MarkPublishedBatch: %v", err)
	}

	// A cutoff in the past must not prune a freshly written event.
	deleted, err := s.PruneEvents(ctx, time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("pruned = %d, want 0", deleted)
	}
}
