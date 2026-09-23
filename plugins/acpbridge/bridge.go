// Package acpbridge exposes an ACP agent as a Hive agent plugin.
//
// It composes the ACP adapter with the plugin SDK: the core speaks the Hive
// Plugin API to this process, and this process speaks ACP to the agent. Neither
// side needs to know the other's protocol.
package acpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/thupham/hive/plugins/acp"
	"github.com/thupham/hive/plugins/sdk"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// Launcher starts an ACP agent and returns a connected, initialized client.
type Launcher interface {
	Launch(ctx context.Context) (*acp.Client, *acp.Capabilities, error)
}

// CommandLauncher starts an ACP agent as a subprocess.
type CommandLauncher struct {
	// Command is the agent invocation, for example ["devin", "acp"].
	Command []string

	// ClientVersion is reported to the agent.
	ClientVersion string
}

// Launch starts the agent process and completes the ACP handshake.
func (l CommandLauncher) Launch(ctx context.Context) (*acp.Client, *acp.Capabilities, error) {
	if len(l.Command) == 0 {
		return nil, nil, errors.New("acpbridge: agent command is required")
	}

	cmd := exec.Command(l.Command[0], l.Command[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("acpbridge: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("acpbridge: stdout: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("acpbridge: start agent: %w", err)
	}

	transport := &processTransport{stdout: stdout, stdin: stdin, cmd: cmd}
	client := acp.NewClient(transport, acp.Options{ClientVersion: l.ClientVersion})

	caps, err := client.Initialize(ctx)
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	return client, caps, nil
}

type processTransport struct {
	stdout io.ReadCloser
	stdin  io.WriteCloser
	cmd    *exec.Cmd
	once   sync.Once
}

func (t *processTransport) Read(p []byte) (int, error)  { return t.stdout.Read(p) }
func (t *processTransport) Write(p []byte) (int, error) { return t.stdin.Write(p) }

func (t *processTransport) Close() error {
	t.once.Do(func() {
		_ = t.stdout.Close()
		_ = t.stdin.Close()
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		_ = t.cmd.Wait()
	})
	return nil
}

// Bridge maps Hive Plugin API execution calls onto one ACP agent.
type Bridge struct {
	host     *sdk.Host
	launcher Launcher

	mu     sync.Mutex
	client *acp.Client
	caps   *acp.Capabilities
	runs   map[string]*run

	// pending permission requests, keyed by the agent request id
	perms map[string]pendingPermission
}

type run struct {
	agentRunID string
	sessionID  string
}

// pendingPermission keeps what is needed to answer an ACP permission request in
// the agent's own vocabulary.
type pendingPermission struct {
	requestID json.RawMessage
	options   []acp.PermissionOption
}

// New returns a bridge over a connected host.
func New(host *sdk.Host, launcher Launcher) *Bridge {
	return &Bridge{
		host:     host,
		launcher: launcher,
		runs:     make(map[string]*run),
		perms:    make(map[string]pendingPermission),
	}
}

// Register installs the plugin handlers.
func (b *Bridge) Register() {
	b.host.Handle(v1.MethodExecutionStart, b.start)
	b.host.Handle(v1.MethodExecutionPrompt, b.prompt)
	b.host.Handle(v1.MethodExecutionCancel, b.cancel)
	b.host.Handle(v1.MethodPermissionRespond, b.respond)
}

// Run serves until ctx is cancelled or the connection ends.
func (b *Bridge) Run(ctx context.Context) error {
	return b.host.Run(ctx)
}

func (b *Bridge) start(ctx context.Context, params json.RawMessage) (any, error) {
	var req v1.ExecutionStartParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, v1.InvalidParams("invalid execution.start request")
	}
	if req.AgentRunID == "" {
		return nil, v1.InvalidParams("agentRunId is required")
	}

	client, err := b.ensureAgent(ctx)
	if err != nil {
		// The start side effect never happened. Reporting absent lets the core
		// reconcile without starting a second execution.
		_ = b.report(ctx, req.AgentRunID, req.Generation, "absent", "", err.Error())
		return nil, err
	}

	sessionID, err := client.NewSession(ctx, acp.NewSessionRequest{Cwd: req.WorkspacePath})
	if err != nil {
		// The start side effect did not happen. Reporting absent lets the core
		// reconcile without starting a second execution.
		_ = b.report(ctx, req.AgentRunID, req.Generation, "absent", "", err.Error())
		return nil, v1.Unavailable("agent session could not be created: %s", err.Error())
	}

	b.mu.Lock()
	b.runs[req.AgentRunID] = &run{agentRunID: req.AgentRunID, sessionID: sessionID}
	b.mu.Unlock()

	_ = b.report(ctx, req.AgentRunID, req.Generation, "starting", sessionID, "")

	return map[string]any{"runtimeSessionId": sessionID}, nil
}

func (b *Bridge) prompt(ctx context.Context, params json.RawMessage) (any, error) {
	var req v1.ExecutionPromptParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, v1.InvalidParams("invalid execution.prompt request")
	}

	b.mu.Lock()
	r := b.runs[req.AgentRunID]
	client := b.client
	b.mu.Unlock()

	if r == nil || client == nil {
		return nil, v1.NotFound("agent run %s has no live execution", req.AgentRunID)
	}

	if err := b.report(ctx, req.AgentRunID, req.Generation, "running", r.sessionID, ""); err != nil {
		return nil, err
	}

	stopReason, err := client.Prompt(ctx, r.sessionID, req.Text)
	if err != nil {
		return nil, v1.Unavailable("agent prompt failed: %s", err.Error())
	}

	if stopReason == "cancelled" {
		_ = b.report(ctx, req.AgentRunID, req.Generation, "terminal", r.sessionID, "cancelled")
	} else {
		_ = b.report(ctx, req.AgentRunID, req.Generation, "terminal", r.sessionID, stopReason)
	}
	return map[string]any{"stopReason": stopReason}, nil
}

