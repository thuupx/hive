package acp

import (
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// TestNoCoreDependency enforces the plugin boundary: an agent adapter is a
// plugin, and a plugin does not link the Hive core.
func TestNoCoreDependency(t *testing.T) {
	skipTests := func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", skipTests, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	found := 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, imp := range file.Imports {
				found++
				path := strings.Trim(imp.Path.Value, `"`)
				if path == "internal" || strings.HasPrefix(path, "internal/") ||
					strings.Contains(path, "/internal/") {
					t.Errorf("acp adapter imports internal package %q", path)
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("no imports found; package parsing did not work")
	}
}
