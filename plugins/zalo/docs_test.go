package zalo_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thuupx/hive/plugins/zalo"
)

// The documented command table must match the transport's own catalog.
//
// A catalog is a promise to the user. Adding a command without documenting it, or
// documenting one that no longer exists, is a documentation bug a test can catch
// cheaply.
func TestDocumentedCommandsMatchTheCatalog(t *testing.T) {
	body := readDoc(t)

	for _, entry := range zalo.Catalog() {
		row := "| `" + entry.Command + "` |"
		if !strings.Contains(body, row) {
			t.Errorf("command %q is in the catalog but not in docs/zalo.md", entry.Command)
		}
	}

	for _, name := range documentedCommands(t, body) {
		if _, ok := zalo.Commands[name]; !ok {
			t.Errorf("docs/zalo.md documents %q, which the transport does not expose", name)
		}
	}
}

// The documentation tells the user which environment variables to set, and those
// names are the transport's contract with the operator.
func TestDocumentedEnvironmentVariables(t *testing.T) {
	body := readDoc(t)
	for _, name := range []string{"ZALO_BOT_TOKEN", "ZALO_TOKEN"} {
		if !strings.Contains(body, name) {
			t.Errorf("docs/zalo.md does not mention %s", name)
		}
	}
}

// The transport answers a permission in words, and the documentation says so.
func TestDocumentedPermissionReplies(t *testing.T) {
	body := readDoc(t)
	for _, want := range []string{"allow", "deny"} {
		if !strings.Contains(body, want) {
			t.Errorf("docs/zalo.md does not mention %q", want)
		}
	}
}

func readDoc(t *testing.T) string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "zalo.md"))
	if err != nil {
		t.Fatalf("read the Zalo documentation: %v", err)
	}
	return string(doc)
}

// documentedCommands extracts the names from the command table.
func documentedCommands(t *testing.T, body string) []string {
	t.Helper()

	const header = "| Command | Arguments | What it does |"
	start := strings.Index(body, header)
	if start < 0 {
		t.Fatal("docs/zalo.md has no command table")
	}

	var names []string
	for _, line := range strings.Split(body[start:], "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		if !strings.HasPrefix(trimmed, "| `") {
			continue
		}

		name := strings.TrimPrefix(trimmed, "| `")
		end := strings.Index(name, "`")
		if end < 0 {
			continue
		}
		names = append(names, name[:end])
	}

	if len(names) == 0 {
		t.Fatal("the command table lists no commands")
	}
	return names
}
