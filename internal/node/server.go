package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	v1 "github.com/thupham/hive/protocol/hive/v1"
)

// HandshakeTimeout bounds how long a node has to say hello.
const HandshakeTimeout = 10 * time.Second

// DefaultLeaseSeconds is the node liveness claim used when a node does not
// specify one.
const DefaultLeaseSeconds = 30

// Handler serves a node-invoked method on the coordinator.
type Handler func(ctx context.Context, call Call) (any, error)

// Call is a method a node invoked.
type Call struct {
	Node   *Node
	Method string
	Params json.RawMessage
}

// ExecutionStore answers what the coordinator knows about executions and
// events, which is what reconnect reconciliation needs.
type ExecutionStore interface {
	// LiveExecutions returns, per AgentRun, the execution generation the
	// coordinator still considers live for a node.
	LiveExecutions(ctx context.Context, nodeID string) (map[string]int64, error)

	// HasEvent reports whether an event id is already durable.
	HasEvent(ctx context.Context, eventID string) (bool, error)
}

// Node is one connected node.
type Node struct {
	NodeID               string
	Version              string
	Agents               []string
	ConnectionGeneration int64
	LeaseDuration        time.Duration
	ConnectedAt          time.Time

	mu         sync.Mutex
	executions map[string]v1.ExecutionRef
	lastSeen   time.Time
	connected  bool

	peer *v1.Peer
}

// Agents returns the agent ids the node declared it can run.
func (n *Node) AgentIDs() []string {
	out := make([]string, len(n.Agents))
	copy(out, n.Agents)
	return out
}

// RunsAgent reports whether the node declared it can run agentID.
func (n *Node) RunsAgent(agentID string) bool {
	for _, id := range n.Agents {
		if id == agentID {
			return true
		}
	}
	return false
}

// Executions returns the executions the node last reported, in a stable order.
func (n *Node) Executions() []v1.ExecutionRef {
	n.mu.Lock()
	defer n.mu.Unlock()

	out := make([]v1.ExecutionRef, 0, len(n.executions))
	for _, ref := range n.executions {
		out = append(out, ref)
	}
	return out
}

// LastSeen returns when the node last renewed its lease.
func (n *Node) LastSeen() time.Time {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lastSeen
}

// Connected reports whether the link is up.
func (n *Node) Connected() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.connected
}

// LeaseExpired reports whether the node's lease has expired at now.
func (n *Node) LeaseExpired(now time.Time) bool {
	if !n.Connected() {
		return true
	}
	return now.Sub(n.LastSeen()) > n.LeaseDuration
}

// Call invokes a method on the node.
func (n *Node) Call(ctx context.Context, method string, params, result any) error {
	if !n.Connected() {
		return v1.Unavailable("node %s is not connected", n.NodeID)
	}
	return n.peer.Call(ctx, method, params, result)
}

func (n *Node) renew(executions []v1.ExecutionRef) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.lastSeen = time.Now().UTC()
	n.executions = make(map[string]v1.ExecutionRef, len(executions))
	for _, ref := range executions {
		n.executions[ref.AgentRunID] = ref
	}
}

func (n *Node) markDisconnected() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.connected = false
}

// Server accepts node connections on the coordinator.
type Server struct {
	log  *slog.Logger
	base context.Context

	// Store is optional. Without it, reconnect reconciliation reports nothing
	// missing rather than guessing.
	Store ExecutionStore

	mu       sync.Mutex
	nodes    map[string]*Node
	gens     map[string]int64
	handlers map[string]Handler
}

// NewServer returns a coordinator-side node server. The context bounds every
// node connection.
func NewServer(ctx context.Context, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &Server{
		log:      log,
		base:     ctx,
		nodes:    make(map[string]*Node),
		gens:     make(map[string]int64),
		handlers: make(map[string]Handler),
	}
}

