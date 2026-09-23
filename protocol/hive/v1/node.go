package v1

import "time"

// Node API methods.
//
// The execution.* and event.* methods are shared with agent plugins: both a
// node and an agent plugin carry execution work for the coordinator, so they
// speak the same execution surface.
const (
	// MethodNodeHello is a node's first message on a new connection.
	MethodNodeHello = "node.hello"

	// MethodNodeReady is the coordinator's acceptance of a node connection.
	MethodNodeReady = "node.ready"

	// MethodNodeHeartbeat renews node liveness and the execution lease.
	MethodNodeHeartbeat = "node.heartbeat"

	// MethodNodeSync is the reconnect synchronization exchange.
	MethodNodeSync = "node.sync"
)

// NodeHello is a node's opening message.
type NodeHello struct {
	NodeID          string          `json:"nodeId"`
	Version         string          `json:"version"`
	ProtocolVersion ProtocolVersion `json:"protocolVersion"`

	// Agents lists the agent ids this node can run. The coordinator routes
	// execution work by agent, so a node has to declare what it offers.
	Agents []string `json:"agents,omitempty"`

	// LeaseSeconds is how long the node's liveness claim lasts. The node
	// renews it with heartbeats.
	//
	// Lease expiry classifies a run as interrupted. It never authorizes
	// starting a replacement execution, because the original execution may
	// still be alive and duplicating it could repeat side effects.
	LeaseSeconds int `json:"leaseSeconds"`
}

// NodeReady is the coordinator's acceptance of a node connection.
type NodeReady struct {
	// ConnectionGeneration identifies this connection for this node identity.
	// A superseded generation may not be treated as the current node link.
	ConnectionGeneration int64 `json:"connectionGeneration"`

	// Term is reserved for coordinator failover and is always 0 in v1.
	Term int64 `json:"term"`
}

// ExecutionRef identifies one execution generation of an AgentRun.
type ExecutionRef struct {
	AgentRunID      string `json:"agentRunId"`
	Generation      int64  `json:"generation"`
	NodeExecutionID string `json:"nodeExecutionId,omitempty"`
}

// NodeHeartbeat renews node liveness and the execution lease.
type NodeHeartbeat struct {
	NodeID               string         `json:"nodeId"`
	ConnectionGeneration int64          `json:"connectionGeneration"`
	Executions           []ExecutionRef `json:"executions"`
	SentAt               time.Time      `json:"sentAt"`
}

// NodeSyncRequest is the reconnect synchronization payload.
//
// Reconnect is a synchronization protocol, not merely a new connection: the
// node presents what it believes it owns so the coordinator can reconcile.
type NodeSyncRequest struct {
	NodeID               string         `json:"nodeId"`
	ConnectionGeneration int64          `json:"connectionGeneration"`
	ActiveExecutions     []ExecutionRef `json:"activeExecutions"`
	BufferedEventIDs     []string       `json:"bufferedEventIds"`
}

// NodeSyncResponse tells the node what the coordinator still needs.
type NodeSyncResponse struct {
	// UnknownExecutions are executions the coordinator has no record of.
	UnknownExecutions []ExecutionRef `json:"unknownExecutions"`

	// ObsoleteExecutions are executions the coordinator considers finished, so
	// the node must not keep reporting them as active.
	ObsoleteExecutions []ExecutionRef `json:"obsoleteExecutions"`

	// RequestedEventIDs are buffered events the coordinator has not persisted
	// yet. Replaying one must not create a second durable event.
	RequestedEventIDs []string `json:"requestedEventIds"`
}
