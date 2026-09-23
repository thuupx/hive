package storage

import (
	"strings"
	"testing"
)

// These are internal tests: splitStatements is unexported, and its edge
// cases are exactly the ones a black-box test cannot reach.
func TestSplitStatements(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "two statements",
			in:   "CREATE TABLE a (id INTEGER); CREATE TABLE b (id INTEGER);",
			want: []string{"CREATE TABLE a (id INTEGER)", "CREATE TABLE b (id INTEGER)"},
		},
		{
			name: "no trailing semicolon",
			in:   "SELECT 1; SELECT 2",
			want: []string{"SELECT 1", "SELECT 2"},
		},
		{
			name: "semicolon inside string literal",
			in:   `INSERT INTO t VALUES ('a;b')`,
			want: []string{`INSERT INTO t VALUES ('a;b')`},
		},
		{
			name: "escaped quote inside string literal",
			in:   `INSERT INTO t VALUES ('it''s; fine')`,
			want: []string{`INSERT INTO t VALUES ('it''s; fine')`},
		},
		{
			name: "line comment is not a separator",
			in:   "-- a; comment with a ' quote\nSELECT 1;",
			want: []string{"SELECT 1"},
		},
		{
			name: "block comment is not a separator",
			in:   "/* a; comment */ SELECT 1;",
			want: []string{"SELECT 1"},
		},
		{
			name: "whitespace only",
			in:   "  \n\t ",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitStatements(tc.in)
			if err != nil {
				t.Fatalf("splitStatements: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d statements %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("statement %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestSplitStatementsRejectsUnterminated(t *testing.T) {
	cases := map[string]string{
		"string literal": `INSERT INTO t VALUES ('oops)`,
		"block comment":  "SELECT 1; /* oops",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := splitStatements(in); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestEmbeddedMigrationsSplit(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations are embedded")
	}
	for _, m := range migrations {
		statements, err := splitStatements(m.body)
		if err != nil {
			t.Fatalf("migration %04d_%s: %v", m.version, m.name, err)
		}
		if len(statements) < 20 {
			t.Errorf("migration %04d_%s produced only %d statements", m.version, m.name, len(statements))
		}
		for i, s := range statements {
			if strings.TrimSpace(s) == "" {
				t.Errorf("migration %04d_%s statement %d is empty", m.version, m.name, i)
			}
		}
	}
}

func TestParseMigrationName(t *testing.T) {
	cases := []struct {
		in      string
		version int
		name    string
		wantErr bool
	}{
		{in: "0001_init.sql", version: 1, name: "init"},
		{in: "0012_add_index.sql", version: 12, name: "add_index"},
		{in: "init.sql", wantErr: true},
		{in: "0000_init.sql", wantErr: true},
		{in: "abcd_init.sql", wantErr: true},
		{in: "0001_.sql", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			version, name, err := parseMigrationName(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMigrationName: %v", err)
			}
			if version != tc.version || name != tc.name {
				t.Fatalf("got %d/%q, want %d/%q", version, name, tc.version, tc.name)
			}
		})
	}
}
