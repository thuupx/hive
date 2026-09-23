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
	"sync"
	"time"

	"github.com/thupham/hive/internal/agent"
	"github.com/thupham/hive/internal/apierr"
	"github.com/thupham/hive/internal/control"
	"github.com/thupham/hive/internal/event"
	"github.com/thupham/hive/internal/eventbus"
	"github.com/thupham/hive/internal/ids"
	"github.com/thupham/hive/internal/node"
	"github.com/thupham/hive/internal/permission"
	"github.com/thupham/hive/internal/plugin"
	"github.com/thupham/hive/internal/storage"
	"github.com/thupham/hive/internal/transport"
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

	// Agents are the configured agent ids, used to route execution work.
	Agents []string

	// DefaultAgent is used when a request does not name one.
	DefaultAgent string

	// AllowedUsers is the configured allow list of additional principals.
	AllowedUsers []string

	// ControlSocket enables the local Control API when set. The socket is
	// owner-only, so the caller is authenticated by the operating system.
	ControlSocket string

	// TransportPlugins are the transport plugins this coordinator runs.
	TransportPlugins []plugin.Spec
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

	transportPlugins []plugin.Spec
	control          *control.Service
	gateway          *control.Gateway
	controlSocket    string
	controlListener  net.Listener

	// routers are per-transport and created on first use, because a transport
	// only exists once its plugin connects.
	mu      sync.Mutex
	routers map[string]*transport.Router

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
	events.Bindings = bindingCursor{store: opts.Store}
	events.Register(supervisor)

	nodeServer := node.NewServer(context.Background(), log)
	nodeServer.Store = executionStore{store: opts.Store}

	c := &Coordinator{
		log:              log,
		store:            opts.Store,
		bus:              bus,
		publisher:        eventbus.NewPublisher(bus, opts.Store),
		supervisor:       supervisor,
		events:           events,
		nodeServer:       nodeServer,
		routers:          make(map[string]*transport.Router),
		transportPlugins: opts.TransportPlugins,
	}

	service, err := control.New(control.Options{
		Store:        opts.Store,
		Nodes:        nodeServer,
		Log:          log,
		Agents:       opts.Agents,
		DefaultAgent: opts.DefaultAgent,
		AllowedUsers: opts.AllowedUsers,
		Plugins:      pluginIDs(opts.TransportPlugins),
	})
	if err != nil {
		return nil, err
	}
	c.control = service
	c.gateway = control.NewGateway(service, log)
	c.controlSocket = opts.ControlSocket

	nodeServer.Handle(v1.MethodEventPublish, c.handleEventPublish)
	nodeServer.Handle(v1.MethodPermissionRequest, c.handlePermissionRequest)
	nodeServer.Handle(v1.MethodExecutionReport, c.handleExecutionReport)

	// A transport plugin invokes Control API operations through the plugin link.
	// The acting principal comes from the connection: the plugin is trusted for
	// its own transport, so it may assert a principal of that transport and
	// nothing else.
	for _, method := range []string{
		v1.MethodSessionCreate,
		v1.MethodSessionPrompt,
		v1.MethodSessionCancel,
		v1.MethodSessionStatus,
		v1.MethodSessionList,
		v1.MethodSessionEvents,
		v1.MethodAgentList,
		v1.MethodNodeList,
		v1.MethodCommandGet,
	} {
		supervisor.Handle(method, c.pluginControlHandler)
	}

	// A transport hands normalized inbound input to Hive, which owns the
	// operation semantics.
	supervisor.Handle(v1.MethodTransportInbound, c.handleTransportInbound)

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

	for _, spec := range c.transportPlugins {
		if _, err := c.supervisor.Start(ctx, spec); err != nil {
			// A transport that cannot start must not stop the coordinator:
			// unrelated sessions keep running.
			c.log.Error("transport plugin could not start", "plugin", spec.ID, "error", err)
			continue
		}
		c.log.Info("transport plugin ready", "plugin", spec.ID)
	}

	c.publisher.OnError = func(err error) {
		c.log.Warn("event publication failed", "error", err)
	}
	go func() { _ = c.publisher.Run(ctx) }()
	go func() { _ = c.events.Run(ctx) }()
	go func() { _ = c.nodeServer.WatchLeases(ctx, LeaseSweepInterval, c.onLeaseExpired) }()

	c.log.Info("coordinator listening", "url", c.url)

	if c.controlSocket != "" {
		listener, err := control.ListenUnix(c.controlSocket)
		if err != nil {
			return err
		}
		c.controlListener = listener

		// The socket is owner-only, so the operating system has already
		// authenticated the caller.
		conn := control.Connection{Principal: control.OwnerPrincipal}
		go func() {
			if err := control.ServeUnix(ctx, listener, c.gateway, conn, c.log); err != nil {
				c.log.Error("control listener stopped", "error", err)
			}
		}()
		c.log.Info("control api listening", "socket", c.controlSocket)
	}

	return nil
}

