package node

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/thupham/hive/internal/event"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// DialTimeout bounds how long a connection attempt may take.
const DialTimeout = 10 * time.Second

// Executor performs execution work on behalf of the coordinator.
//
// The node owns the live execution. The coordinator owns logical state, so the
// executor reports what actually happened rather than what was requested.
type Executor interface {
	Start(ctx context.Context, req v1.ExecutionStartParams) (runtimeSessionID string, err error)
	Prompt(ctx context.Context, req v1.ExecutionPromptParams) error
	Cancel(ctx context.Context, req v1.ExecutionCancelParams) error

	// Respond relays a permission decision to the agent that asked for it.
	Respond(ctx context.Context, req v1.PermissionRespondParams) error
}

// Options configures a node.
type Options struct {
	NodeID       string
	Version      string
	LeaseSeconds int

	// Agents lists the agent ids this node can run. The coordinator routes
	// execution work by agent.
	Agents []string

	Executor Executor
	Log      *slog.Logger

	// ReconnectDelay is the pause before reconnecting. Zero uses a default.
	ReconnectDelay time.Duration
}

// Client is the node side of the coordinator link.
type Client struct {
	opts Options

	mu         sync.Mutex
	ready      v1.NodeReady
	executions map[string]v1.ExecutionRef
	buffer     []*event.Event
	buffered   map[string]*event.Event
	requested  []*event.Event
	peer       *v1.Peer
}

// New returns a node client.
func New(opts Options) *Client {
	return &Client{
		opts:       opts,
		executions: make(map[string]v1.ExecutionRef),
		buffered:   make(map[string]*event.Event),
	}
}

// Call invokes a method on the coordinator over the current link.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	peer := c.currentPeer()
	if peer == nil {
		return v1.Unavailable("node is not connected to the coordinator")
	}
	return peer.Call(ctx, method, params, result)
}

// ConnectionGeneration returns the current link's generation, or 0 when
// disconnected.
func (c *Client) ConnectionGeneration() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready.ConnectionGeneration
}

// Executions returns the executions this node believes it owns.
func (c *Client) Executions() []v1.ExecutionRef {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]v1.ExecutionRef, 0, len(c.executions))
	for _, ref := range c.executions {
		out = append(out, ref)
	}
	return out
}

// BufferedIDs returns the ids of events buffered while disconnected.
func (c *Client) BufferedIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]string, 0, len(c.buffer))
	for _, ev := range c.buffer {
		out = append(out, ev.ID)
	}
	return out
}

// Pending returns how many events are still buffered locally.
func (c *Client) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.buffer)
}

// RequestedEvents returns the buffered events the coordinator asked to replay
// during the last synchronization, and clears the request.
func (c *Client) RequestedEvents() []*event.Event {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := c.requested
	c.requested = nil
	return out
}

// Buffer stores an event locally while the coordinator is unreachable.
//
// The buffer is node-local execution state, not a second source of truth. The
// authoritative stream sequence is assigned by the coordinator when the event
// becomes durable; the local order is only a synchronization aid.
func (c *Client) Buffer(ev *event.Event) {
	if ev == nil || ev.ID == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.buffered[ev.ID]; exists {
		return
	}
	c.buffered[ev.ID] = ev
	c.buffer = append(c.buffer, ev)
}

// AckBuffered drops buffered events the coordinator has accepted.
func (c *Client) AckBuffered(eventIDs ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	accepted := make(map[string]bool, len(eventIDs))
	for _, id := range eventIDs {
		accepted[id] = true
		delete(c.buffered, id)
	}

	kept := c.buffer[:0]
	for _, ev := range c.buffer {
		if !accepted[ev.ID] {
			kept = append(kept, ev)
		}
	}
	c.buffer = kept
}

