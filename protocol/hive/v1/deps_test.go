package v1

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestNoInternalDependency enforces the plugin boundary: the public
// protocol package must not import any internal Hive package, so plugins
// and external clients can depend on it without linking the core.
func TestNoInternalDependency(t *testing.T) {
	for _, path := range packageImports(t) {
		if path == "internal" || strings.HasPrefix(path, "internal/") || strings.Contains(path, "/internal/") {
			t.Errorf("protocol package imports internal package %q", path)
		}
	}
}

// TestStdlibOnly keeps the protocol surface dependency-free. A plugin must
// be able to vendor this package without pulling in third-party code.
func TestStdlibOnly(t *testing.T) {
	for _, path := range packageImports(t) {
		first, _, _ := strings.Cut(path, "/")
		if strings.Contains(first, ".") {
			t.Errorf("protocol package depends on non-stdlib package %q", path)
		}
	}
}

func packageImports(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	var out []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, imp := range file.Imports {
				out = append(out, strings.Trim(imp.Path.Value, `"`))
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no imports found; package parsing did not work")
	}
	return out
}
