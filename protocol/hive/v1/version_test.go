package v1

import "testing"

func TestCurrentIsCompatible(t *testing.T) {
	if !Current().Compatible() {
		t.Fatal("Current() must be compatible with this build")
	}
}

func TestCompatibility(t *testing.T) {
	cases := []struct {
		name string
		v    ProtocolVersion
		want bool
	}{
		{"same", ProtocolVersion{ProtocolFamily, 1, 0}, true},
		{"lower minor", ProtocolVersion{ProtocolFamily, 1, -1}, true},
		{"higher minor", ProtocolVersion{ProtocolFamily, 1, 1}, false},
		{"higher major", ProtocolVersion{ProtocolFamily, 2, 0}, false},
		{"lower major", ProtocolVersion{ProtocolFamily, 0, 9}, false},
		{"other family", ProtocolVersion{"acp", 1, 0}, false},
		{"empty", ProtocolVersion{}, false},
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
	if got, want := Current().String(), "hive/1.0"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
