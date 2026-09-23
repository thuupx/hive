package tests

import (
	"encoding/json"
	"errors"
	"testing"

	v1 "github.com/thupham/hive/protocol/hive/v1"
)

func TestErrorImplementsError(t *testing.T) {
	var err error = v1.Unauthorized("denied")
	if err.Error() == "" {
		t.Fatal("Error() must not be empty")
	}
}

func TestAsError(t *testing.T) {
	if v1.AsError(nil) != nil {
		t.Fatal("AsError(nil) must be nil")
	}
	orig := v1.CursorExpired("gone")
	if got := v1.AsError(orig); got != orig {
		t.Fatal("AsError must return the original protocol error")
	}
	wrapped := errors.New("boom")
	got := v1.AsError(wrapped)
	if got.Code != v1.CodeInternalError {
		t.Fatalf("code = %d, want %d", got.Code, v1.CodeInternalError)
	}
}

func TestWithData(t *testing.T) {
	e := v1.CursorExpired("gone").WithData(map[string]any{
		"next_sequence": 103,
		"snapshot_id":   "snap_1",
		"snapshot_seq":  102,
	})
	var data map[string]any
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("data is not valid json: %v", err)
	}
	if data["next_sequence"].(float64) != 103 {
		t.Fatalf("data = %v", data)
	}
}

func TestErrorCodesAreDistinct(t *testing.T) {
	codes := map[string]int{
		"parse":       v1.CodeParseError,
		"invalid_req": v1.CodeInvalidRequest,
		"no_method":   v1.CodeMethodNotFound,
		"bad_params":  v1.CodeInvalidParams,
		"internal":    v1.CodeInternalError,
		"unauth":      v1.CodeUnauthorized,
		"not_found":   v1.CodeNotFound,
		"conflict":    v1.CodeConflict,
		"cursor":      v1.CodeCursorExpired,
		"ambiguous":   v1.CodeAmbiguous,
		"unavailable": v1.CodeUnavailable,
		"retryable":   v1.CodeRetryable,
		"unsupported": v1.CodeUnsupported,
	}
	seen := map[int]string{}
	for name, code := range codes {
		if other, ok := seen[code]; ok {
			t.Errorf("%s and %s share code %d", name, other, code)
		}
		seen[code] = name
	}
}

func TestErrorJSONShape(t *testing.T) {
	b, err := json.Marshal(v1.Unsupported("agent does not support images"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(raw["code"]) != "-32007" {
		t.Errorf("code = %s", raw["code"])
	}
	if _, ok := raw["data"]; ok {
		t.Error("data must be omitted when unset")
	}
}
