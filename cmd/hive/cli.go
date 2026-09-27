package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thuupx/hive/internal/client"
	"github.com/thuupx/hive/internal/config"
	"github.com/thuupx/hive/internal/control"
	"github.com/thuupx/hive/internal/ids"
	"github.com/thuupx/hive/internal/tui"
	v1 "github.com/thuupx/hive/protocol/hive/v1"
)

// The options each command takes. They are plain values, so a command body does
// not parse flags and the tree owns the flag definitions.

type sessionCreateOptions struct {
	agent     string
	workspace string
	commandID string
}

type sessionListOptions struct {
	limit int
}

type sessionPromptOptions struct {
	sessionID string
	text      string
	runID     string
	commandID string
	noWait    bool
}

type sessionCancelOptions struct {
	sessionID string
	runID     string
}

type sessionConfigOptions struct {
	sessionID string
	configID  string
	value     string
	runID     string
}

type sessionHandoffOptions struct {
	sessionID string
	agentID   string
	summary   string
	workspace string
	commandID string
}

type sessionEventsOptions struct {
	sessionID string
	from      int64
	limit     int
	asJSON    bool
	all       bool
	types     string
}

type tuiOptions struct {
	interval time.Duration
}

type workspaceCreateOptions struct {
	name      string
	nodeID    string
	path      string
	commandID string
}

type permissionListOptions struct {
	sessionID string
}

type permissionRespondOptions struct {
	requestID string
	allow     bool
	deny      bool
	sessionID string
}

