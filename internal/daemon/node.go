package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/thupham/hive/internal/event"
	"github.com/thupham/hive/internal/ids"
	"github.com/thupham/hive/internal/node"
	"github.com/thupham/hive/internal/plugin"
	"github.com/thupham/hive/internal/storage"
	"github.com/thupham/hive/internal/workspace"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// UploadTimeout bounds one upload to the coordinator.
const UploadTimeout = 10 * time.Second

// FlushInterval is how often a node retries its buffered events.
const FlushInterval = time.Second

// NodeOptions configures a node runtime.
type NodeOptions struct {
	Store *storage.Store
	Log   *slog.Logger

	NodeID       string
	Version      string
	LeaseSeconds int

	CoordinatorURL string
	TLSConfig      *tls.Config

	// WorkspaceDir is where a run works when the coordinator names no location.
	// Empty uses the node process's working directory.
	WorkspaceDir string

	// AgentPlugins are the agent plugins this node runs.
	AgentPlugins []plugin.Spec

	// AgentPluginID is the plugin that performs execution work.
	AgentPluginID string
}

// Node is the execution plane.
type Node struct {
	opts       NodeOptions
	log        *slog.Logger
	store      *storage.Store
	supervisor *plugin.Supervisor
	client     *node.Client
	executor   *pluginExecutor

	eventPrefix string
	seq         atomic.Int64
}

// NewNode wires the execution plane without starting it.
func NewNode(opts NodeOptions) (*Node, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("daemon: node requires a store")
	case opts.CoordinatorURL == "":
		return nil, errors.New("daemon: node requires a coordinator url")
	case opts.AgentPluginID == "":
		return nil, errors.New("daemon: node requires an agent plugin id")
	}

	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	n := &Node{
		opts:        opts,
		log:         log,
		store:       opts.Store,
		eventPrefix: ids.New("ev"),
	}
	n.supervisor = plugin.NewSupervisor(log)
	n.executor = &pluginExecutor{
		supervisor:   n.supervisor,
		pluginID:     opts.AgentPluginID,
		workspaceDir: opts.WorkspaceDir,
	}
	n.client = node.New(node.Options{
		NodeID:       opts.NodeID,
		Version:      opts.Version,
		Agents:       agentIDs(opts.AgentPlugins),
		LeaseSeconds: opts.LeaseSeconds,
		Executor:     n.executor,
		Log:          log,
	})

	// Plugin to node: the node owns the buffer and the upload, so agent plugins
	// report to the node rather than to the coordinator.
	n.supervisor.Handle(v1.MethodEventPublish, n.handleEventPublish)
	n.supervisor.Handle(v1.MethodExecutionReport, n.handleExecutionReport)
	n.supervisor.Handle(v1.MethodPermissionRequest, n.handlePermissionRequest)

	return n, nil
}

// Run starts the agent plugins and serves the coordinator link until ctx ends.
func (n *Node) Run(ctx context.Context) error {
	for _, spec := range n.opts.AgentPlugins {
		if _, err := n.supervisor.Start(ctx, spec); err != nil {
			return fmt.Errorf("daemon: node: start plugin %s: %w", spec.ID, err)
		}
	}
	defer n.supervisor.StopAll()

	go n.flushBuffer(ctx)

	return n.client.Run(ctx, n.opts.CoordinatorURL, n.opts.TLSConfig)
}

// Client exposes the node link.
func (n *Node) Client() *node.Client { return n.client }

// Supervisor exposes the plugin supervisor.
func (n *Node) Supervisor() *plugin.Supervisor { return n.supervisor }

// Store exposes the node store.
func (n *Node) Store() *storage.Store { return n.store }

// handleEventPublish buffers an agent event and uploads it.
//
// The event is buffered durably before the upload is attempted, so an event is
// never lost just because the coordinator is unreachable. A failed upload is not
// an error for the agent: the buffer replays on the next synchronization.
func (n *Node) handleEventPublish(ctx context.Context, call plugin.Call) (any, error) {
	var params v1.PublishEventParams
	if err := json.Unmarshal(call.Params, &params); err != nil {
		return nil, v1.InvalidParams("invalid event.publish request")
	}
	switch {
	case params.SessionID == "":
		return nil, v1.InvalidParams("session id is required")
	case params.Type == "":
		return nil, v1.InvalidParams("event type is required")
	}

	ev := &event.Event{
		ID:         n.nextEventID(),
		SessionID:  params.SessionID,
		RunID:      params.AgentRunID,
		OriginNode: n.opts.NodeID,
		Timestamp:  time.Now().UTC(),
		Type:       params.Type,
		Version:    1,
		Protocol:   params.Protocol,
		Method:     params.Method,
		Payload:    params.Payload,
	}

	if err := n.store.WriteTx(ctx, func(tx storage.Execer) error {
		_, err := n.store.BufferEvent(ctx, tx, ev)
		return err
	}); err != nil {
		return nil, v1.Unavailable("event could not be buffered: %s", err.Error())
	}

	result, err := n.upload(ctx, ev)
	if err != nil {
		return map[string]any{"buffered": true, "uploaded": false}, nil
	}
	return map[string]any{
		"buffered":  false,
		"uploaded":  true,
		"duplicate": result.Duplicate,
	}, nil
}