func (b *Bridge) cancel(_ context.Context, params json.RawMessage) (any, error) {
	var req v1.ExecutionCancelParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, v1.InvalidParams("invalid execution.cancel request")
	}

	b.mu.Lock()
	r := b.runs[req.AgentRunID]
	client := b.client
	b.mu.Unlock()

	if r == nil || client == nil {
		return nil, v1.NotFound("agent run %s has no live execution", req.AgentRunID)
	}
	if err := client.Cancel(r.sessionID); err != nil {
		return nil, v1.Unavailable("agent cancel failed: %s", err.Error())
	}
	return map[string]any{"cancelled": true}, nil
}

func (b *Bridge) respond(ctx context.Context, params json.RawMessage) (any, error) {
	var req v1.PermissionRespondParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, v1.InvalidParams("invalid permission.respond request")
	}

	b.mu.Lock()
	pending, ok := b.perms[req.AgentRequestID]
	if ok {
		delete(b.perms, req.AgentRequestID)
	}
	client := b.client
	b.mu.Unlock()

	if !ok || client == nil {
		return nil, v1.NotFound("permission request %s is not pending", req.AgentRequestID)
	}

	outcome := acp.OutcomeForDecision(pending.options, req.Approved, req.OptionID)
	if err := client.RespondToPermission(pending.requestID, outcome); err != nil {
		return nil, v1.Unavailable("permission response failed: %s", err.Error())
	}
	return map[string]any{"outcome": outcome.Outcome}, nil
}

// ensureAgent starts the agent once and pumps its notifications.
func (b *Bridge) ensureAgent(ctx context.Context) (*acp.Client, error) {
	b.mu.Lock()
	if b.client != nil {
		client := b.client
		b.mu.Unlock()
		return client, nil
	}
	b.mu.Unlock()

	client, caps, err := b.launcher.Launch(ctx)
	if err != nil {
		return nil, v1.Unavailable("agent could not be started: %s", err.Error())
	}

	b.mu.Lock()
	b.client = client
	b.caps = caps
	b.mu.Unlock()

	go b.pump(client)
	return client, nil
}

// pump forwards ACP notifications to the core without interpreting them.
func (b *Bridge) pump(client *acp.Client) {
	for {
		select {
		case <-client.Done():
			return
		case update, ok := <-client.Updates():
			if !ok {
				return
			}
			b.publish(update)
		case perm, ok := <-client.Permissions():
			if !ok {
				return
			}
			b.forwardPermission(perm)
		}
	}
}

func (b *Bridge) publish(update acp.Update) {
	agentRunID := b.runIDFor(update.SessionID)
	if agentRunID == "" {
		return
	}
	_ = b.host.Call(context.Background(), v1.MethodEventPublish, v1.PublishEventParams{
		AgentRunID: agentRunID,
		Type:       "agent.raw",
		Protocol:   "acp",
		Method:     update.Method,
		Payload:    update.Payload,
	}, nil)
}

func (b *Bridge) forwardPermission(perm acp.PermissionRequest) {
	agentRunID := b.runIDFor(perm.SessionID)
	if agentRunID == "" {
		return
	}

	payload, err := json.Marshal(map[string]any{
		"toolCall": perm.ToolCall,
		"options":  perm.Options,
	})
	if err != nil {
		return
	}

	b.mu.Lock()
	b.perms[string(perm.RequestID)] = pendingPermission{requestID: perm.RequestID, options: perm.Options}
	b.mu.Unlock()

	ctx := context.Background()
	err = b.host.Call(ctx, v1.MethodPermissionRequest, v1.PermissionRequestParams{
		AgentRunID:     agentRunID,
		AgentRequestID: string(perm.RequestID),
		Payload:        payload,
	}, nil)
	if err != nil {
		// The core could not take the request. Cancel it so the agent aborts
		// instead of proceeding on an approval nobody gave.
		_ = b.client.RespondToPermission(perm.RequestID, acp.PermissionOutcome{Outcome: "cancelled"})
	}
}

func (b *Bridge) runIDFor(sessionID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, r := range b.runs {
		if r.sessionID == sessionID {
			return id
		}
	}
	return ""
}

func (b *Bridge) report(ctx context.Context, agentRunID string, generation int64, state, runtimeSessionID, reason string) error {
	return b.host.Call(ctx, v1.MethodExecutionReport, v1.ExecutionReportParams{
		AgentRunID:       agentRunID,
		Generation:       generation,
		State:            state,
		RuntimeSessionID: runtimeSessionID,
		Reason:           reason,
	}, nil)
}