func sessionCreate(f flags, opts sessionCreateOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.CreateSession(ctx, v1.SessionCreateParams{
		CommandID: commandIDOr(opts.commandID),
		AgentID:   opts.agent,
		Workspace: opts.workspace,
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

func sessionList(f flags, opts sessionListOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.ListSessions(ctx, opts.limit)
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

func sessionStatus(f flags, sessionID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	status, err := c.Status(ctx, sessionID)
	if err != nil {
		return err
	}
	tui.RenderSession(os.Stdout, status)
	return nil
}

func sessionPrompt(f flags, opts sessionPromptOptions) error {
	// The call itself is quick. The turn is not, so it is followed separately.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.Prompt(ctx, v1.SessionPromptParams{
		CommandID: commandIDOr(opts.commandID),
		SessionID: opts.sessionID,
		RunID:     opts.runID,
		Text:      opts.text,
	})
	if err != nil {
		return err
	}

	target := "the current run"
	if result.CreatedRun {
		target = "a new run"
	}
	fmt.Printf("prompted %s (%s)\n", result.RunID, target)

	if opts.noWait {
		fmt.Printf("\nThe turn runs in the background. Follow it with:\n")
		fmt.Printf("  hive command get %s\n", result.CommandID)
		fmt.Printf("  hive session events %s\n", opts.sessionID)
		return nil
	}

	// A command's lifetime is not a request's lifetime, so the client follows the
	// durable record rather than holding the prompt open.
	waitCtx, stopWaiting := context.WithTimeout(context.Background(), turnWaitTimeout)
	defer stopWaiting()

	if err := followCommand(waitCtx, c, result.CommandID); err != nil {
		return err
	}

	return showRunAnswer(waitCtx, c, opts.sessionID, result.RunID)
}

// turnWaitTimeout bounds how long the CLI follows a turn.
const turnWaitTimeout = 30 * time.Minute

// followCommand reports a command until it reaches a terminal state.
func followCommand(ctx context.Context, c *client.Client, commandID string) error {
	last := ""

	for {
		cmd, err := c.GetCommand(ctx, commandID)
		if err != nil {
			return err
		}

		if cmd.State != last {
			fmt.Printf("  %s\n", cmd.State)
			last = cmd.State
		}

		switch cmd.State {
		case "completed":
			return nil
		case "failed", "rejected":
			if len(cmd.Error) > 0 {
				return fmt.Errorf("the operation %s: %s", cmd.State, cmd.Error)
			}
			return fmt.Errorf("the operation %s", cmd.State)
		}

		select {
		case <-ctx.Done():
			fmt.Printf("\nstill running; follow it with `hive command get %s`\n", commandID)
			return nil
		case <-time.After(time.Second):
		}
	}
}

// showRunAnswer prints the readable events a run produced.
func showRunAnswer(ctx context.Context, c *client.Client, sessionID, runID string) error {
	replay, err := c.Replay(ctx, v1.EventReplayParams{SessionID: sessionID, Limit: 500})
	if err != nil {
		return err
	}

	printed := 0
	for _, ev := range replay.Events {
		if ev.RunID != runID || ev.Type == v1.EventAgentRaw {
			continue
		}
		if printed == 0 {
			fmt.Println()
		}
		fmt.Println(eventSummary(ev))
		printed++
	}
	return nil
}

func sessionCancel(f flags, opts sessionCancelOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.Cancel(ctx, v1.SessionCancelParams{
		CommandID: ids.New("cmd"),
		SessionID: opts.sessionID,
		RunID:     opts.runID,
	}); err != nil {
		return err
	}

	fmt.Println("cancelled")
	return nil
}

// sessionConfig reads or changes the agent's session selectors.
//
// The selectors are the agent's own: Hive renders whatever the agent declared,
// which is why this prints them rather than knowing what a model is.
func sessionConfig(f flags, opts sessionConfigOptions) error {
	params := v1.SessionConfigParams{
		SessionID: opts.sessionID,
		RunID:     opts.runID,
		ConfigID:  opts.configID,
		Value:     opts.value,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.SessionConfig(ctx, params)
	if err != nil {
		return err
	}

	if len(result.Options) == 0 {
		agent := result.AgentID
		if agent == "" {
			agent = "this agent"
		}
		fmt.Printf("%s declares no session settings over ACP\n", agent)
		fmt.Println("Hive renders the settings an agent declares for a session. An agent that")
		fmt.Println("exposes its model picker only through the legacy ACP models/modes fields")
		fmt.Println("declares nothing here: Hive reads configOptions, which supersedes them.")
		return nil
	}

	for _, option := range result.Options {
		current := strings.Trim(string(option.CurrentValue), `"`)
		fmt.Printf("%s (%s)", option.Name, option.ID)
		if current != "" {
			fmt.Printf(" = %s", current)
		}
		fmt.Println()
		for _, value := range option.Options {
			mark := " "
			if value.Value == current {
				mark = "*"
			}
			fmt.Printf("  %s %-20s %s\n", mark, value.Value, value.Name)
		}
	}
	return nil
}

func sessionHandoff(f flags, opts sessionHandoffOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.Handoff(ctx, v1.SessionHandoffParams{
		CommandID: commandIDOr(opts.commandID),
		SessionID: opts.sessionID,
		AgentID:   opts.agentID,
		Summary:   opts.summary,
		Workspace: opts.workspace,
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

func sessionEvents(f flags, opts sessionEventsOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.Replay(ctx, v1.EventReplayParams{
		SessionID:    opts.sessionID,
		FromSequence: opts.from,
		Limit:        opts.limit,
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

	shown := 0
	for _, ev := range result.Events {
		if !visibleEvent(ev, opts.all, opts.types) {
			continue
		}
		shown++

		if opts.asJSON {
			encoded, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			fmt.Println(string(encoded))
			continue
		}
		fmt.Printf("%6d  %-22s %s\n", ev.Sequence, ev.Type, eventSummary(ev))
	}

	if shown == 0 && !opts.asJSON {
		// Say why the stream looked empty rather than leaving it ambiguous.
		if opts.all || opts.types != "" {
			fmt.Println("no events matched")
		} else {
			fmt.Printf("no readable events; %d are protocol traffic (use -all)\n", len(result.Events))
		}
	}
	return nil
}

// visibleEvent decides whether an event is worth showing.
//
// Agent protocol traffic dominates a stream by volume: one turn can produce
// hundreds of chunks and exactly one answer. Showing all of it by default would
// bury the thing the user asked for.
func visibleEvent(ev v1.Event, all bool, only string) bool {
	if only != "" {
		return ev.Type == only
	}
	if all {
		return true
	}
	return ev.Type != v1.EventAgentRaw
}

func agentList(f flags) error {
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

func nodeList(f flags) error {
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

func commandGet(f flags, commandID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.GetCommand(ctx, commandID)
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

func tuiCommand(f flags, opts tuiOptions) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	return tui.Run(ctx, c, os.Stdout, opts.interval)
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

func workspaceCreate(f flags, opts workspaceCreateOptions) error {
	locations := map[string]string{}
	if opts.nodeID != "" && opts.path != "" {
		locations[opts.nodeID] = opts.path
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	summary, err := c.CreateWorkspace(ctx, v1.WorkspaceCreateParams{
		CommandID: commandIDOr(opts.commandID),
		Name:      opts.name,
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

func workspaceList(f flags) error {
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

func permissionList(f flags, opts permissionListOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	result, err := c.ListPermissions(ctx, v1.PermissionListParams{SessionID: opts.sessionID})
	if err != nil {
		return err
	}
	if len(result.Permissions) == 0 {
		fmt.Println("no pending permissions")
		return nil
	}

	for _, pending := range result.Permissions {
		fmt.Printf("%s  session %s  run %s\n", pending.PermissionID, pending.SessionID, pending.RunID)
		requestID := control.UnquoteRequestID(pending.AgentRequestID)
		fmt.Printf("    agent request: %s\n", requestID)
		fmt.Printf("    waiting since: %s\n", pending.CreatedAt.Format(time.RFC3339))
		fmt.Printf("    answer with:   hive permission respond %s -allow   (or -deny)\n", requestID)
	}
	return nil
}

func permissionRespond(f flags, opts permissionRespondOptions) error {
	if opts.allow == opts.deny {
		return errors.New("pass exactly one of -allow or -deny")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, err := connect(ctx, f)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.RespondToPermission(ctx, v1.PermissionRespondParams{
		AgentRequestID: opts.requestID,
		SessionID:      opts.sessionID,
		Approved:       opts.allow,
	}); err != nil {
		return err
	}

	if opts.allow {
		fmt.Println("approved")
	} else {
		fmt.Println("denied")
	}
	return nil
}