// ControlSocket returns the Control API socket path, if one is enabled.
func (c *Coordinator) ControlSocket() string { return c.controlSocket }

// ControlService exposes the Control API service.
func (c *Coordinator) ControlService() *control.Service { return c.control }

// Gateway exposes the Control API gateway.
func (c *Coordinator) Gateway() *control.Gateway { return c.gateway }

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
	if c.controlListener != nil {
		_ = c.controlListener.Close()
	}
	if c.httpServer != nil {
		return c.httpServer.Close()
	}
	return nil
}

// pluginControlHandler routes a transport plugin's call into the Control API.
func (c *Coordinator) pluginControlHandler(ctx context.Context, call plugin.Call) (any, error) {
	req := &v1.Message{
		JSONRPC: v1.JSONRPCVersion,
		Method:  call.Method,
		Params:  call.Params,
	}

	// The plugin id is the transport name, so a plugin may assert a principal of
	// its own transport and nothing else.
	transport := call.Instance.PluginID
	conn := control.Connection{
		Principal: control.Principal(call.Instance.PluginID),
		Transport: transport,
	}

	actor := ""
	if call.Actor != "" {
		actor = call.Actor
	}
	principal, err := conn.ActingPrincipal(actor)
	if err != nil {
		return nil, err
	}

	return c.gateway.HandleAs(ctx, principal, req)
}

// handleTransportInbound routes a normalized inbound delivery.
func (c *Coordinator) handleTransportInbound(ctx context.Context, call plugin.Call) (any, error) {
	var params v1.TransportInboundParams
	if err := json.Unmarshal(call.Params, &params); err != nil {
		return nil, v1.InvalidParams("invalid transport.inbound request")
	}

	// A plugin speaks only for its own transport, so a transport cannot deliver
	// input on behalf of another one.
	if params.Transport != call.Instance.PluginID {
		return nil, v1.Unauthorized("plugin %s may not speak for transport %s",
			call.Instance.PluginID, params.Transport)
	}

	// The plugin is trusted for its own transport, so it may assert a principal
	// for the user it authenticated — but only one of that transport.
	assertion := control.TransportAssertion{
		Transport: params.Transport,
		Principal: control.Principal(params.Principal),
	}
	if err := assertion.Validate(); err != nil {
		return nil, err
	}

	router, err := c.routerFor(params.Transport)
	if err != nil {
		return nil, apierr.From(err)
	}

	outcome := router.Handle(ctx, envelopeOf(params))
	return wireOutcome(outcome)
}

// routerFor returns the router for a transport, creating it on first use.
func (c *Coordinator) routerFor(name string) (*transport.Router, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if router, ok := c.routers[name]; ok {
		return router, nil
	}

	router, err := transport.NewRouter(transport.RouterOptions{
		Transport: name,
		Control:   c.control,
		Store:     c.store,
		Nodes:     c.nodeServer,
		Log:       c.log,
	})
	if err != nil {
		return nil, err
	}
	c.routers[name] = router
	return router, nil
}