// Handle registers the handler for a node-invoked method.
func (s *Server) Handle(method string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

// Node returns the current connection for a node identity.
func (s *Server) Node(nodeID string) (*Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[nodeID]
	return n, ok
}

// Nodes returns every node the coordinator has heard from, current or not.
func (s *Server) Nodes() []*Node {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, n)
	}
	return out
}

// ServeHTTP upgrades a request to a node link.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.log.Warn("node websocket upgrade failed", "error", err)
		return
	}
	go s.serve(ws)
}

func (s *Server) serve(ws *websocket.Conn) {
	ctx := s.base
	conn := websocket.NetConn(context.WithoutCancel(ctx), ws, websocket.MessageText)
	peer := v1.NewPeer(v1.NewStream(conn, conn, conn))
	peer.Start()

	node, err := s.handshake(ctx, peer)
	if err != nil {
		s.log.Warn("node handshake failed", "error", err)
		_ = peer.Close()
		return
	}

	defer func() {
		node.markDisconnected()
		_ = peer.Close()
		s.log.Info("node disconnected", "node", node.NodeID, "generation", node.ConnectionGeneration)
	}()

	s.log.Info("node connected",
		"node", node.NodeID,
		"version", node.Version,
		"generation", node.ConnectionGeneration,
	)

	s.serveRequests(ctx, node, peer)
}

func (s *Server) handshake(ctx context.Context, peer *v1.Peer) (*Node, error) {
	ctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()

	select {
	case <-ctx.Done():
		return nil, errors.New("node: handshake timed out")
	case <-peer.Done():
		return nil, errors.New("node: connection closed during handshake")
	case req, ok := <-peer.Requests():
		if !ok {
			return nil, errors.New("node: connection closed during handshake")
		}
		if req.Method != v1.MethodNodeHello {
			return nil, fmt.Errorf("node: expected %s, got %q", v1.MethodNodeHello, req.Method)
		}

		var hello v1.NodeHello
		if err := json.Unmarshal(req.Params, &hello); err != nil {
			return nil, fmt.Errorf("node: invalid hello: %w", err)
		}
		switch {
		case hello.NodeID == "":
			return nil, errors.New("node: hello is missing a node id")
		case !hello.ProtocolVersion.Compatible():
			return nil, fmt.Errorf("node: %s: incompatible protocol version %s", hello.NodeID, hello.ProtocolVersion)
		}

		lease := time.Duration(hello.LeaseSeconds) * time.Second
		if lease <= 0 {
			lease = DefaultLeaseSeconds * time.Second
		}

		s.mu.Lock()
		s.gens[hello.NodeID]++
		generation := s.gens[hello.NodeID]
		s.mu.Unlock()

		now := time.Now().UTC()
		node := &Node{
			NodeID:               hello.NodeID,
			Version:              hello.Version,
			Agents:               hello.Agents,
			ConnectionGeneration: generation,
			LeaseDuration:        lease,
			ConnectedAt:          now,
			executions:           make(map[string]v1.ExecutionRef),
			lastSeen:             now,
			connected:            true,
			peer:                 peer,
		}

		if err := peer.Respond(req.RequestID(), v1.NodeReady{ConnectionGeneration: generation}); err != nil {
			return nil, fmt.Errorf("node: respond ready: %w", err)
		}

		s.mu.Lock()
		previous := s.nodes[hello.NodeID]
		s.nodes[hello.NodeID] = node
		s.mu.Unlock()

		if previous != nil {
			// The old connection generation becomes invalid as soon as the new
			// connection is authenticated, so a delayed packet from it cannot
			// be treated as a current node link.
			previous.markDisconnected()
			_ = previous.peer.Close()
		}
		return node, nil
	}
}

func (s *Server) serveRequests(ctx context.Context, node *Node, peer *v1.Peer) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-peer.Done():
			return
		case req, ok := <-peer.Requests():
			if !ok {
				return
			}
			s.handleRequest(ctx, node, req)
		case _, ok := <-peer.Notifications():
			if !ok {
				return
			}
		}
	}
}

