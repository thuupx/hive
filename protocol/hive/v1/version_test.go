package v1_test

import (
	"testing"

	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

func TestCurrentIsCompatible(t *testing.T) {
	if !v1.Current().Compatible() {
		t.Fatal("Current() must be compatible with this build")
	}
}

func TestCompatibility(t *testing.T) {
	cases := []struct {
		name string
		v    v1.ProtocolVersion
		want bool
	}{
		{"same", v1.ProtocolVersion{Family: v1.ProtocolFamily, Major: 1, Minor: 0}, true},
		{"lower minor", v1.ProtocolVersion{Family: v1.ProtocolFamily, Major: 1, Minor: -1}, true},
		{"higher minor", v1.ProtocolVersion{Family: v1.ProtocolFamily, Major: 1, Minor: 1}, false},
		{"higher major", v1.ProtocolVersion{Family: v1.ProtocolFamily, Major: 2, Minor: 0}, false},
		{"lower major", v1.ProtocolVersion{Family: v1.ProtocolFamily, Major: 0, Minor: 9}, false},
		{"other family", v1.ProtocolVersion{Family: "acp", Major: 1, Minor: 0}, false},
		{"empty", v1.ProtocolVersion{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.Compatible(); got != tc.want {
				t.Fatalf("Compatible() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVersionString(t *testing.T) {
	if got, want := v1.Current().String(), "hive/1.0"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