func envelopeOf(params v1.TransportInboundParams) v1.Envelope {
	env := v1.Envelope{
		Transport:      params.Transport,
		ConversationID: params.ConversationID,
		Principal:      params.Principal,
		SourceID:       params.SourceID,
		Kind:           v1.EnvelopeKind(params.Kind),
		ChannelContext: params.ChannelContext,
	}

	switch env.Kind {
	case v1.EnvelopeMessage:
		env.Message = &v1.IncomingMessage{
			Text:        params.Text,
			Attachments: params.Attachments,
		}
	case v1.EnvelopeCommand:
		env.Command = &v1.IncomingCommand{
			Name:     params.Command,
			Method:   params.Method,
			Args:     params.Args,
			ConfigID: params.ConfigID,
		}
	case v1.EnvelopeInteraction:
		env.Interaction = &v1.IncomingInteraction{
			Action: params.Action,
			Method: params.Method,
			Value:  params.Value,
		}
	}
	return env
}

func wireOutcome(outcome *transport.Outcome) (v1.TransportOutcome, error) {
	wire := v1.TransportOutcome{
		Method:    outcome.Method,
		CommandID: outcome.CommandID,
		SessionID: outcome.SessionID,
		RunID:     outcome.RunID,
		Created:   outcome.Created,
		Error:     outcome.Err,
	}

	if outcome.Result != nil {
		encoded, err := json.Marshal(outcome.Result)
		if err != nil {
			return wire, v1.Internal("the outcome could not be encoded")
		}
		wire.Result = encoded
	}
	return wire, nil
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

	// A log that shows only lifecycle events is not enough to diagnose anything.
	// The agent's activity is what a user needs to see, so it is logged as it
	// becomes durable.
	if inserted > 0 {
		c.logAgentActivity(ev)
	}

	return map[string]any{
		"accepted":  true,
		"duplicate": inserted == 0,
		"sequence":  ev.Sequence,
	}, nil
}

// logAgentActivity reports what an agent is doing.
//
// The agent's own stream is preserved as raw data, which is right for storage and
// wrong for a human reading a log. This is the readable form of the same activity.
func (c *Coordinator) logAgentActivity(ev *event.Event) {
	switch ev.Type {
	case v1.EventTool:
		var call v1.ToolCall
		if err := json.Unmarshal(ev.Payload, &call); err != nil {
			return
		}

		attrs := []any{"run", ev.RunID, "tool", call.ToolCallID, "status", call.Status}
		if call.Kind != "" {
			attrs = append(attrs, "kind", call.Kind)
		}
		if call.Title != "" {
			attrs = append(attrs, "title", call.Title)
		}
		switch call.Status {
		case v1.ToolFailed:
			c.log.Warn("agent tool failed", attrs...)
		default:
			c.log.Info("agent tool", attrs...)
		}

	case v1.EventMessage:
		var message struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(ev.Payload, &message); err != nil {
			return
		}
		c.log.Info("agent answered", "run", ev.RunID, "chars", len(message.Text))

	case v1.EventError:
		c.log.Error("agent reported an error", "run", ev.RunID, "payload", string(ev.Payload))

	case v1.EventPermissionRequested:
		c.log.Info("agent asked for permission", "run", ev.RunID)

	case v1.EventPermissionResponded:
		c.log.Info("permission resolved", "run", ev.RunID)
	}
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

	req := permission.New(ids.New("perm"), params.SessionID, params.AgentRunID,
		params.AgentRequestID, params.Payload, expiresAt)

	// The request becomes an event in the same transaction as the record.
	//
	// Without one the request is durable and invisible: the agent waits for an
	// answer, the transport never learns there is a question, and the turn stops
	// with nothing to show for it. A user reports that as the bot getting stuck.
	payload, err := permissionEventPayload(params)
	if err != nil {
		return nil, err
	}

	ev := &event.Event{
		ID:         ids.New("ev"),
		SessionID:  params.SessionID,
		RunID:      params.AgentRunID,
		OriginNode: call.Node.NodeID,
		Timestamp:  time.Now().UTC(),
		Type:       v1.EventPermissionRequested,
		Version:    1,
		Protocol:   "hive",
		Payload:    payload,
	}

	if err := c.store.WriteTx(ctx, func(tx storage.Execer) error {
		if err := c.store.InsertPermissionRequest(ctx, tx, req); err != nil {
			return err
		}
		_, err := c.store.AppendEvents(ctx, tx, ev)
		return err
	}); err != nil {
		return nil, v1.Unavailable("permission request could not be recorded: %s", err.Error())
	}

	c.log.Info("permission requested",
		"session", req.SessionID, "run", req.RunID, "permission", req.ID, "sequence", ev.Sequence)

	return map[string]any{"permissionRequestId": req.ID}, nil
}

