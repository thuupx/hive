// Command client is a minimal Hive client.
//
// It drives the Control API the way the CLI does. An out-of-tree client would
// speak the protocol directly instead of importing this module's client package;
// the wire format is documented in docs/protocol.md.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/thuupx/hive/internal/client"
	"github.com/thuupx/hive/internal/config"
	"github.com/thuupx/hive/internal/control"
	"github.com/thuupx/hive/internal/ids"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg, err := config.Load(mustDefaultPath())
	if err != nil {
		return err
	}
	dataDir, err := cfg.EffectiveDataDir()
	if err != nil {
		return err
	}

	c, err := client.Dial(ctx, filepath.Join(dataDir, control.SocketFile))
	if err != nil {
		return fmt.Errorf("%w\nis `hive serve` running?", err)
	}
	defer c.Close()

	agents, err := c.ListAgents(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("agents: %d\n", len(agents.Agents))

	// Creating a session is one operation with one idempotency key. Calling this
	// twice with the same key returns the same session rather than creating a
	// second one.
	created, err := c.CreateSession(ctx, v1.SessionCreateParams{
		CommandID: ids.New("cmd"),
	})
	if err != nil {
		return err
	}
	fmt.Printf("session %s with run %s on agent %s\n",
		created.SessionID, created.RunID, created.AgentID)

	// A command can outlive the request, so its status is a durable resource.
	command, err := c.GetCommand(ctx, created.CommandID)
	if err != nil {
		return err
	}
	fmt.Printf("command %s is %s\n", command.CommandID, command.State)

	// Prompt the session. Routing is deterministic: with no explicit run it
	// targets the session's default interactive run.
	prompted, err := c.Prompt(ctx, v1.SessionPromptParams{
		CommandID: ids.New("cmd"),
		SessionID: created.SessionID,
		Text:      "hello from the example client",
	})
	switch {
	case err != nil:
		// The run exists but its execution never started, which is what happens
		// when the agent is not installed. The session survives and is still
		// inspectable.
		fmt.Printf("the prompt could not be delivered: %v\n", err)
	default:
		fmt.Printf("prompted run %s (created=%v)\n", prompted.RunID, prompted.CreatedRun)
	}

	// Replay the session stream. A pruned cursor is reported explicitly rather
	// than skipped.
	events, err := c.Replay(ctx, v1.EventReplayParams{SessionID: created.SessionID, FromSequence: 0})
	if err != nil {
		return err
	}
	if events.Gap != nil {
		fmt.Fprintf(os.Stderr, "cursor expired: next valid sequence is %d\n", events.Gap.NextSequence)
		return nil
	}
	for _, ev := range events.Events {
		payload, _ := json.Marshal(ev.Payload)
		fmt.Printf("  %d  %-20s %s\n", ev.Sequence, ev.Type, payload)
	}
	return nil
}

func mustDefaultPath() string {
	path, err := config.DefaultPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		os.Exit(1)
	}
	return path
}