// Run connects, serves, and reconnects until ctx is cancelled.
func (c *Client) Run(ctx context.Context, url string, tlsConfig *tls.Config) error {
	delay := c.opts.ReconnectDelay
	if delay <= 0 {
		delay = time.Second
	}

	for {
		err := c.session(ctx, url, tlsConfig)
		if ctx.Err() != nil {
			return nil
		}
		c.log().Warn("node link ended; reconnecting", "node", c.opts.NodeID, "error", err)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

func (c *Client) session(ctx context.Context, url string, tlsConfig *tls.Config) error {
	ws, err := dialWebSocket(ctx, url, tlsConfig)
	if err != nil {
		return err
	}

	conn := websocket.NetConn(context.WithoutCancel(ctx), ws, websocket.MessageText)
	peer := v1.NewPeer(v1.NewStream(conn, conn, conn))
	peer.Start()

	c.mu.Lock()
	c.peer = peer
	c.ready = v1.NodeReady{}
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		if c.peer == peer {
			c.peer = nil
		}
		c.ready = v1.NodeReady{}
		c.mu.Unlock()
		_ = peer.Close()
	}()

	if err := c.handshake(ctx); err != nil {
		return err
	}
	if err := c.sync(ctx); err != nil {
		return err
	}

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.heartbeatLoop(sessionCtx)

	for {
		select {
		case <-sessionCtx.Done():
			return nil
		case <-peer.Done():
			return peer.Err()
		case req, ok := <-peer.Requests():
			if !ok {
				return peer.Err()
			}
			c.handleRequest(sessionCtx, peer, req)
		case _, ok := <-peer.Notifications():
			if !ok {
				return peer.Err()
			}
		}
	}
}

func (c *Client) handshake(ctx context.Context) error {
	peer := c.currentPeer()
	if peer == nil {
		return fmt.Errorf("node: not connected")
	}

	callCtx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()

	var ready v1.NodeReady
	err := peer.Call(callCtx, v1.MethodNodeHello, v1.NodeHello{
		NodeID:          c.opts.NodeID,
		Version:         c.opts.Version,
		ProtocolVersion: v1.Current(),
		Agents:          c.opts.Agents,
		LeaseSeconds:    c.opts.LeaseSeconds,
	}, &ready)
	if err != nil {
		return fmt.Errorf("node: handshake: %w", err)
	}

	c.mu.Lock()
	c.ready = ready
	c.mu.Unlock()
	return nil
}

// sync presents what the node believes it owns so the coordinator can
// reconcile. It runs on every connection, not only on a detected reconnect.
func (c *Client) sync(ctx context.Context) error {
	peer := c.currentPeer()
	if peer == nil {
		return fmt.Errorf("node: not connected")
	}

	req := v1.NodeSyncRequest{
		NodeID:               c.opts.NodeID,
		ConnectionGeneration: c.ConnectionGeneration(),
		ActiveExecutions:     c.Executions(),
		BufferedEventIDs:     c.BufferedIDs(),
	}

	var resp v1.NodeSyncResponse
	if err := peer.Call(ctx, v1.MethodNodeSync, req, &resp); err != nil {
		return fmt.Errorf("node: sync: %w", err)
	}

	c.mu.Lock()
	for _, ref := range resp.ObsoleteExecutions {
		delete(c.executions, ref.AgentRunID)
	}
	for _, id := range resp.RequestedEventIDs {
		if ev, ok := c.buffered[id]; ok {
			c.requested = append(c.requested, ev)
		}
	}
	c.mu.Unlock()

	c.log().Info("node synchronized",
		"node", c.opts.NodeID,
		"generation", c.ConnectionGeneration(),
		"active", len(req.ActiveExecutions),
		"buffered", len(req.BufferedEventIDs),
		"requested", len(resp.RequestedEventIDs),
		"unknown", len(resp.UnknownExecutions),
	)
	return nil
}

func (c *Client) heartbeatLoop(ctx context.Context) {
	lease := time.Duration(c.opts.LeaseSeconds) * time.Second
	if lease <= 0 {
		lease = DefaultLeaseSeconds * time.Second
	}
	interval := lease / 3
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			peer := c.currentPeer()
			if peer == nil {
				return
			}
			var out map[string]any
			hb := v1.NodeHeartbeat{
				NodeID:               c.opts.NodeID,
				ConnectionGeneration: c.ConnectionGeneration(),
				Executions:           c.Executions(),
				SentAt:               time.Now().UTC(),
			}
			if err := peer.Call(ctx, v1.MethodNodeHeartbeat, hb, &out); err != nil {
				return
			}
		}
	}
}

