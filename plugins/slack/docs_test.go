package slack_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thupham/hive/plugins/slack"
)

// The documented command table must match the transport's own catalog.
//
// A catalog is a promise to the user. Adding a command without documenting it, or
// documenting one that no longer exists, is a documentation bug that a test can
// catch cheaply.
func TestDocumentedCommandsMatchTheCatalog(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "slack.md"))
	if err != nil {
		t.Fatalf("read the Slack documentation: %v", err)
	}
	body := string(doc)

	for _, entry := range slack.Catalog() {
		// The table row is `| `name` | args | description |`.
		row := "| `" + entry.Command + "` |"
		if !strings.Contains(body, row) {
			t.Errorf("command %q is in the catalog but not in docs/slack.md", entry.Command)
		}
	}

	// The other direction: a command in the table that the transport does not
	// expose would mislead a user. Only the command table counts; the scope and
	// event tables are not commands.
	for _, name := range documentedCommands(t, body) {
		if _, ok := slack.Commands[name]; !ok {
			t.Errorf("docs/slack.md documents %q, which the transport does not expose", name)
		}
	}
}

// documentedCommands extracts the names from the command table.
func documentedCommands(t *testing.T, body string) []string {
	t.Helper()

	const header = "| Command | Arguments | What it does |"
	start := strings.Index(body, header)
	if start < 0 {
		t.Fatal("docs/slack.md has no command table")
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

// The permission buttons are the only way to answer a request from Slack, so
// their action ids are part of the documented contract.
func TestDocumentedPermissionActionsExist(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "slack.md"))
	if err != nil {
		t.Fatalf("read the Slack documentation: %v", err)
	}

	if !strings.Contains(string(doc), "[ Allow ]") || !strings.Contains(string(doc), "[ Deny ]") {
		t.Error("docs/slack.md should show the permission buttons")
	}

	// The action ids themselves are the transport's, and both must be handled.
	for _, action := range []string{slack.ActionPermissionAllow, slack.ActionPermissionDeny} {
		if action == "" {
			t.Errorf("permission action %q is empty", action)
		}
	}
}

// The documentation tells the user which environment variables to set, and those
// names are the transport's contract with the operator.
func TestDocumentedEnvironmentVariables(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "slack.md"))
	if err != nil {
		t.Fatalf("read the Slack documentation: %v", err)
	}
	body := string(doc)

	for _, name := range []string{"SLACK_APP_TOKEN", "SLACK_BOT_TOKEN"} {
		if !strings.Contains(body, name) {
			t.Errorf("docs/slack.md does not mention %s", name)
		}
	}
}

// The help text is generated from the catalog, so it cannot drift from it.
func TestHelpListsEveryCommand(t *testing.T) {
	message := slack.HelpMessage()

	for _, entry := range slack.Catalog() {
		if !strings.Contains(message.Text, entry.Command) {
			t.Errorf("help does not list %q", entry.Command)
		}
	}
	if !strings.Contains(message.Text, "@Hive") {
		t.Error("help should say how to address the bot")
	}
}