// handleExecutionReport records the execution locally and forwards it.
//
// The local record is execution evidence: it is what the node reports when the
// coordinator asks what it actually has.
func (n *Node) handleExecutionReport(ctx context.Context, call plugin.Call) (any, error) {
	var params v1.ExecutionReportParams
	if err := json.Unmarshal(call.Params, &params); err != nil {
		return nil, v1.InvalidParams("invalid execution.report request")
	}
	if params.AgentRunID == "" {
		return nil, v1.InvalidParams("agent run id is required")
	}

	if err := n.store.WriteTx(ctx, func(tx storage.Execer) error {
		return n.store.PutNodeExecution(ctx, tx, &storage.NodeExecution{
			AgentRunID:       params.AgentRunID,
			Generation:       params.Generation,
			RuntimeSessionID: params.RuntimeSessionID,
			State:            params.State,
		})
	}); err != nil {
		return nil, v1.Unavailable("execution could not be recorded: %s", err.Error())
	}

	callCtx, cancel := context.WithTimeout(ctx, UploadTimeout)
	defer cancel()

	var out map[string]any
	if err := n.client.Call(callCtx, v1.MethodExecutionReport, params, &out); err != nil {
		// The node keeps its own evidence, so a failed forward is recoverable
		// during the next synchronization.
		n.log.Warn("execution report could not be forwarded",
			"run", params.AgentRunID, "error", err)
		return map[string]any{"recorded": true, "forwarded": false}, nil
	}
	return map[string]any{"recorded": true, "forwarded": true}, nil
}

// handlePermissionRequest relays a permission request to the coordinator.
//
// This one is not buffered. A permission request that cannot be relayed must
// fail closed rather than be answered from a stale local view, so the agent is
// told the relay failed and cancels the operation.
func (n *Node) handlePermissionRequest(ctx context.Context, call plugin.Call) (any, error) {
	var params v1.PermissionRequestParams
	if err := json.Unmarshal(call.Params, &params); err != nil {
		return nil, v1.InvalidParams("invalid permission.request request")
	}
	if params.AgentRequestID == "" {
		return nil, v1.InvalidParams("agent request id is required")
	}

	callCtx, cancel := context.WithTimeout(ctx, UploadTimeout)
	defer cancel()

	var out map[string]any
	if err := n.client.Call(callCtx, v1.MethodPermissionRequest, params, &out); err != nil {
		return nil, v1.Unavailable("permission request could not be relayed: %s", err.Error())
	}
	return out, nil
}

// upload sends one buffered event to the coordinator and clears the buffer entry
// once it is durable there.
func (n *Node) upload(ctx context.Context, ev *event.Event) (uploadResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, UploadTimeout)
	defer cancel()

	var result uploadResult
	err := n.client.Call(callCtx, v1.MethodEventPublish, v1.PublishEventParams{
		EventID:    ev.ID,
		AgentRunID: ev.RunID,
		SessionID:  ev.SessionID,
		Type:       ev.Type,
		Protocol:   ev.Protocol,
		Method:     ev.Method,
		Payload:    ev.Payload,
	}, &result)
	if err != nil {
		return uploadResult{}, err
	}

	if err := n.store.WriteTx(ctx, func(tx storage.Execer) error {
		return n.store.DeleteBufferedEvents(ctx, tx, ev.ID)
	}); err != nil {
		n.log.Warn("buffered event could not be cleared", "event", ev.ID, "error", err)
	}
	return result, nil
}

// flushBuffer retries buffered events while the link is up.
func (n *Node) flushBuffer(ctx context.Context) {
	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n.client.ConnectionGeneration() == 0 {
				continue
			}
			buffered, err := n.store.BufferedEvents(ctx, 100)
			if err != nil {
				continue
			}
			for _, b := range buffered {
				if _, err := n.upload(ctx, b.Event); err != nil {
					// The link went away again; the next tick retries.
					break
				}
			}
		}
	}
}

// agentIDs lists the agents a node offers, derived from its plugin specs.
//
// The plugin id is the agent name, so configuration alone decides which agents
// a node runs and the coordinator routes execution work by that name.
func agentIDs(specs []plugin.Spec) []string {
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec.ID)
	}
	return out
}

