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
	"strings"
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
	opts     Options

	mu     sync.Mutex
	client *acp.Client
	caps   *acp.Capabilities
	runs   map[string]*run

	// pending permission requests, keyed by the agent request id
	perms map[string]pendingPermission
}

type run struct {
	agentRunID string

	// sessionID is the ACP session id. It belongs to this agent runtime and is
	// never interchangeable with another runtime's session id.
	sessionID string

	// hiveSessionID is the Hive session the run belongs to, used to attribute
	// published events.
	hiveSessionID string

	// preamble is context handed over from a previous agent. It is delivered with
	// the first prompt, because ACP has no field for seeding a session.
	preamble string
	seeded   bool

	// answer accumulates the assistant text of the current turn, so the answer to
	// a prompt can be surfaced as one readable message.
	answer strings.Builder
}

// pendingPermission keeps what is needed to answer an ACP permission request in
// the agent's own vocabulary.
type pendingPermission struct {
	requestID json.RawMessage
	options   []acp.PermissionOption
}

// Options configures a bridge.
type Options struct {
	// AuthMethod selects which of the agent's advertised auth methods to use.
	// Empty uses the first one the agent offers.
	AuthMethod string

	// APIKey is passed as _meta.api_key for a method that authenticates that way.
	// It is read from the environment by the caller, never from configuration.
	APIKey string
}

// New returns a bridge over a connected host.
func New(host *sdk.Host, launcher Launcher, opts Options) *Bridge {
	return &Bridge{
		host:     host,
		launcher: launcher,
		opts:     opts,
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

	// ACP requires a working directory to create a session. The node supplies one;
	// if it did not, the directory this process was started in is the honest
	// answer, because that is where the node was running.
	if req.WorkspacePath == "" {
		if dir, err := os.Getwd(); err == nil {
			req.WorkspacePath = dir
		}
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
	b.runs[req.AgentRunID] = &run{
		agentRunID:    req.AgentRunID,
		sessionID:     sessionID,
		hiveSessionID: req.SessionID,
		preamble:      req.Context,
	}
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

	stopReason, err := client.Prompt(ctx, r.sessionID, b.promptText(r, req.Text))
	if err != nil {
		return nil, v1.Unavailable("agent prompt failed: %s", err.Error())
	}

	// The turn is over, so the answer is complete.
	b.publishAnswer(ctx, r)

	if stopReason == "cancelled" {
		_ = b.report(ctx, req.AgentRunID, req.Generation, "terminal", r.sessionID, "cancelled")
	} else {
		_ = b.report(ctx, req.AgentRunID, req.Generation, "terminal", r.sessionID, stopReason)
	}
	return map[string]any{"stopReason": stopReason}, nil
}

// promptText delivers the handoff context with the first prompt.
//
// ACP has no field for seeding a session, so the context is prepended exactly
// once. A later prompt must not repeat it.
func (b *Bridge) promptText(r *run, text string) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	if r.seeded || r.preamble == "" {
		return text
	}
	r.seeded = true
	return r.preamble + "\n\n" + text
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

	// An agent that advertises auth methods refuses to create a session until the
	// client has authenticated.
	if err := b.authenticate(ctx, client, caps); err != nil {
		_ = client.Close()
		return nil, err
	}

	b.mu.Lock()
	b.client = client
	b.caps = caps
	b.mu.Unlock()

	go b.pump(client)
	return client, nil
}

// authenticate satisfies the agent's auth requirement, when it has one.
//
// Hive does not own agent credentials, so which method to use and what to pass are
// configuration rather than something the core can infer. A method that needs a
// browser is the user's to complete, and the error says which method was tried.
func (b *Bridge) authenticate(ctx context.Context, client *acp.Client, caps *acp.Capabilities) error {
	if caps == nil || len(caps.AuthMethods) == 0 {
		return nil
	}

	method := b.opts.AuthMethod
	if method == "" {
		method = caps.AuthMethods[0].ID
	} else if !hasAuthMethod(caps.AuthMethods, method) {
		return v1.InvalidParams("agent does not offer auth method %q; it offers: %s",
			method, authMethodNames(caps.AuthMethods))
	}

	var meta json.RawMessage
	if b.opts.APIKey != "" {
		encoded, err := json.Marshal(map[string]string{"api_key": b.opts.APIKey})
		if err != nil {
			return v1.Internal("the api key could not be encoded")
		}
		meta = encoded
	}

	if err := client.Authenticate(ctx, method, meta); err != nil {
		return v1.Unavailable("agent authentication with %q failed: %s", method, err.Error())
	}
	return nil
}

func hasAuthMethod(methods []acp.AuthMethod, id string) bool {
	for _, method := range methods {
		if method.ID == id {
			return true
		}
	}
	return false
}

func authMethodNames(methods []acp.AuthMethod) string {
	names := make([]string, 0, len(methods))
	for _, method := range methods {
		names = append(names, method.ID)
	}
	return strings.Join(names, ", ")
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
	r := b.runFor(update.SessionID)
	if r == nil {
		return
	}

	// Collect the assistant text while the turn runs. It becomes one readable
	// message when the turn ends; the raw stream is preserved separately.
	if text, ok := agentMessageText(update.Payload); ok {
		b.mu.Lock()
		r.answer.WriteString(text)
		b.mu.Unlock()
	}
	// No event id is set here on purpose: the node owns the buffer and the
	// durable upload, so it assigns a stable id that survives a replay.
	_ = b.host.Call(context.Background(), v1.MethodEventPublish, v1.PublishEventParams{
		AgentRunID: r.agentRunID,
		SessionID:  r.hiveSessionID,
		Type:       "agent.raw",
		Protocol:   "acp",
		Method:     update.Method,
		Payload:    update.Payload,
	}, nil)
}

// publishAnswer surfaces the agent's answer to the turn that just ended.
//
// This is the one normalization Hive needs from an agent stream. Without it the
// answer to a prompt is only reachable as raw protocol chunks, which no client can
// read. Everything else stays protocol-native.
func (b *Bridge) publishAnswer(ctx context.Context, r *run) {
	b.mu.Lock()
	text := strings.TrimSpace(r.answer.String())
	r.answer.Reset()
	b.mu.Unlock()

	if text == "" {
		return
	}

	payload, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return
	}

	_ = b.host.Call(ctx, v1.MethodEventPublish, v1.PublishEventParams{
		AgentRunID: r.agentRunID,
		SessionID:  r.hiveSessionID,
		Type:       v1.EventMessage,
		Protocol:   "acp",
		Method:     "session/update",
		Payload:    payload,
	}, nil)
}

