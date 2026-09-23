package v1

import (
	"encoding/json"
	"testing"
)

func TestIDMarshal(t *testing.T) {
	cases := []struct {
		name string
		id   ID
		want string
	}{
		{"number", NumberID(7), "7"},
		{"zero number", NumberID(0), "0"},
		{"string", StringID("abc"), `"abc"`},
		{"empty string", StringID(""), `""`},
		{"unset", ID{}, "null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.id)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("marshal = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestIDUnmarshal(t *testing.T) {
	var num ID
	if err := json.Unmarshal([]byte("42"), &num); err != nil {
		t.Fatalf("unmarshal number: %v", err)
	}
	if n, ok := num.Number(); !ok || n != 42 {
		t.Fatalf("Number() = %d, %v; want 42, true", n, ok)
	}
	if num.IsString() {
		t.Fatal("numeric id reported as string")
	}

	var str ID
	if err := json.Unmarshal([]byte(`"req-1"`), &str); err != nil {
		t.Fatalf("unmarshal string: %v", err)
	}
	if str.String() != "req-1" || !str.IsString() {
		t.Fatalf("string id = %q, isString=%v", str.String(), str.IsString())
	}
	if _, ok := str.Number(); ok {
		t.Fatal("string id reported a numeric value")
	}

	var nul ID
	if err := json.Unmarshal([]byte("null"), &nul); err != nil {
		t.Fatalf("unmarshal null: %v", err)
	}
	if nul.Valid() {
		t.Fatal("null id reported as valid")
	}
}

func TestIDUnmarshalRejectsInvalid(t *testing.T) {
	for _, in := range []string{`1.5`, `-3`, `{}`, `true`} {
		var id ID
		if err := json.Unmarshal([]byte(in), &id); err == nil {
			t.Errorf("unmarshal %s: expected error", in)
		}
	}
}

func TestIDRoundTripPreservesType(t *testing.T) {
	for _, id := range []ID{NumberID(12), StringID("12")} {
		b, err := json.Marshal(id)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got ID
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got != id {
			t.Fatalf("round trip changed id: %+v -> %+v", id, got)
		}
	}
}

func TestIDComparable(t *testing.T) {
	if NumberID(1) == StringID("1") {
		t.Fatal("numeric and string ids with the same text must not compare equal")
	}
	if NumberID(1) != NumberID(1) {
		t.Fatal("identical ids must compare equal")
	}
}