// nextEventID returns a stable event id that is unique across node restarts.
//
// The random prefix matters: a counter alone would restart at 1 and a new event
// could be deduplicated as a duplicate of an old one.
func (n *Node) nextEventID() string {
	return fmt.Sprintf("%s_%d", n.eventPrefix, n.seq.Add(1))
}

type uploadResult struct {
	Accepted  bool  `json:"accepted"`
	Duplicate bool  `json:"duplicate"`
	Sequence  int64 `json:"sequence"`
}

// pluginExecutor performs node execution work by delegating to an agent plugin.
//
// The node owns the live execution; the plugin owns the agent runtime. Neither
// needs to know the other's protocol beyond the execution surface.
type pluginExecutor struct {
	supervisor *plugin.Supervisor
	pluginID   string

	// workspaceDir is where a run works when the coordinator names no location.
	workspaceDir string
}

// resolveWorkspace returns the working directory for an execution.
//
// A run must have one: an agent that writes files needs to know where, and ACP
// requires a cwd to create a session. A location the coordinator named wins, then
// the configured workspace.
//
// It never falls back to the process's own directory. A daemon started by the
// system runs in the filesystem root, and an agent told to work there can read
// and write everything its user can — that is not a working directory, it is the
// whole machine. Refusing is the honest answer: the error says what to fix.
func (e *pluginExecutor) resolveWorkspace(requested string) (string, error) {
	dir, err := workspace.Directory(requested, e.workspaceDir)
	if err != nil {
		return "", v1.InvalidRequest("%s", err.Error())
	}
	return dir, nil
}

// instanceFor picks the plugin that runs a given agent.
//
// A node runs several agents, so the execution surface names the agent. Falling
// back to the default plugin would silently run the wrong agent.
func (e *pluginExecutor) instanceFor(agentID string) (*plugin.Instance, error) {
	if agentID == "" {
		agentID = e.pluginID
	}
	inst, ok := e.supervisor.Instance(agentID)
	if !ok {
		return nil, v1.Unavailable("agent plugin %s is not running", agentID)
	}
	return inst, nil
}

func (e *pluginExecutor) Start(ctx context.Context, req v1.ExecutionStartParams) (string, error) {
	inst, err := e.instanceFor(req.AgentID)
	if err != nil {
		return "", err
	}
	workspace, err := e.resolveWorkspace(req.WorkspacePath)
	if err != nil {
		return "", err
	}
	req.WorkspacePath = workspace

	var out struct {
		RuntimeSessionID string `json:"runtimeSessionId"`
	}
	if err := inst.Call(ctx, v1.MethodExecutionStart, req, &out); err != nil {
		return "", err
	}
	if out.RuntimeSessionID == "" {
		return "", v1.Unavailable("agent plugin returned no runtime session id")
	}
	return out.RuntimeSessionID, nil
}

func (e *pluginExecutor) Prompt(ctx context.Context, req v1.ExecutionPromptParams) error {
	inst, err := e.instanceFor(req.AgentID)
	if err != nil {
		return err
	}
	return inst.Call(ctx, v1.MethodExecutionPrompt, req, nil)
}

func (e *pluginExecutor) Cancel(ctx context.Context, req v1.ExecutionCancelParams) error {
	inst, err := e.instanceFor(req.AgentID)
	if err != nil {
		return err
	}
	return inst.Call(ctx, v1.MethodExecutionCancel, req, nil)
}

func (e *pluginExecutor) Respond(ctx context.Context, req v1.PermissionRespondParams) error {
	inst, err := e.instanceFor(req.AgentID)
	if err != nil {
		return err
	}
	return inst.Call(ctx, v1.MethodPermissionRespond, req, nil)
}

// Live reports whether an execution is still held.
//
// Anything other than a plain "no" is reported as live. Marking a run interrupted
// abandons a turn, so the answer has to be certain: an agent that does not
// implement the question is an agent that might be working, and losing that work
// is worse than leaving a run alone.
func (e *pluginExecutor) Live(ctx context.Context, req v1.ExecutionLiveParams) (*v1.ExecutionLiveResult, error) {
	inst, err := e.instanceFor(req.AgentID)
	if err != nil {
		return &v1.ExecutionLiveResult{Live: true}, nil
	}

	var out v1.ExecutionLiveResult
	if err := inst.Call(ctx, v1.MethodExecutionLive, req, &out); err != nil {
		return &v1.ExecutionLiveResult{Live: true}, nil
	}
	return &out, nil
}

func (e *pluginExecutor) Config(ctx context.Context, req v1.ExecutionConfigParams) (*v1.ExecutionConfigResult, error) {
	inst, err := e.instanceFor(req.AgentID)
	if err != nil {
		return nil, err
	}

	var out v1.ExecutionConfigResult
	if err := inst.Call(ctx, v1.MethodExecutionConfig, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
