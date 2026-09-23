package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thupham/hive/internal/client"
	"github.com/thupham/hive/internal/config"
	"github.com/thupham/hive/internal/control"
	"github.com/thupham/hive/internal/ids"
	"github.com/thupham/hive/internal/tui"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// sessionCommand handles `hive session <action>`.
func sessionCommand(f flags, args []string) error {
	if len(args) == 0 {
		return errors.New("session requires an action: create, list, status, prompt, cancel, handoff, events")
	}

	action, rest := args[0], args[1:]
	switch action {
	case "create":
		return sessionCreate(f, rest)
	case "list":
		return sessionList(f, rest)
	case "status":
		return sessionStatus(f, rest)
	case "prompt":
		return sessionPrompt(f, rest)
	case "cancel":
		return sessionCancel(f, rest)
	case "handoff":
		return sessionHandoff(f, rest)
	case "events":
		return sessionEvents(f, rest)
	default:
		return fmt.Errorf("unknown session action %q", action)
	}
}

func sessionCreate(f flags, args []string) error {
	fs := flag.NewFlagSet("session create", flag.ContinueOnError)
	agentID := fs.String("agent", "", "agent to run (default: the configured default agent)")
	workspace := fs.String("workspace", "", "workspace the run works in")
	commandID := fs.String("command-id", "", "idempotency key (default: a fresh one)")
	if err := parseArgsAndFlags(fs, args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.CreateSession(ctx, v1.SessionCreateParams{
		CommandID: commandIDOr(*commandID),
		AgentID:   *agentID,
		Workspace: *workspace,
	})
	if err != nil {
		return err
	}

	fmt.Printf("session: %s\n", result.SessionID)
	fmt.Printf("run:     %s\n", result.RunID)
	fmt.Printf("agent:   %s\n", result.AgentID)
	fmt.Printf("node:    %s\n", result.NodeID)
	fmt.Printf("command: %s\n", result.CommandID)
	return nil
}

func sessionList(f flags, args []string) error {
	fs := flag.NewFlagSet("session list", flag.ContinueOnError)
	limit := fs.Int("limit", 0, "maximum sessions to show")
	if err := parseArgsAndFlags(fs, args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.ListSessions(ctx, *limit)
	if err != nil {
		return err
	}
	if len(result.Sessions) == 0 {
		fmt.Println("no sessions")
		return nil
	}

	for _, session := range result.Sessions {
		fmt.Printf("%s  %-8s  %d runs  updated %s\n",
			session.SessionID, session.State, session.Runs, session.UpdatedAt.Format(time.RFC3339))
	}
	return nil
}

func sessionStatus(f flags, args []string) error {
	fs := flag.NewFlagSet("session status", flag.ContinueOnError)
	if err := parseArgsAndFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("session status requires a session id")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	status, err := c.Status(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	tui.RenderSession(os.Stdout, status)
	return nil
}

func sessionPrompt(f flags, args []string) error {
	fs := flag.NewFlagSet("session prompt", flag.ContinueOnError)
	runID := fs.String("run", "", "target a specific AgentRun")
	commandID := fs.String("command-id", "", "idempotency key (default: a fresh one)")
	if err := parseArgsAndFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("session prompt requires a session id and text")
	}

	sessionID := fs.Arg(0)
	text := strings.Join(fs.Args()[1:], " ")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.Prompt(ctx, v1.SessionPromptParams{
		CommandID: commandIDOr(*commandID),
		SessionID: sessionID,
		RunID:     *runID,
		Text:      text,
	})
	if err != nil {
		return err
	}

	target := "the current run"
	if result.CreatedRun {
		target = "a new run"
	}
	fmt.Printf("prompted %s (%s)\n", result.RunID, target)
	fmt.Printf("command: %s\n", result.CommandID)
	return nil
}

func sessionCancel(f flags, args []string) error {
	fs := flag.NewFlagSet("session cancel", flag.ContinueOnError)
	runID := fs.String("run", "", "cancel a specific AgentRun")
	if err := parseArgsAndFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("session cancel requires a session id")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.Cancel(ctx, v1.SessionCancelParams{
		CommandID: ids.New("cmd"),
		SessionID: fs.Arg(0),
		RunID:     *runID,
	}); err != nil {
		return err
	}

	fmt.Println("cancelled")
	return nil
}

func sessionHandoff(f flags, args []string) error {
	fs := flag.NewFlagSet("session handoff", flag.ContinueOnError)
	summary := fs.String("summary", "", "optional source-agent summary")
	workspace := fs.String("workspace", "", "workspace the target works in")
	commandID := fs.String("command-id", "", "idempotency key (default: a fresh one)")
	if err := parseArgsAndFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("session handoff requires a session id and a target agent")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.Handoff(ctx, v1.SessionHandoffParams{
		CommandID: commandIDOr(*commandID),
		SessionID: fs.Arg(0),
		AgentID:   fs.Arg(1),
		Summary:   *summary,
		Workspace: *workspace,
	})
	if err != nil {
		return err
	}

	fmt.Printf("handoff:    %s\n", result.HandoffID)
	fmt.Printf("state:      %s\n", result.State)
	fmt.Printf("source run: %s\n", result.SourceRunID)
	fmt.Printf("target run: %s\n", result.TargetRunID)
	fmt.Printf("agent:      %s\n", result.AgentID)
	fmt.Printf("node:       %s\n", result.NodeID)
	return nil
}

func sessionEvents(f flags, args []string) error {
	fs := flag.NewFlagSet("session events", flag.ContinueOnError)
	from := fs.Int64("from", 0, "resume after this sequence")
	limit := fs.Int("limit", 50, "maximum events to show")
	asJSON := fs.Bool("json", false, "print raw events")
	if err := parseArgsAndFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("session events requires a session id")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.Replay(ctx, v1.EventReplayParams{
		SessionID:    fs.Arg(0),
		FromSequence: *from,
		Limit:        *limit,
	})
	if err != nil {
		return err
	}

	if result.Gap != nil {
		// A pruned cursor is reported explicitly rather than silently skipped.
		fmt.Fprintf(os.Stderr, "cursor expired: requested %d, next valid %d\n",
			result.Gap.Requested, result.Gap.NextSequence)
		if result.Gap.SnapshotID != "" {
			fmt.Fprintf(os.Stderr, "rehydrate from snapshot %s at sequence %d\n",
				result.Gap.SnapshotID, result.Gap.SnapshotSequence)
		}
		return errors.New("rehydrate before continuing the stream")
	}

	for _, ev := range result.Events {
		if *asJSON {
			encoded, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			fmt.Println(string(encoded))
			continue
		}
		fmt.Printf("%6d  %-22s %s\n", ev.Sequence, ev.Type, eventSummary(ev))
	}
	return nil
}

func agentCommand(f flags, args []string) error {
	if len(args) == 0 || args[0] != "list" {
		return errors.New("agent requires the action: list")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.ListAgents(ctx)
	if err != nil {
		return err
	}
	if len(result.Agents) == 0 {
		fmt.Println("no agents configured")
		return nil
	}

	for _, agent := range result.Agents {
		fmt.Printf("%-20s %s\n", agent.ID, agent.Protocol)
	}
	return nil
}

func nodeCommand(f flags, args []string) error {
	if len(args) == 0 || args[0] != "list" {
		return errors.New("node requires the action: list")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.ListNodes(ctx)
	if err != nil {
		return err
	}
	if len(result.Nodes) == 0 {
		fmt.Println("no nodes")
		return nil
	}

	for _, node := range result.Nodes {
		state := "offline"
		if node.Connected {
			state = "connected"
		}
		fmt.Printf("%-24s %-10s generation %d  %d executions  %s\n",
			node.NodeID, state, node.ConnectionGeneration, node.Executions, node.Version)
	}
	return nil
}

func commandCommand(f flags, args []string) error {
	if len(args) == 0 || args[0] != "get" {
		return errors.New("command requires the action: get")
	}

	fs := flag.NewFlagSet("command get", flag.ContinueOnError)
	if err := parseArgsAndFlags(fs, args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("command get requires a command id")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.GetCommand(ctx, fs.Arg(0))
	if err != nil {
		return err
	}

	fmt.Printf("command: %s\n", result.CommandID)
	fmt.Printf("method:  %s\n", result.Method)
	fmt.Printf("state:   %s\n", result.State)
	if result.Target != "" {
		fmt.Printf("target:  %s\n", result.Target)
	}
	if len(result.Result) > 0 {
		fmt.Printf("result:  %s\n", result.Result)
	}
	if len(result.Error) > 0 {
		fmt.Printf("error:   %s\n", result.Error)
	}
	return nil
}

func tuiCommand(f flags, args []string) error {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	interval := fs.Duration("interval", 0, "refresh interval (default: render once)")
	if err := parseArgsAndFlags(fs, args); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	return tui.Run(ctx, c, os.Stdout, *interval)
}

// connect opens the Control API socket described by the configuration.
func connect(ctx context.Context, f flags) (*client.Client, error) {
	cfg, err := loadConfig(f)
	if err != nil {
		return nil, err
	}

	dataDir, err := cfg.EffectiveDataDir()
	if err != nil {
		return nil, err
	}

	socketPath := filepath.Join(dataDir, control.SocketFile)
	c, err := client.Dial(ctx, socketPath)
	if err != nil {
		return nil, fmt.Errorf("%w\nis `hive serve` running?", err)
	}
	return c, nil
}

func loadConfig(f flags) (config.Config, error) {
	cfgPath := f.configPath
	if cfgPath == "" {
		var err error
		if cfgPath, err = config.DefaultPath(); err != nil {
			return config.Config{}, err
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return config.Config{}, err
	}
	applyOverrides(&cfg, f)
	return cfg, nil
}

// reorderFlags moves flag arguments before positional ones.
//
// The standard flag package stops parsing at the first positional argument, so
// `workspace create piceta -node x` would silently drop `-node`. A caller should
// not have to remember that.
func reorderFlags(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}

		flags = append(flags, arg)
		if strings.Contains(arg, "=") {
			continue
		}

		defined := fs.Lookup(strings.TrimLeft(arg, "-"))
		if defined == nil {
			continue
		}
		if _, isBool := defined.Value.(interface{ IsBoolFlag() bool }); isBool {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}

	return append(flags, positional...)
}

// parseArgsAndFlags parses a subcommand's arguments regardless of flag order.
func parseArgsAndFlags(fs *flag.FlagSet, args []string) error {
	return fs.Parse(reorderFlags(fs, args))
}

// commandIDOr returns an explicit command id or a fresh one.
//
// A fresh id means "this is a new logical operation". Passing an explicit id is
// what a caller does when retrying the same one.
func commandIDOr(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return ids.New("cmd")
}

// eventSummary renders an event for a human.
//
// Readable text wins over an identifier: a message event carries the agent's
// answer, and showing the run id instead would hide the thing the user asked for.
func eventSummary(ev v1.Event) string {
	if len(ev.Payload) > 0 {
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err == nil && payload.Text != "" {
			return payload.Text
		}
	}
	if ev.RunID != "" {
		return ev.RunID
	}
	return string(ev.Payload)
}

// workspaceCommand handles `hive workspace <action>`.
func workspaceCommand(f flags, args []string) error {
	if len(args) == 0 {
		return errors.New("workspace requires an action: create, list")
	}

	action, rest := args[0], args[1:]
	switch action {
	case "create":
		return workspaceCreate(f, rest)
	case "list":
		return workspaceList(f, rest)
	default:
		return fmt.Errorf("unknown workspace action %q", action)
	}
}

func workspaceCreate(f flags, args []string) error {
	fs := flag.NewFlagSet("workspace create", flag.ContinueOnError)
	nodeID := fs.String("node", "", "node the location is on")
	path := fs.String("path", "", "path the workspace lives at on that node")
	commandID := fs.String("command-id", "", "idempotency key (default: a fresh one)")
	if err := parseArgsAndFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("workspace create requires a name")
	}

	locations := map[string]string{}
	if *nodeID != "" && *path != "" {
		locations[*nodeID] = *path
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	summary, err := c.CreateWorkspace(ctx, v1.WorkspaceCreateParams{
		CommandID: commandIDOr(*commandID),
		Name:      fs.Arg(0),
		Locations: locations,
	})
	if err != nil {
		return err
	}

	fmt.Printf("workspace: %s\n", summary.WorkspaceID)
	fmt.Printf("name:      %s\n", summary.Name)
	for node, path := range summary.Locations {
		fmt.Printf("location:  %s -> %s\n", node, path)
	}
	return nil
}

func workspaceList(f flags, args []string) error {
	fs := flag.NewFlagSet("workspace list", flag.ContinueOnError)
	if err := parseArgsAndFlags(fs, args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.ListWorkspaces(ctx)
	if err != nil {
		return err
	}
	if len(result.Workspaces) == 0 {
		fmt.Println("no workspaces")
	} else {
		for _, workspace := range result.Workspaces {
			fmt.Printf("%-20s %s  %d active runs\n", workspace.Name, workspace.WorkspaceID, workspace.ActiveRuns)
			for node, path := range workspace.Locations {
				fmt.Printf("    %s -> %s\n", node, path)
			}
		}
	}

	// A shared location is a warning, never a lock: concurrent runs on one
	// location are a real workflow.
	for _, warning := range result.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %d active runs share %s on %s: %v\n",
			len(warning.RunIDs), warning.Path, warning.NodeID, warning.RunIDs)
	}
	return nil
}