// agentMessageText extracts assistant text from an agent message chunk.
//
// The extraction is deliberately narrow: it reads only what Hive needs to surface
// a turn's answer, and every other notification stays raw.
func agentMessageText(payload json.RawMessage) (string, bool) {
	var chunk struct {
		SessionUpdate string `json:"sessionUpdate"`
		Content       struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return "", false
	}
	if chunk.SessionUpdate != "agent_message_chunk" || chunk.Content.Type != "text" {
		return "", false
	}
	if chunk.Content.Text == "" {
		return "", false
	}
	return chunk.Content.Text, true
}

func (b *Bridge) forwardPermission(perm acp.PermissionRequest) {
	r := b.runFor(perm.SessionID)
	if r == nil {
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
		AgentRunID:     r.agentRunID,
		SessionID:      r.hiveSessionID,
		AgentRequestID: string(perm.RequestID),
		Payload:        payload,
	}, nil)
	if err != nil {
		// The core could not take the request. Cancel it so the agent aborts
		// instead of proceeding on an approval nobody gave.
		_ = b.client.RespondToPermission(perm.RequestID, acp.PermissionOutcome{Outcome: "cancelled"})
	}
}

func (b *Bridge) runFor(sessionID string) *run {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range b.runs {
		if r.sessionID == sessionID {
			return r
		}
	}
	return nil
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