func (s *Server) handleRequest(ctx context.Context, node *Node, req *v1.Message) {
	id := req.RequestID()

	if !s.isCurrent(node) {
		_ = node.peer.RespondError(id, v1.Unauthorized(
			"node connection generation %d is superseded", node.ConnectionGeneration))
		return
	}

	switch req.Method {
	case v1.MethodNodeHeartbeat:
		var hb v1.NodeHeartbeat
		if err := json.Unmarshal(req.Params, &hb); err != nil {
			_ = node.peer.RespondError(id, v1.InvalidParams("invalid heartbeat"))
			return
		}
		node.renew(hb.Executions)
		_ = node.peer.Respond(id, map[string]any{"ok": true})
		return

	case v1.MethodNodeSync:
		var syncReq v1.NodeSyncRequest
		if err := json.Unmarshal(req.Params, &syncReq); err != nil {
			_ = node.peer.RespondError(id, v1.InvalidParams("invalid sync request"))
			return
		}
		node.renew(syncReq.ActiveExecutions)
		_ = node.peer.Respond(id, s.reconcile(ctx, node, syncReq))
		return
	}

	s.mu.Lock()
	h := s.handlers[req.Method]
	s.mu.Unlock()

	if h == nil {
		_ = node.peer.RespondError(id, v1.MethodNotFound(req.Method))
		return
	}

	result, err := h(ctx, Call{Node: node, Method: req.Method, Params: req.Params})
	if err != nil {
		_ = node.peer.RespondError(id, v1.AsError(err))
		return
	}
	_ = node.peer.Respond(id, result)
}

// reconcile tells a reconnecting node what the coordinator still needs.
//
// Synchronization must be idempotent: replaying a buffered event or re-reporting
// an execution must not create a second durable event or a second execution.
func (s *Server) reconcile(ctx context.Context, node *Node, req v1.NodeSyncRequest) v1.NodeSyncResponse {
	var resp v1.NodeSyncResponse
	if s.Store == nil {
		return resp
	}

	live, err := s.Store.LiveExecutions(ctx, node.NodeID)
	if err != nil {
		s.log.Warn("node sync: could not read live executions", "node", node.NodeID, "error", err)
		return resp
	}

	for _, ref := range req.ActiveExecutions {
		generation, known := live[ref.AgentRunID]
		switch {
		case !known:
			resp.UnknownExecutions = append(resp.UnknownExecutions, ref)
		case generation != ref.Generation:
			// The node is running a superseded generation.
			resp.ObsoleteExecutions = append(resp.ObsoleteExecutions, ref)
		}
	}

	for _, id := range req.BufferedEventIDs {
		durable, err := s.Store.HasEvent(ctx, id)
		if err != nil {
			s.log.Warn("node sync: could not check event", "node", node.NodeID, "event", id, "error", err)
			continue
		}
		if !durable {
			resp.RequestedEventIDs = append(resp.RequestedEventIDs, id)
		}
	}
	return resp
}

func (s *Server) isCurrent(node *Node) bool {
	s.mu.Lock()
	current, ok := s.nodes[node.NodeID]
	s.mu.Unlock()
	return ok && current == node && node.Connected()
}

// WatchLeases calls onExpired for every execution whose node lease has expired.
//
// Lease expiry classifies a run as interrupted. It never authorizes a
// replacement execution: the original execution may still be alive, and
// duplicating it could repeat side effects. The callback must therefore be
// idempotent and must not start work.
func (s *Server) WatchLeases(ctx context.Context, interval time.Duration, onExpired func(nodeID string, ref v1.ExecutionRef)) error {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			now := time.Now().UTC()
			for _, node := range s.Nodes() {
				if !node.LeaseExpired(now) {
					continue
				}
				for _, ref := range node.Executions() {
					onExpired(node.NodeID, ref)
				}
			}
		}
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
