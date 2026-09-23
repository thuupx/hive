package event

import (
	"encoding/json"
	"testing"
)

// Unknown protocol data must survive: a new agent protocol feature must not
// require a new core event type or an immediate schema change.
func TestRawPreservesProtocolData(t *testing.T) {
	payload := json.RawMessage(`{"future":"thing","nested":{"a":1}}`)
	ev := Raw("sess_1", "acp", "future/thing", payload)

	if ev.Type != TypeAgentRaw {
		t.Errorf("type = %q, want %q", ev.Type, TypeAgentRaw)
	}
	if ev.Protocol != "acp" {
		t.Errorf("protocol = %q", ev.Protocol)
	}
	if ev.Method != "future/thing" {
		t.Errorf("method = %q", ev.Method)
	}
	if string(ev.Payload) != string(payload) {
		t.Errorf("payload = %s, want %s", ev.Payload, payload)
	}
	if ev.Version < 1 {
		t.Errorf("version = %d, want at least 1", ev.Version)
	}
	if ev.SessionID != "sess_1" {
		t.Errorf("session = %q", ev.SessionID)
	}
}

func TestCoreEventTypesAreDistinct(t *testing.T) {
	types := []string{
		TypeMessage, TypeStatus, TypeRunStarted, TypeRunFinished,
		TypePermissionRequested, TypePermissionResponded,
		TypeHandoffCreated, TypeError, TypeAgentRaw,
	}
	seen := map[string]bool{}
	for _, typ := range types {
		if seen[typ] {
			t.Errorf("duplicate core event type %q", typ)
		}
		seen[typ] = true
	}
}
