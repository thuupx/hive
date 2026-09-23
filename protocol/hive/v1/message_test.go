package v1_test

import (
	"encoding/json"
	"testing"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

func TestNewRequestShape(t *testing.T) {
	m, err := v1.NewRequest(v1.NumberID(1), "session.create", map[string]string{"agent": "devin"})
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if got := m.Kind(); got != v1.KindRequest {
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
	m, err := v1.NewRequest(v1.NumberID(1), "session.list", nil)
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
	m, err := v1.NewNotification("session.update", map[string]string{"state": "running"})
	if err != nil {
		t.Fatalf("NewNotification: %v", err)
	}
	if got := m.Kind(); got != v1.KindNotification {
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
	m, err := v1.NewResult(v1.StringID("a"), nil)
	if err != nil {
		t.Fatalf("NewResult: %v", err)
	}
	if got := m.Kind(); got != v1.KindResponse {
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
	id := v1.NumberID(1)
	cases := []struct {
		name string
		msg  *v1.Message
	}{
		{"nil", nil},
		{"bad jsonrpc", &v1.Message{JSONRPC: "1.0", ID: &id, Method: "x"}},
		{"empty", &v1.Message{JSONRPC: v1.JSONRPCVersion}},
		{"request with result", &v1.Message{JSONRPC: v1.JSONRPCVersion, ID: &id, Method: "x", Result: json.RawMessage("1")}},
		{"response with both", &v1.Message{JSONRPC: v1.JSONRPCVersion, ID: &id, Result: json.RawMessage("1"), Error: v1.Internal("x")}},
		{"response with neither", &v1.Message{JSONRPC: v1.JSONRPCVersion, ID: &id}},
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
	req, _ := v1.NewRequest(v1.NumberID(1), "session.list", nil)
	note, _ := v1.NewNotification("session.update", nil)
	res, _ := v1.NewResult(v1.NumberID(1), map[string]string{"ok": "true"})
	errRes := v1.NewErrorResponse(v1.NumberID(1), v1.Unauthorized("no"))
	for _, m := range []*v1.Message{req, note, res, errRes} {
		if err := m.Validate(); err != nil {
			t.Errorf("Validate(%v): %v", m.Kind(), err)
		}
	}
}

func TestParseMessageRoundTrip(t *testing.T) {
	orig, err := v1.NewRequest(v1.StringID("r1"), "agent.list", map[string]int{"limit": 10})
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	b, _ := json.Marshal(orig)
	got, err := v1.ParseMessage(b)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if got.Method != "agent.list" {
		t.Errorf("method = %q", got.Method)
	}
	if got.RequestID() != v1.StringID("r1") {
		t.Errorf("id = %+v", got.RequestID())
	}
	if string(got.Params) != string(orig.Params) {
		t.Errorf("params = %s, want %s", got.Params, orig.Params)
	}
}

func TestParseMessageRejectsGarbage(t *testing.T) {
	_, err := v1.ParseMessage([]byte("not json"))
	if err == nil {
		t.Fatal("expected parse error")
	}
	if e := v1.AsError(err); e.Code != v1.CodeParseError {
		t.Fatalf("code = %d, want %d", e.Code, v1.CodeParseError)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	v := v1.Current()
	m, err := v1.NewRequest(v1.NumberID(1), "session.prompt", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	m.WithMeta(&v1.Meta{
		ProtocolVersion: &v,
		Actor:           "slack:U123",
		Capabilities:    []string{"message_acknowledgement"},
		SessionID:       "sess_1",
	})
	b, _ := json.Marshal(m)
	got, err := v1.ParseMessage(b)
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
