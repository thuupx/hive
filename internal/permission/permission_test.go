package permission

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func newPending(id string, expiresAt time.Time) *Request {
	return New(id, "sess_1", "run_1", "agent-req-1", json.RawMessage(`{"toolCall":{"title":"Write file"}}`), expiresAt)
}

func TestNewStartsPending(t *testing.T) {
	req := newPending("perm_1", time.Time{})
	if req.State != StatePending {
		t.Fatalf("state = %q, want pending", req.State)
	}
	if !req.IsPending() || req.State.IsTerminal() {
		t.Fatal("a new request should be pending and not terminal")
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestResolveIsFirstWins(t *testing.T) {
	req := newPending("perm_1", time.Time{})

	won, err := req.Resolve(StateApproved)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !won {
		t.Fatal("the first terminal transition must win")
	}
	if req.ResolvedAt.IsZero() {
		t.Error("ResolvedAt should be set")
	}

	// A later response observes the resolved state and must not produce a
	// second authorization.
	won, err = req.Resolve(StateDenied)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if won {
		t.Fatal("a later response must not win")
	}
	if req.State != StateApproved {
		t.Fatalf("state = %q, want the first decision to stand", req.State)
	}
}

func TestResolveRejectsNonTerminalAndUnknown(t *testing.T) {
	req := newPending("perm_1", time.Time{})

	if _, err := req.Resolve(StatePending); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if _, err := req.Resolve("maybe"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if req.State != StatePending {
		t.Fatalf("state = %q, want pending", req.State)
	}
}

func TestResolveIsIdempotentForTheSameDecision(t *testing.T) {
	req := newPending("perm_1", time.Time{})

	if won, _ := req.Resolve(StateApproved); !won {
		t.Fatal("first resolve should win")
	}
	won, err := req.Resolve(StateApproved)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if won {
		t.Fatal("repeating the same response must not win again")
	}
}

// A pending request must never become an implicit approval, including when the
// coordinator connection is lost.
func TestExpireRequiresTheDeadlineToHavePassed(t *testing.T) {
	now := time.Now().UTC()
	req := newPending("perm_1", now.Add(time.Hour))

	if _, err := req.Expire(now); err == nil {
		t.Fatal("a request inside its deadline must not expire")
	}
	if req.State != StatePending {
		t.Fatalf("state = %q, want pending", req.State)
	}

	won, err := req.Expire(now.Add(2 * time.Hour))
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if !won {
		t.Fatal("expiry should resolve the request")
	}
	if req.State != StateExpired {
		t.Fatalf("state = %q, want expired", req.State)
	}
	if req.State == StateApproved {
		t.Fatal("expiry must never be an approval")
	}
}

func TestExpireDoesNotOverrideAResolvedRequest(t *testing.T) {
	now := time.Now().UTC()
	req := newPending("perm_1", now.Add(-time.Minute))

	if won, _ := req.Resolve(StateDenied); !won {
		t.Fatal("first resolve should win")
	}
	won, err := req.Expire(now)
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if won {
		t.Fatal("expiry must not override an existing decision")
	}
	if req.State != StateDenied {
		t.Fatalf("state = %q, want denied", req.State)
	}
}

func TestNoDeadlineNeverExpires(t *testing.T) {
	req := newPending("perm_1", time.Time{})
	if req.IsExpired(time.Now().UTC().Add(1000 * time.Hour)) {
		t.Fatal("a request without a deadline must not expire")
	}
}

func TestStateOfDecision(t *testing.T) {
	if got := StateOf(Decision{Approved: true}); got != StateApproved {
		t.Errorf("approved -> %q", got)
	}
	if got := StateOf(Decision{Approved: false}); got != StateDenied {
		t.Errorf("denied -> %q", got)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		req  *Request
		ok   bool
	}{
		{"valid", newPending("perm_1", time.Time{}), true},
		{"nil", nil, false},
		{"no id", &Request{SessionID: "s", AgentRequestID: "a", State: StatePending}, false},
		{"no session", &Request{ID: "p", AgentRequestID: "a", State: StatePending}, false},
		{"no agent request", &Request{ID: "p", SessionID: "s", State: StatePending}, false},
		{"unknown state", &Request{ID: "p", SessionID: "s", AgentRequestID: "a", State: "perhaps"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}
