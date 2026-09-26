package storage_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thupham/hive/internal/event"
)

// The newest report of a kind is what a status read wants, without reading the
// whole stream to find it.
func TestLatestEventOfType(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	appendEvents(t, s,
		newEvent(testSession, "ev_1"),
		typedEvent(testSession, "ev_2", event.TypeUsage, `{"totalTokens":10}`),
		typedEvent(testSession, "ev_3", event.TypeUsage, `{"totalTokens":20}`),
	)

	latest, err := s.LatestEventOfType(ctx, testSession, event.TypeUsage)
	if err != nil {
		t.Fatalf("LatestEventOfType: %v", err)
	}
	if latest == nil || latest.ID != "ev_3" {
		t.Fatalf("latest = %+v, want ev_3", latest)
	}

	// A type the stream never carried is not an error.
	none, err := s.LatestEventOfType(ctx, testSession, event.TypeTool)
	if err != nil {
		t.Fatalf("LatestEventOfType: %v", err)
	}
	if none != nil {
		t.Fatalf("latest = %+v, want none", none)
	}
}

// A tool call is published once per state change, so the count is of calls, not
// of events.
func TestToolCallCountCountsCallsNotUpdates(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	createSession(t, s, testSession)

	appendEvents(t, s,
		typedEvent(testSession, "ev_1", event.TypeTool, `{"toolCallId":"a","status":"pending"}`),
		typedEvent(testSession, "ev_2", event.TypeTool, `{"toolCallId":"a","status":"completed"}`),
		typedEvent(testSession, "ev_3", event.TypeTool, `{"toolCallId":"b","status":"pending"}`),
		newEvent(testSession, "ev_4"),
	)

	got, err := s.ToolCallCount(ctx, testSession)
	if err != nil {
		t.Fatalf("ToolCallCount: %v", err)
	}
	if got != 2 {
		t.Fatalf("tool calls = %d, want 2", got)
	}
}

func typedEvent(sessionID, id, eventType, payload string) *event.Event {
	return &event.Event{
		ID:        id,
		SessionID: sessionID,
		Type:      eventType,
		Version:   1,
		Payload:   json.RawMessage(payload),
	}
}
