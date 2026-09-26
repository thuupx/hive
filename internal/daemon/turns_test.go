package daemon_test

import (
	"context"
	"testing"

	"github.com/thupham/hive/internal/permission"
	"github.com/thupham/hive/internal/plugin"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// A prompt that arrives while a turn is running waits behind it.
//
// Found by asking what happens when a user types again mid-turn: the second
// message was raced into the same agent session, and two prompts in flight are
// answered at once — the answers interleave into one, which reads as a garbled
// reply rather than as two.
func TestAPromptWhileATurnRunsIsQueued(t *testing.T) {
	// The agent holds its turn open on a permission request, so the second prompt
	// really does arrive while the first turn is running.
	store, peer := startStackWithAgentSpecs(t,
		[]string{testAgent},
		[]plugin.Spec{testAgentSpecWithEnv("HIVE_TEST_AGENT_BLOCK_ON_PERMISSION=1")},
	)
	ctx := context.Background()

	var created v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, v1.SessionCreateParams{CommandID: "cmd_1"}, &created); err != nil {
		t.Fatalf("session.create: %v", err)
	}

	// The first turn blocks on a permission request, which is what holds it open
	// while the second prompt arrives.
	if err := peer.Call(ctx, v1.MethodSessionPrompt, v1.SessionPromptParams{
		CommandID: "cmd_2", SessionID: created.SessionID, Text: "ask permission please",
	}, &v1.SessionPromptResult{}); err != nil {
		t.Fatalf("session.prompt: %v", err)
	}

	var pending []*permission.Request
	waitFor(t, "the permission to be recorded", func() bool {
		var err error
		pending, err = store.ListOpenPermissionRequests(ctx, created.SessionID)
		return err == nil && len(pending) == 1
	})

	var queued v1.SessionPromptResult
	if err := peer.Call(ctx, v1.MethodSessionPrompt, v1.SessionPromptParams{
		CommandID: "cmd_3", SessionID: created.SessionID, Text: "and then this",
	}, &queued); err != nil {
		t.Fatalf("session.prompt: %v", err)
	}
	if !queued.Queued {
		t.Fatalf("a prompt while a turn runs should be queued, got %+v", queued)
	}
	if queued.RunID != "" {
		t.Errorf("a queued prompt has no run yet, got %q", queued.RunID)
	}

	// Answering the permission lets the first turn finish, and the queued prompt
	// then runs — as a new run, because a run completes at the end of its turn.
	if err := peer.Call(ctx, v1.MethodPermissionRespond, v1.PermissionRespondParams{
		AgentRunID:     created.RunID,
		SessionID:      created.SessionID,
		AgentID:        created.AgentID,
		AgentRequestID: pending[0].AgentRequestID,
		Approved:       true,
	}, &map[string]any{}); err != nil {
		t.Fatalf("permission.respond: %v", err)
	}

	waitFor(t, "the queued turn to run", func() bool {
		runs, err := store.ListAgentRuns(ctx, created.SessionID)
		return err == nil && len(runs) == 2
	})
}

// A session with nothing running takes the prompt immediately.
func TestAPromptOnAnIdleSessionIsNotQueued(t *testing.T) {
	_, _, peer, _ := startStack(t)
	ctx := context.Background()

	var created v1.SessionCreateResult
	if err := peer.Call(ctx, v1.MethodSessionCreate, v1.SessionCreateParams{CommandID: "cmd_1"}, &created); err != nil {
		t.Fatalf("session.create: %v", err)
	}

	var prompted v1.SessionPromptResult
	if err := peer.Call(ctx, v1.MethodSessionPrompt, v1.SessionPromptParams{
		CommandID: "cmd_2", SessionID: created.SessionID, Text: "do the thing",
	}, &prompted); err != nil {
		t.Fatalf("session.prompt: %v", err)
	}
	if prompted.Queued {
		t.Fatalf("nothing was running, so the prompt should not be queued: %+v", prompted)
	}
	if prompted.RunID != created.RunID {
		t.Errorf("prompted run = %q, want %q", prompted.RunID, created.RunID)
	}
}
