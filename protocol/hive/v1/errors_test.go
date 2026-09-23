package v1

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestErrorImplementsError(t *testing.T) {
	var err error = Unauthorized("denied")
	if err.Error() == "" {
		t.Fatal("Error() must not be empty")
	}
}

func TestAsError(t *testing.T) {
	if AsError(nil) != nil {
		t.Fatal("AsError(nil) must be nil")
	}
	orig := CursorExpired("gone")
	if got := AsError(orig); got != orig {
		t.Fatal("AsError must return the original protocol error")
	}
	wrapped := errors.New("boom")
	got := AsError(wrapped)
	if got.Code != CodeInternalError {
		t.Fatalf("code = %d, want %d", got.Code, CodeInternalError)
	}
}

func TestWithData(t *testing.T) {
	e := CursorExpired("gone").WithData(map[string]any{
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
		"parse":       CodeParseError,
		"invalid_req": CodeInvalidRequest,
		"no_method":   CodeMethodNotFound,
		"bad_params":  CodeInvalidParams,
		"internal":    CodeInternalError,
		"unauth":      CodeUnauthorized,
		"not_found":   CodeNotFound,
		"conflict":    CodeConflict,
		"cursor":      CodeCursorExpired,
		"ambiguous":   CodeAmbiguous,
		"unavailable": CodeUnavailable,
		"retryable":   CodeRetryable,
		"unsupported": CodeUnsupported,
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
	b, err := json.Marshal(Unsupported("agent does not support images"))
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