func (c *Client) handleRequest(ctx context.Context, peer *v1.Peer, req *v1.Message) {
	id := req.RequestID()

	switch req.Method {
	case v1.MethodExecutionStart:
		var params v1.ExecutionStartParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = peer.RespondError(id, v1.InvalidParams("invalid execution.start request"))
			return
		}
		if c.opts.Executor == nil {
			_ = peer.RespondError(id, v1.Unavailable("node has no executor"))
			return
		}

		runtimeSessionID, err := c.opts.Executor.Start(ctx, params)
		if err != nil {
			_ = peer.RespondError(id, v1.AsError(err))
			return
		}
		c.recordExecution(params.AgentRunID, params.Generation, runtimeSessionID)
		_ = peer.Respond(id, map[string]any{"runtimeSessionId": runtimeSessionID})

	case v1.MethodExecutionPrompt:
		var params v1.ExecutionPromptParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = peer.RespondError(id, v1.InvalidParams("invalid execution.prompt request"))
			return
		}
		if c.opts.Executor == nil {
			_ = peer.RespondError(id, v1.Unavailable("node has no executor"))
			return
		}
		if err := c.opts.Executor.Prompt(ctx, params); err != nil {
			_ = peer.RespondError(id, v1.AsError(err))
			return
		}
		_ = peer.Respond(id, map[string]any{"ok": true})

	case v1.MethodExecutionCancel:
		var params v1.ExecutionCancelParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = peer.RespondError(id, v1.InvalidParams("invalid execution.cancel request"))
			return
		}
		if c.opts.Executor == nil {
			_ = peer.RespondError(id, v1.Unavailable("node has no executor"))
			return
		}
		if err := c.opts.Executor.Cancel(ctx, params); err != nil {
			_ = peer.RespondError(id, v1.AsError(err))
			return
		}
		_ = peer.Respond(id, map[string]any{"ok": true})

	case v1.MethodPermissionRespond:
		var params v1.PermissionRespondParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = peer.RespondError(id, v1.InvalidParams("invalid permission.respond request"))
			return
		}
		if c.opts.Executor == nil {
			_ = peer.RespondError(id, v1.Unavailable("node has no executor"))
			return
		}
		if err := c.opts.Executor.Respond(ctx, params); err != nil {
			_ = peer.RespondError(id, v1.AsError(err))
			return
		}
		_ = peer.Respond(id, map[string]any{"ok": true})

	default:
		// An unknown method is reported as such regardless of what the node can
		// execute, so the error names the real reason.
		_ = peer.RespondError(id, v1.MethodNotFound(req.Method))
	}
}

func (c *Client) recordExecution(agentRunID string, generation int64, runtimeSessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.executions[agentRunID] = v1.ExecutionRef{
		AgentRunID:      agentRunID,
		Generation:      generation,
		NodeExecutionID: runtimeSessionID,
	}
}

func (c *Client) currentPeer() *v1.Peer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peer
}

func (c *Client) log() *slog.Logger {
	if c.opts.Log == nil {
		return slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return c.opts.Log
}

func dialWebSocket(ctx context.Context, url string, tlsConfig *tls.Config) (*websocket.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()

	opts := &websocket.DialOptions{}
	if tlsConfig != nil {
		opts.HTTPClient = &http.Client{
			Transport: &http.Transport{TLSClientConfig: tlsConfig},
		}
	}

	ws, _, err := websocket.Dial(dialCtx, url, opts)
	if err != nil {
		return nil, fmt.Errorf("node: dial %s: %w", url, err)
	}
	return ws, nil
}
