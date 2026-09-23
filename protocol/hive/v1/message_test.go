package v1

import (
	"encoding/json"
	"testing"
)

func TestNewRequestShape(t *testing.T) {
	m, err := NewRequest(NumberID(1), "session.create", map[string]string{"agent": "devin"})
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if got := m.Kind(); got != KindRequest {
		t.Fatalf("Kind() = %v, want request", got)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(raw["jsonrpc"]) != `"2.0"` {
		t.Errorf("jsonrpc = %s", raw["jsonrpc"])
	}
	if string(raw["id"]) != "1" {
		t.Errorf("id = %s", raw["id"])
	}
	if _, ok := raw["result"]; ok {
		t.Error("request must not carry result")
	}
	if _, ok := raw["error"]; ok {
		t.Error("request must not carry error")
	}
}

func TestNewRequestOmitsNilParams(t *testing.T) {
	m, err := NewRequest(NumberID(1), "session.list", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	b, _ := json.Marshal(m)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(b, &raw)
	if _, ok := raw["params"]; ok {
		t.Error("nil params must be omitted")
	}
}

func TestNotificationOmitsID(t *testing.T) {
	m, err := NewNotification("session.update", map[string]string{"state": "running"})
	if err != nil {
		t.Fatalf("NewNotification: %v", err)
	}
	if got := m.Kind(); got != KindNotification {
		t.Fatalf("Kind() = %v, want notification", got)
	}
	b, _ := json.Marshal(m)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(b, &raw)
	if _, ok := raw["id"]; ok {
		t.Error("notification must not carry id")
	}
}

func TestNewResultPreservesNull(t *testing.T) {
	m, err := NewResult(StringID("a"), nil)
	if err != nil {
		t.Fatalf("NewResult: %v", err)
	}
	if got := m.Kind(); got != KindResponse {
		t.Fatalf("Kind() = %v, want response", got)
	}
	b, _ := json.Marshal(m)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(b, &raw)
	if string(raw["result"]) != "null" {
		t.Errorf("result = %s, want null", raw["result"])
	}
}

func TestValidateRejectsMalformed(t *testing.T) {
	id := NumberID(1)
	cases := []struct {
		name string
		msg  *Message
	}{
		{"nil", nil},
		{"bad jsonrpc", &Message{JSONRPC: "1.0", ID: &id, Method: "x"}},
		{"empty", &Message{JSONRPC: JSONRPCVersion}},
		{"request with result", &Message{JSONRPC: JSONRPCVersion, ID: &id, Method: "x", Result: json.RawMessage("1")}},
		{"response with both", &Message{JSONRPC: JSONRPCVersion, ID: &id, Result: json.RawMessage("1"), Error: Internal("x")}},
		{"response with neither", &Message{JSONRPC: JSONRPCVersion, ID: &id}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.msg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidateAcceptsWellFormed(t *testing.T) {
	req, _ := NewRequest(NumberID(1), "session.list", nil)
	note, _ := NewNotification("session.update", nil)
	res, _ := NewResult(NumberID(1), map[string]string{"ok": "true"})
	errRes := NewErrorResponse(NumberID(1), Unauthorized("no"))
	for _, m := range []*Message{req, note, res, errRes} {
		if err := m.Validate(); err != nil {
			t.Errorf("Validate(%v): %v", m.Kind(), err)
		}
	}
}

func TestParseMessageRoundTrip(t *testing.T) {
	orig, err := NewRequest(StringID("r1"), "agent.list", map[string]int{"limit": 10})
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	b, _ := json.Marshal(orig)
	got, err := ParseMessage(b)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if got.Method != "agent.list" {
		t.Errorf("method = %q", got.Method)
	}
	if got.RequestID() != StringID("r1") {
		t.Errorf("id = %+v", got.RequestID())
	}
	if string(got.Params) != string(orig.Params) {
		t.Errorf("params = %s, want %s", got.Params, orig.Params)
	}
}

func TestParseMessageRejectsGarbage(t *testing.T) {
	_, err := ParseMessage([]byte("not json"))
	if err == nil {
		t.Fatal("expected parse error")
	}
	if e := AsError(err); e.Code != CodeParseError {
		t.Fatalf("code = %d, want %d", e.Code, CodeParseError)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	v := Current()
	m, err := NewRequest(NumberID(1), "session.prompt", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	m.WithMeta(&Meta{
		ProtocolVersion: &v,
		Actor:           "slack:U123",
		Capabilities:    []string{"message_acknowledgement"},
		SessionID:       "sess_1",
	})
	b, _ := json.Marshal(m)
	got, err := ParseMessage(b)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if got.Meta == nil || got.Meta.Actor != "slack:U123" {
		t.Fatalf("meta not preserved: %+v", got.Meta)
	}
	if got.Meta.ProtocolVersion == nil || !got.Meta.ProtocolVersion.Compatible() {
		t.Fatalf("protocol version not preserved: %+v", got.Meta.ProtocolVersion)
	}
}
