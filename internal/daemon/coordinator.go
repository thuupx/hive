package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/thupham/hive/internal/agent"
	"github.com/thupham/hive/internal/event"
	"github.com/thupham/hive/internal/eventbus"
	"github.com/thupham/hive/internal/node"
	"github.com/thupham/hive/internal/permission"
	"github.com/thupham/hive/internal/plugin"
	"github.com/thupham/hive/internal/storage"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// LeaseSweepInterval is how often the coordinator checks node leases.
const LeaseSweepInterval = time.Second

// CoordinatorOptions configures a coordinator runtime.
type CoordinatorOptions struct {
	Store       *storage.Store
	Log         *slog.Logger
	Listen      string
	Certificate tls.Certificate
}

// Coordinator is the control plane.
type Coordinator struct {
	log         *slog.Logger
	store       *storage.Store
	bus         *eventbus.Bus
	publisher   *eventbus.Publisher
	supervisor  *plugin.Supervisor
	events      *plugin.Events
	nodeServer  *node.Server
	certificate tls.Certificate
	listen      string

	listener   net.Listener
	httpServer *http.Server
	url        string
}

// NewCoordinator wires the control plane without starting it.
func NewCoordinator(opts CoordinatorOptions) (*Coordinator, error) {
	if opts.Store == nil {
		return nil, errors.New("daemon: coordinator requires a store")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	bus := eventbus.NewBus()
	supervisor := plugin.NewSupervisor(log)
	events := plugin.NewEvents(bus, supervisor, log)
	events.Register(supervisor)

	nodeServer := node.NewServer(context.Background(), log)
	nodeServer.Store = executionStore{store: opts.Store}

	c := &Coordinator{
		log:        log,
		store:      opts.Store,
		bus:        bus,
		publisher:  eventbus.NewPublisher(bus, opts.Store),
		supervisor: supervisor,
		events:     events,
		nodeServer: nodeServer,
	}

	nodeServer.Handle(v1.MethodEventPublish, c.handleEventPublish)
	nodeServer.Handle(v1.MethodPermissionRequest, c.handlePermissionRequest)
	nodeServer.Handle(v1.MethodExecutionReport, c.handleExecutionReport)

	if opts.Certificate.Certificate == nil {
		return nil, errors.New("daemon: coordinator requires a TLS certificate")
	}
	c.certificate = opts.Certificate
	c.listen = opts.Listen

	return c, nil
}

// Start begins serving the node link and the background loops.
func (c *Coordinator) Start(ctx context.Context) error {
	address := c.listen
	if address == "" {
		address = "127.0.0.1:0"
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("daemon: coordinator listen on %s: %w", address, err)
	}
	c.listener = listener
	c.url = "wss://" + listener.Addr().String() + "/node"

	mux := http.NewServeMux()
	mux.Handle("/node", c.nodeServer)

	c.httpServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	tlsListener := tls.NewListener(listener, node.ServerTLSConfig(c.certificate))
	go func() {
		if err := c.httpServer.Serve(tlsListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			c.log.Error("coordinator node listener stopped", "error", err)
		}
	}()

	c.publisher.OnError = func(err error) {
		c.log.Warn("event publication failed", "error", err)
	}
	go func() { _ = c.publisher.Run(ctx) }()
	go func() { _ = c.events.Run(ctx) }()
	go func() { _ = c.nodeServer.WatchLeases(ctx, LeaseSweepInterval, c.onLeaseExpired) }()

	c.log.Info("coordinator listening", "url", c.url)
	return nil
}

// URL returns the node link URL a node should connect to.
func (c *Coordinator) URL() string { return c.url }

// NodeServer exposes the node link for tests and for the control plane.
func (c *Coordinator) NodeServer() *node.Server { return c.nodeServer }

// Supervisor exposes the plugin supervisor.
func (c *Coordinator) Supervisor() *plugin.Supervisor { return c.supervisor }

// Bus exposes the realtime event bus.
func (c *Coordinator) Bus() *eventbus.Bus { return c.bus }

// Store exposes the coordinator store.
func (c *Coordinator) Store() *storage.Store { return c.store }

// Close stops the listener and releases plugins.
func (c *Coordinator) Close() error {
	c.supervisor.StopAll()
	c.bus.Close()
	if c.httpServer != nil {
		return c.httpServer.Close()
	}
	return nil
}

// handleEventPublish makes a node-produced event durable.
//
// The node assigns the event id, so replaying a buffered event is deduplicated
// here rather than creating a second durable event.
func (c *Coordinator) handleEventPublish(ctx context.Context, call node.Call) (any, error) {
	var params v1.PublishEventParams
	if err := json.Unmarshal(call.Params, &params); err != nil {
		return nil, v1.InvalidParams("invalid event.publish request")
	}
	switch {
	case params.EventID == "":
		return nil, v1.InvalidParams("event id is required")
	case params.SessionID == "":
		return nil, v1.InvalidParams("session id is required")
	case params.Type == "":
		return nil, v1.InvalidParams("event type is required")
	}

	ev := &event.Event{
		ID:         params.EventID,
		SessionID:  params.SessionID,
		RunID:      params.AgentRunID,
		OriginNode: call.Node.NodeID,
		Timestamp:  time.Now().UTC(),
		Type:       params.Type,
		Version:    1,
		Protocol:   params.Protocol,
		Method:     params.Method,
		Payload:    params.Payload,
	}

	inserted := 0
	err := c.store.WriteTx(ctx, func(tx storage.Execer) error {
		n, err := c.store.AppendEvents(ctx, tx, ev)
		inserted = n
		return err
	})
	if err != nil {
		return nil, v1.Unavailable("event could not be persisted: %s", err.Error())
	}

	return map[string]any{
		"accepted":  true,
		"duplicate": inserted == 0,
		"sequence":  ev.Sequence,
	}, nil
}

// handlePermissionRequest records a pending permission request.
//
// A pending request is durable coordinator state, so a replacement coordinator
// can recover it and a response submitted later is still idempotent.
func (c *Coordinator) handlePermissionRequest(ctx context.Context, call node.Call) (any, error) {
	var params v1.PermissionRequestParams
	if err := json.Unmarshal(call.Params, &params); err != nil {
		return nil, v1.InvalidParams("invalid permission.request request")
	}
	switch {
	case params.AgentRunID == "":
		return nil, v1.InvalidParams("agent run id is required")
	case params.SessionID == "":
		return nil, v1.InvalidParams("session id is required")
	case params.AgentRequestID == "":
		return nil, v1.InvalidParams("agent request id is required")
	}

	var expiresAt time.Time
	if params.ExpiresInSeconds > 0 {
		expiresAt = time.Now().UTC().Add(time.Duration(params.ExpiresInSeconds) * time.Second)
	}

	req := permission.New(newID("perm"), params.SessionID, params.AgentRunID,
		params.AgentRequestID, params.Payload, expiresAt)

	if err := c.store.WriteTx(ctx, func(tx storage.Execer) error {
		return c.store.InsertPermissionRequest(ctx, tx, req)
	}); err != nil {
		return nil, v1.Unavailable("permission request could not be recorded: %s", err.Error())
	}

	c.log.Info("permission requested",
		"session", req.SessionID, "run", req.RunID, "permission", req.ID)

	return map[string]any{"permissionRequestId": req.ID}, nil
}

// handleExecutionReport applies a node's report about one execution generation.
func (c *Coordinator) handleExecutionReport(ctx context.Context, call node.Call) (any, error) {
	var params v1.ExecutionReportParams
	if err := json.Unmarshal(call.Params, &params); err != nil {
		return nil, v1.InvalidParams("invalid execution.report request")
	}
	if params.AgentRunID == "" {
		return nil, v1.InvalidParams("agent run id is required")
	}

	run, err := c.store.GetAgentRun(ctx, params.AgentRunID)
	if errors.Is(err, storage.ErrNotFound) {
		// The coordinator has no record of this run, so the node should stop
		// claiming it rather than keep reporting.
		return map[string]any{"known": false}, nil
	}
	if err != nil {
		return nil, v1.Unavailable("agent run could not be read: %s", err.Error())
	}

	// Fence on the execution generation: a report from a superseded generation
	// must not move the current one.
	if err := run.CheckGeneration(params.Generation); err != nil {
		return nil, v1.AsError(err)
	}

	if params.RuntimeSessionID != "" {
		run.RuntimeSessionID = params.RuntimeSessionID
	}
	if err := applyExecutionReport(run, params); err != nil {
		return nil, v1.AsError(err)
	}

	if err := c.store.WriteTx(ctx, func(tx storage.Execer) error {
		return c.store.UpdateAgentRun(ctx, tx, run)
	}); err != nil {
		return nil, v1.Unavailable("agent run could not be updated: %s", err.Error())
	}

	return map[string]any{"known": true, "state": string(run.State)}, nil
}

// applyExecutionReport maps a node's report onto the AgentRun state machine.
func applyExecutionReport(run *agent.AgentRun, params v1.ExecutionReportParams) error {
	switch params.State {
	case string(agent.ExecutionAbsent):
		// Nothing to change: absence is evidence the caller reconciles with,
		// not a state of its own.
		return nil
	case string(agent.ExecutionStarting):
		return run.Transition(agent.StateStarting)
	case string(agent.ExecutionRunning):
		return run.Transition(agent.StateRunning)
	case string(agent.ExecutionTerminal):
		switch params.Reason {
		case "cancelled":
			return run.Transition(agent.StateCancelled)
		case "failed":
			return run.Transition(agent.StateFailed)
		case "interrupted":
			return run.Transition(agent.StateInterrupted)
		default:
			return run.Transition(agent.StateCompleted)
		}
	default:
		return v1.InvalidParams("unknown execution state %q", params.State)
	}
}

// onLeaseExpired classifies a run as interrupted when its node lease expires.
//
// This never starts a replacement execution. The original execution may still be
// alive, and duplicating it could repeat side effects.
func (c *Coordinator) onLeaseExpired(nodeID string, ref v1.ExecutionRef) {
	ctx := context.Background()

	run, err := c.store.GetAgentRun(ctx, ref.AgentRunID)
	if err != nil {
		return
	}
	if err := run.CheckGeneration(ref.Generation); err != nil {
		// A superseded generation is not this run's current execution.
		return
	}
	if run.State.IsTerminal() {
		return
	}
	if !run.State.CanTransitionTo(agent.StateInterrupted) {
		return
	}

	if err := run.Transition(agent.StateInterrupted); err != nil {
		return
	}
	if err := c.store.WriteTx(ctx, func(tx storage.Execer) error {
		return c.store.UpdateAgentRun(ctx, tx, run)
	}); err != nil {
		c.log.Warn("could not record an interrupted run", "run", ref.AgentRunID, "error", err)
		return
	}

	c.log.Warn("execution lease expired; run is interrupted",
		"node", nodeID, "run", ref.AgentRunID, "generation", ref.Generation)
}

// executionStore answers the reconnect reconciliation questions from the store.
type executionStore struct {
	store *storage.Store
}

func (e executionStore) LiveExecutions(ctx context.Context, nodeID string) (map[string]int64, error) {
	runs, err := e.store.ListAgentRunsByNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}

	live := make(map[string]int64, len(runs))
	for _, run := range runs {
		if run.IsTerminal() {
			continue
		}
		live[run.ID] = run.ExecutionGeneration
	}
	return live, nil
}

func (e executionStore) HasEvent(ctx context.Context, eventID string) (bool, error) {
	return e.store.HasEvent(ctx, eventID)
}