// permissionEventPayload is what a transport needs to ask a user.
//
// The agent's own request is passed through and the request id is added, because
// the answer has to name the request and the transport must not have to know how
// Hive identifies one.
func permissionEventPayload(params v1.PermissionRequestParams) (json.RawMessage, error) {
	payload := map[string]any{}
	if len(params.Payload) > 0 {
		if err := json.Unmarshal(params.Payload, &payload); err != nil {
			return nil, v1.InvalidParams("the permission request could not be read")
		}
	}
	payload["agentRequestId"] = params.AgentRequestID

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, v1.Internal("the permission request could not be encoded")
	}
	return encoded, nil
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

	// The read-modify-write happens inside one transaction, and the report is
	// fenced on the execution generation: a report from a superseded generation
	// must not move the current one.
	run, err := c.store.UpdateAgentRunWith(ctx, params.AgentRunID, func(run *agent.AgentRun) error {
		if err := run.CheckGeneration(params.Generation); err != nil {
			return err
		}
		if params.RuntimeSessionID != "" {
			run.RuntimeSessionID = params.RuntimeSessionID
		}
		return applyExecutionReport(run, params)
	})
	if errors.Is(err, storage.ErrNotFound) {
		// The coordinator has no record of this run, so the node should stop
		// claiming it rather than keep reporting.
		return map[string]any{"known": false}, nil
	}
	if err != nil {
		return nil, apierr.From(err)
	}

	// A state change is the lifecycle half of the log. Together with the tool
	// events it answers "what is the agent doing right now".
	c.log.Info("execution reported",
		"run", run.ID,
		"agent", run.AgentID,
		"state", string(run.State),
		"generation", run.ExecutionGeneration,
		"reason", params.Reason,
	)

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

	_, err := c.store.UpdateAgentRunWith(ctx, ref.AgentRunID, func(run *agent.AgentRun) error {
		if err := run.CheckGeneration(ref.Generation); err != nil {
			// A superseded generation is not this run's current execution.
			return apierr.ErrNoChange
		}
		if run.State.IsTerminal() || !run.State.CanTransitionTo(agent.StateInterrupted) {
			return apierr.ErrNoChange
		}
		return run.Transition(agent.StateInterrupted)
	})
	switch {
	case errors.Is(err, apierr.ErrNoChange):
		return
	case err != nil:
		c.log.Warn("could not record an interrupted run", "run", ref.AgentRunID, "error", err)
		return
	}

	c.log.Warn("execution lease expired; run is interrupted",
		"node", nodeID, "run", ref.AgentRunID, "generation", ref.Generation)
}

// pluginIDs lists the plugin identities a coordinator runs.
func pluginIDs(specs []plugin.Spec) []string {
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec.ID)
	}
	return out
}

// bindingCursor advances durable transport binding cursors.
type bindingCursor struct {
	store *storage.Store
}

func (b bindingCursor) Advance(ctx context.Context, transport, conversationID string, sequence int64) error {
	binding, err := b.store.GetBinding(ctx, transport, conversationID)
	if err != nil {
		return err
	}
	return b.store.WriteTx(ctx, func(tx storage.Execer) error {
		return b.store.AdvanceBindingCursor(ctx, tx, binding.ID, sequence)
	})
}

// Conversations names the platform conversations a session belongs to.
func (b bindingCursor) Conversations(ctx context.Context, sessionID string) ([]string, error) {
	bindings, err := b.store.ListBindingsForSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, binding.ConversationID)
	}
	return out, nil
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
