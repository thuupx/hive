package storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/thuupx/hive/internal/permission"
	"github.com/thuupx/hive/internal/storage"
)

func insertPermission(t *testing.T, s *storage.Store, req *permission.Request) {
	t.Helper()
	ctx := context.Background()
	err := s.WriteTx(ctx, func(tx storage.Execer) error {
		return s.InsertPermissionRequest(ctx, tx, req)
	})
	if err != nil {
		t.Fatalf("InsertPermissionRequest %s: %v", req.ID, err)
	}
}

func TestPermissionRequestRoundTrip(t *testing.T) {
	s := openStore(t)
	createSession(t, s, testSession)

	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	req := permission.New("perm_1", testSession, "run_1", "agent-req-1",
		json.RawMessage(`{"toolCall":{"title":"Write file"}}`), expires)
	insertPermission(t, s, req)

	loaded, err := s.GetPermissionRequest(context.Background(), "perm_1")
	if err != nil {
		t.Fatalf("GetPermissionRequest: %v", err)
	}
	if loaded.State != permission.StatePending {
		t.Errorf("state = %q", loaded.State)
	}
	if loaded.RunID != "run_1" || loaded.AgentRequestID != "agent-req-1" {
		t.Errorf("loaded = %+v", loaded)
	}
	if !loaded.ExpiresAt.Equal(expires) {
		t.Errorf("expires = %v, want %v", loaded.ExpiresAt, expires)
	}
	// The agent payload is preserved as opaque protocol data.
	if string(loaded.Payload) != string(req.Payload) {
		t.Errorf("payload = %s, want %s", loaded.Payload, req.Payload)
	}

	if _, err := s.GetPermissionRequest(context.Background(), "absent"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Concurrent responses must resolve to exactly one terminal transition.
func TestResolvePermissionRequestIsFirstWins(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)
	insertPermission(t, s, permission.New("perm_1", testSession, "run_1", "agent-req-1", nil, time.Time{}))

	const responders = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  int
		state permission.State
	)
	for i := 0; i < responders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			next := permission.StateApproved
			if i%2 == 1 {
				next = permission.StateDenied
			}
			req, won, err := s.ResolvePermissionRequest(ctx, "perm_1", next)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if won {
				wins++
			}
			state = req.State
		}(i)
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1", wins)
	}
	if !state.IsTerminal() {
		t.Fatalf("state = %q, want a terminal state", state)
	}

	// Repeating the same response must not win again.
	req, won, err := s.ResolvePermissionRequest(ctx, "perm_1", state)
	if err != nil {
		t.Fatalf("ResolvePermissionRequest: %v", err)
	}
	if won {
		t.Fatal("a repeated response must not win again")
	}
	if req.State != state {
		t.Fatalf("state = %q, want %q", req.State, state)
	}
}

func TestResolvePermissionRequestRejectsNonTerminal(t *testing.T) {
	s := openStore(t)
	createSession(t, s, testSession)

	if _, _, err := s.ResolvePermissionRequest(context.Background(), "perm_1", permission.StatePending); err == nil {
		t.Fatal("a non-terminal target must be rejected")
	}
}

func TestExpirePermissionRequests(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	now := time.Now().UTC()
	insertPermission(t, s, permission.New("perm_due", testSession, "run_1", "r1", nil, now.Add(-time.Minute)))
	insertPermission(t, s, permission.New("perm_later", testSession, "run_1", "r2", nil, now.Add(time.Hour)))
	insertPermission(t, s, permission.New("perm_forever", testSession, "run_1", "r3", nil, time.Time{}))

	expired, err := s.ExpirePermissionRequests(ctx, now, 100)
	if err != nil {
		t.Fatalf("ExpirePermissionRequests: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired = %d, want 1", expired)
	}

	due, err := s.GetPermissionRequest(ctx, "perm_due")
	if err != nil {
		t.Fatalf("GetPermissionRequest: %v", err)
	}
	if due.State != permission.StateExpired {
		t.Fatalf("state = %q, want expired", due.State)
	}
	if due.State == permission.StateApproved {
		t.Fatal("expiry must never be an approval")
	}

	for _, id := range []string{"perm_later", "perm_forever"} {
		req, err := s.GetPermissionRequest(ctx, id)
		if err != nil {
			t.Fatalf("GetPermissionRequest %s: %v", id, err)
		}
		if req.State != permission.StatePending {
			t.Errorf("%s state = %q, want pending", id, req.State)
		}
	}
}

func TestExpireDoesNotOverrideADecision(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	now := time.Now().UTC()
	insertPermission(t, s, permission.New("perm_1", testSession, "run_1", "r1", nil, now.Add(-time.Minute)))

	if _, won, err := s.ResolvePermissionRequest(ctx, "perm_1", permission.StateApproved); err != nil || !won {
		t.Fatalf("resolve = %v, %v", won, err)
	}
	if _, err := s.ExpirePermissionRequests(ctx, now, 100); err != nil {
		t.Fatalf("ExpirePermissionRequests: %v", err)
	}

	req, err := s.GetPermissionRequest(ctx, "perm_1")
	if err != nil {
		t.Fatalf("GetPermissionRequest: %v", err)
	}
	if req.State != permission.StateApproved {
		t.Fatalf("state = %q, want the existing decision to stand", req.State)
	}
}

// A pending permission request is durable coordinator state, so it survives a
// restart and a replacement coordinator can recover it.
func TestOpenPermissionRequestsSurviveReopen(t *testing.T) {
	path := t.TempDir() + "/coordinator.db"

	first := openStoreAt(t, path)
	createSession(t, first, testSession)
	insertPermission(t, first, permission.New("perm_1", testSession, "run_1", "agent-req-1", nil, time.Time{}))
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := openStoreAt(t, path)
	open, err := second.ListOpenPermissionRequests(context.Background(), testSession)
	if err != nil {
		t.Fatalf("ListOpenPermissionRequests: %v", err)
	}
	if len(open) != 1 || open[0].ID != "perm_1" {
		t.Fatalf("open = %+v", open)
	}
	if open[0].AgentRequestID != "agent-req-1" {
		t.Errorf("agent request id = %q", open[0].AgentRequestID)
	}
}
