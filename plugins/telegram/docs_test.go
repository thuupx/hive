// Why it exists: docs/telegram.md is the user-facing contract for this
// transport; these tests keep the doc honest about the command catalog.
package telegram

import (
	"os"
	"strings"
	"testing"
)

// The command table in docs/telegram.md must list every command the parser
// exposes and no other, so the document a user reads is the catalog the
// transport ships.
func TestDocsTelegramMatchesCommandCatalog(t *testing.T) {
	doc, err := os.ReadFile("../../docs/telegram.md")
	if err != nil {
		t.Fatalf("read docs/telegram.md: %v", err)
	}
	body := string(doc)

	for name := range Commands {
		if !strings.Contains(body, "`"+name+"`") {
			t.Errorf("docs/telegram.md does not document command %q", name)
		}
	}

	for _, name := range documentedCommands(t, body) {
		if _, ok := Commands[name]; !ok {
			t.Errorf("docs/telegram.md documents command %q the parser does not expose", name)
		}
	}
}

// The env var the operator exports is a contract: rename it and every
// installation breaks.
func TestDocsTelegramDocumentsTheToken(t *testing.T) {
	doc, err := os.ReadFile("../../docs/telegram.md")
	if err != nil {
		t.Fatalf("read docs/telegram.md: %v", err)
	}
	for _, contract := range []string{"TELEGRAM_BOT_TOKEN", "TELEGRAM_TOKEN", "transport.telegram", "allowed_users"} {
		if !strings.Contains(string(doc), contract) {
			t.Errorf("docs/telegram.md does not mention %s", contract)
		}
	}
}

// documentedCommands extracts the names from the command table.
func documentedCommands(t *testing.T, body string) []string {
	t.Helper()

	const header = "| Command | Arguments | What it does |"
	start := strings.Index(body, header)
	if start < 0 {
		t.Fatal("docs/telegram.md has no command table")
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
