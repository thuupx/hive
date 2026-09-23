# Hive Solution Design

## 1. Purpose

Hive is a self-hosted personal agent gateway that connects any client or
platform to any agent runtime across multiple machines.

The design intentionally preserves the full target architecture:
multi-node execution, coordinator failover, a public Hive protocol,
process-isolated plugins, agent-runtime abstraction, ACP integration,
session-centric orchestration, cross-agent handoff, permission relay,
workspace abstraction, event sourcing, remote networking, discovery,
multiple transports, TUI, optional Web UI, concurrent agent runs, remote
agents, Go/libSQL, and an extensible open-source ecosystem.

The goal is not to implement every feature at once. The goal is to
define stable boundaries now so that the implementation can grow into
the complete architecture without forcing a fundamental rewrite.

The core principle is:

> Hive owns orchestration and context. Agents own execution and tool
> semantics. Transports own presentation and delivery. Plugins own
> integration boundaries.

------------------------------------------------------------------------

## 2. Design Principles

### 2.1 Separation of concerns

The following boundaries are architectural invariants:

``` text
Transport != Hive Session
Hive Session != Agent Session
ACP != Hive Protocol
Workspace != Git Manager
Node != Session Owner
Agent Runtime != Agent Implementation
Plugin != Core
Control Plane != Execution Plane
Realtime Delivery != Durable Storage
```

### 2.2 Open-source first

The public interfaces must be understandable without reading the
implementation. Protocol schemas, lifecycle semantics, security
assumptions, compatibility rules, and failure behavior are part of the
product.

Avoid undocumented behavior that consumers must reverse-engineer.

### 2.3 Extensible, not speculative

Hive should expose abstractions where there is a real boundary, while
keeping implementations simple. Future capabilities may be represented
by interfaces and protocol extension points, but a future feature must
not force unnecessary infrastructure into the first implementation.

### 2.4 Failure containment

Failure of a transport, plugin, agent process, node connection, or
coordinator should not unnecessarily terminate unrelated work.

### 2.5 At-least-once delivery

Hive uses at-least-once delivery for asynchronous events and commands
where applicable. Consumers must be idempotent. Exactly-once delivery is
not a system-wide guarantee.

### 2.6 Unknown data must survive

When Hive receives protocol-specific data it does not understand, it
should preserve it as raw data whenever safe to do so rather than
dropping it or requiring an immediate schema migration.

------------------------------------------------------------------------

# 3. System Architecture

``` text
                         External World
                              |
             +----------------+----------------+
             |                |                |
           Slack           Telegram         Custom
         Transport         Transport        Client
             |                |                |
             +----------------+----------------+
                              |
                       Hive Control API
                              |
                    +---------v---------+
                    | Hive Coordinator  |
                    |                   |
                    | Gateway           |
                    | Security          |
                    | Session Manager   |
                    | Event Bus         |
                    | Event Store       |
                    | Node Router       |
                    | Plugin Supervisor |
                    +---------+---------+
                              |
                       Hive Node Protocol
                              |
             +----------------+----------------+
             |                |                |
          Node A           Node B           Node C
             |                |                |
         ACP Runtime      ACP Runtime      Remote Runtime
             |                |                |
          Claude           Devin             Future
```

A single machine can run the complete stack:

``` text
Hive
 ├── Coordinator
 ├── Node
 └── Agent Processes
```

A multi-machine deployment separates the control plane from execution:

``` text
                 Coordinator
                /     |     \
             Node A Node B Node C
```

The same `hive` binary is used on all machines. The configured cluster
role determines whether the process acts as coordinator, node, or
auto-detects its role.

------------------------------------------------------------------------

# 4. Coordinator and Node

## 4.1 Roles

The supported logical roles are:

``` text
coordinator
node
auto
```

`auto` is the recommended default.

The coordinator is the control-plane authority. Nodes are
execution-plane participants.

### Coordinator responsibilities

The coordinator owns:

``` text
node registry
session registry
routing
event history
session snapshots
handoff metadata
plugin registry
external client bindings
security policy
```

### Node responsibilities

A node owns:

``` text
agent processes
local runtime state
local event buffer
local plugin processes
local execution lifecycle
```

The node must be capable of keeping its local agent processes alive if
the coordinator connection disappears.

## 4.2 Coordinator failure

Coordinator failure must not implicitly terminate agent processes.

When disconnected:

``` text
Coordinator
     X
     |
     +---- Node A ---- Agent
     +---- Node B ---- Agent
```

Agents continue their current execution where possible. New
coordinator-dependent operations may be unavailable until connectivity
returns.

Nodes buffer relevant events locally and reconnect when the coordinator
becomes available.

## 4.3 Session ownership

A session is not owned by a node.

The canonical relationship is:

``` text
Session
  |
  +-- AgentRun
       +-- Agent
       +-- Node
```

An `AgentRun` is the execution assignment connecting a Hive session to a
particular agent runtime on a node.

This distinction allows future reassignment or recovery without making
the session itself node-local.

------------------------------------------------------------------------

# 5. Coordinator Failover

Hive may support best-effort coordinator failover as a future deployment
capability. The v1 architecture does not require coordinator failover.

The v1 posture is:

``` text
coordinator is single-authority
coordinator restart is the recovery mechanism
nodes may keep running agents while the coordinator is unavailable
node execution state is reconcilable
```

The failover design below is retained as the target architecture for that
future capability, not as a v1 requirement.

The failover design deliberately avoids Raft, etcd, or another external
consensus service because the intended deployment is a small personal
cluster, typically two to three machines.

The mechanism is:

``` text
heartbeat
lease
priority
term
```

Each coordinator-capable node has a configured priority:

``` toml
[cluster]
priority = 100
```

For example:

``` text
Mac mini   100
Desktop     50
MacBook     10
```

If the active coordinator loses its lease, an eligible node may promote
itself according to the election rules.

## 5.1 Important guarantee

Hive failover is explicitly:

> Best-effort failover, not strong HA or consensus.

The implementation must not claim linearizable distributed state.

A partition can temporarily produce competing coordinator candidates.
Terms and persisted event metadata are used to converge after
connectivity is restored, but they do not magically eliminate every
distributed-systems ambiguity.

Therefore:

- side effects must be idempotent where possible;
- commands must carry unique IDs;
- events must carry origin metadata;
- stale coordinators must relinquish authority when a higher valid
  term is observed;
- reconciliation must be deterministic;
- operations that cannot safely be reconciled must fail closed or
  require retry.

The design accepts this trade-off because Hive targets a small personal
deployment rather than a general-purpose consensus cluster.

## 5.2 Coordinator authority and terms

A coordinator term identifies the current control-plane generation.

A term is represented as an ordered pair:

``` text
(epoch, coordinator_id)
```

`epoch` is the numeric authority generation and `coordinator_id` is a
stable tie-breaker. Terms compare lexicographically, so two candidates
that promote concurrently cannot create the same term even when they
were partitioned. A node may still observe competing valid candidates;
term ordering provides deterministic fencing once the candidates meet.

A coordinator may perform authoritative control-plane writes only while
it holds the currently valid lease for the highest known term.

The following rules are required:

1. A candidate must observe the current cluster term before promotion.
2. A successful promotion creates a strictly higher term than the
   candidate has previously observed.
3. Every authoritative command and coordinator-originated event carries
   the coordinator term.
4. Each coordinator-to-node connection also has an authenticated
   connection generation. A node accepts commands only on the currently
   valid connection for that coordinator identity and generation.
5. A coordinator that observes a higher valid term must immediately enter
   a non-authoritative state and stop accepting new authoritative writes.
6. Nodes reject authoritative commands from a coordinator term lower
   than the highest valid term they have accepted, and reject commands
   arriving on a superseded coordinator connection generation.
7. A command that was accepted under an older term is not automatically
   replayed after failover. It must be reconciled using its command ID,
   operation state, and target execution generation.
8. Term changes and connection-generation changes are durable cluster
   metadata, not process-local memory.

This is a fencing mechanism, not consensus. It prevents an old
coordinator from continuing ordinary writes after it has learned that a
newer coordinator generation exists.

## 5.3 Coordinator state and failover

Coordinator-owned state has a single logical authority at a time, but
the design does not assume synchronous database replication between
coordinator-capable machines.

Before enabling failover for a deployment, the implementation must
define how the candidate obtains the latest durable coordinator state.
Node-to-coordinator event replay alone is not sufficient because nodes do
not own arbitrary coordinator state such as transport bindings,
administrative policy, or session metadata.

The baseline failover design therefore includes a lightweight,
best-effort replication path for coordinator-owned durable state:

``` text
Active Coordinator
      |
      +--> replicated control snapshot/journal --> standby coordinator(s)
      |
      +--> nodes provide execution evidence + buffered events
```

The replicated control state may lag the active coordinator. The
baseline replication unit is a durable control snapshot plus an
append-only control journal after that snapshot. Each journal entry has a
monotonic control sequence, the originating term, and the domain mutation
already committed by the active coordinator.

A standby records the highest control sequence it has durably received.
Promotion uses the latest complete snapshot plus journal prefix and
records that recovered boundary explicitly. Writes outside that boundary
are not assumed to exist. Node execution evidence/events are then used to
reconcile in-flight operations that may have crossed the boundary.

A future storage replication mechanism may replace or strengthen this
path without changing the domain model.

A promoted coordinator must not silently assume that its local database
is complete. It starts in a reconciliation phase:

``` text
candidate
   |
   v
observe term
   |
   v
promote
   |
   v
reconcile known durable state
   |
   +--> safe to serve
   |
   +--> incomplete/ambiguous -> fail closed for affected operations
```

Operations whose state cannot be established safely must return a retry
or unavailable result rather than inventing state.

## 5.4 Failure model

The system explicitly supports:

``` text
process crash
node disconnect
coordinator disconnect
temporary network loss
network partition
duplicate command
duplicate event
reconnect
stale coordinator
```

It does not guarantee correct concurrent writes from two partitioned
coordinators. Such periods are treated as degraded operation, not as
strongly consistent multi-primary operation.

------------------------------------------------------------------------

# 6. Hive Protocol

Hive Protocol is the public application protocol of Hive. ACP is not the
Hive protocol.

The base wire format is:

``` text
JSON-RPC 2.0
```

Reasons:

-   language agnostic;
-   request/response correlation;
-   notifications;
-   bidirectional communication;
-   easy inspection and debugging;
-   convenient interoperability with ACP.

Hive protocol messages must carry enough authenticated context for the
receiver to distinguish the actor and trust domain of the message:

``` text
actor/principal
protocol version
capability/session context when applicable
```

Authentication is transport-specific, but authorization is a Hive
domain concern and is evaluated before a mutating operation is accepted.

The protocol is transport-independent.

Possible transports include:

``` text
Plugin              JSON-RPC over stdio
Node                JSON-RPC over WebSocket/TLS
Local UI            Unix socket
Remote client       WebSocket/TLS
```

The protocol should be logically divided into API domains even when
transported over one JSON-RPC connection:

``` text
Control API
  session.*
  agent.*
  node.*
  workspace.*
  cluster.*
  command.*

Node API
  node.*
  execution.*
  event.*

Plugin API
  plugin.*
  capability.*
  lifecycle.*
```

Protocol versioning is explicit. At minimum, Hive must distinguish
protocol family/version from individual message or event versions.

The implementation should prefer additive evolution and capability
negotiation over breaking changes.

------------------------------------------------------------------------

# 7. Commands, Requests, and Events

Hive has three related but distinct concepts.

## 7.1 Commands

A command requests an operation:

``` text
session.create
session.resume
session.prompt
session.cancel
session.handoff
session.list

command.get

agent.list
node.list
workspace.list
```

Every mutating command has a unique `command_id`. The command ID is
stable across retries of the same logical operation.

Transport-specific syntax is not part of Hive Protocol semantics.

### 7.1.1 Command lifecycle

A mutating command is tracked through an explicit lifecycle:

``` text
received
   |
   v
accepted
   |
   +--> completed
   +--> rejected
   +--> failed
   +--> retryable
```

Every mutating command has a durable command record containing at least:

``` text
command_id
actor/principal
source_id (when supplied by transport)
method
accepted_term
target (when applicable)
state
result/error
created_at
updated_at
```

For asynchronous operations, the command record is the operation status
resource. Clients can query it explicitly through a command-status
operation rather than depending on a single request/response being alive
until completion.

A retry with the same `command_id` must return the existing operation
result when that result is known. It must not create a second session,
AgentRun, handoff, or permission transition.

Command deduplication is required at the authoritative domain boundary,
not only in individual transports.

### 7.1.2 Command transaction boundary

For coordinator-owned mutations, command acceptance, command identity,
and the authoritative domain mutation must be committed atomically in
the coordinator database transaction whenever the mutation is local to
the coordinator.

The baseline pattern is:

``` text
receive command
      |
      v
validate + authorize
      |
      v
deduplicate by command_id
      |
      v
single durable transaction
  +-- command state
  +-- domain mutation
  +-- authoritative domain event/outbox record
  +-- control journal entry when standby replication is enabled
      |
      v
commit
```

The event bus is not part of this database transaction. Realtime
publication may happen after commit and is therefore at-least-once.

A command may remain `accepted` or `retryable` when execution requires a
node-side operation that has not yet completed. The command record must
still make the logical operation identity durable before the node is
asked to perform the side effect.

This transaction boundary does not imply distributed atomicity between
the coordinator database and a node process. Node-side operations use
stable command IDs and execution generations and are reconciled after
failure or reconnect.


For example:

``` text
Slack:      @Hive new
CLI:        hive session create
Web:        New Session
Custom bot: /new
```

All may map to the same Hive operation.

## 7.2 Events

Events describe something that happened.

An event envelope is:

``` text
event_id
sequence
timestamp
session_id
run_id
source
type
version
protocol
method
payload
```

`payload` is represented as structured JSON/raw JSON depending on the
event type.

`event_id` provides identity.

`sequence` provides ordering within an event stream. The baseline stream
scope is the Hive Session. A session stream contains all durable events
for that session, regardless of which node or AgentRun produced them.

The sequence is assigned by the authoritative event store when an event
becomes durable. It is strictly monotonic within that session stream.
Node-local temporary sequence numbers are not authoritative Hive
sequences.

Session sequence is durable stream ordering only. It is not a causal
ordering mechanism between AgentRuns, nodes, or external side effects.
A higher sequence does not imply that the later event observed, caused,
or depends on the earlier event unless the domain relationship records
that explicitly.

UUIDv7 may be used for globally unique IDs, but UUID ordering must not
be used as the authoritative event-stream cursor.

Events also retain origin metadata so that events produced while a node
is disconnected can be reconciled without losing their original source.

## 7.3 Raw protocol data

Protocol-specific information that Hive does not semantically understand
is preserved through a raw event form:

``` json
{
  "type": "agent.raw",
  "version": 1,
  "protocol": "acp",
  "method": "future/thing",
  "payload": {}
}
```

Hive should not create a new core event type merely because an agent
protocol adds a new feature.

------------------------------------------------------------------------

# 8. Event Architecture

Events are the backbone of Hive.

``` text
Agent / User / Node / Transport
              |
              v
       Canonical Event
              |
        +-----+------+
        |            |
        v            v
     Event Bus    Event Store
        |
   +----+----+----+
   |         |    |
 Slack      Web Telegram
```

The realtime path and durability path are separate:

``` text
realtime path != durability path
```

An agent event is published to the event bus first. Subscribers can
render or process it immediately. Persistence happens asynchronously.

The system uses bounded queues to prevent a slow transport from blocking
the agent execution path.

## 8.1 Durability semantics

An event becomes durable only after the event store successfully
persists it.

Live delivery may occur before durable persistence.

This distinction is intentional and documented.

For reconnect and replay semantics, only durable session-stream
sequences are authoritative. A live event that was delivered before
persistence may be delivered again after reconnect; consumers must use
`event_id` and/or the durable sequence to deduplicate it.

### 8.1.1 Node event buffering

When a node cannot reach the coordinator, it may buffer events locally.

Each buffered event retains:

``` text
event_id
origin_node
origin_timestamp
session_id
run_id
event payload
local_buffer_sequence
```

`local_buffer_sequence` is only a node-local synchronization aid. It is
not a Hive event-stream sequence.

When the node reconnects, it uploads buffered events using their stable
event IDs. The coordinator persists each event at most once by event ID
and assigns the authoritative session sequence only when the event is
accepted into the durable event store.

If an event conflicts with an already durable event with the same event
ID, the coordinator treats the event as a duplicate rather than creating
a second event.

## 8.2 Delivery semantics

Hive provides:

``` text
at-least-once delivery
+
idempotent consumers
```

Consumers must use event IDs and/or stream sequences to avoid duplicate
side effects.

## 8.3 Reconnection

Clients maintain a stream cursor.

``` text
client
  |
  | last_sequence = 102
  v
Coordinator
  |
  +-- replay 103...
```

A sequence is scoped to the appropriate event stream and must be
monotonic within that stream.

A replay request has explicit semantics for retention gaps. When a
requested sequence has already been pruned, Hive returns a
`cursor_expired` condition together with the latest available
snapshot/context boundary and the next valid event sequence. The client
must then rehydrate from that snapshot before continuing the stream.

The protocol does not claim that a historical cursor remains valid
forever.

------------------------------------------------------------------------

# 9. Event Types

Hive should normalize only semantics it actually owns.

Initial core event families are:

``` text
message
status
run.started
run.finished
permission.requested
permission.responded
handoff.created
error
agent.raw
```

Agent-specific streaming updates should not all be collapsed into a
generic `agent_update` structure.

For example, text deltas, tool lifecycle, resource updates, and
protocol-specific notifications can remain protocol-native/raw unless
Hive needs to act on them.

This prevents schema explosion while preserving useful information.

------------------------------------------------------------------------

# 10. Session Model

The Hive Session is the central domain abstraction.

``` text
Hive Session
 |
 +-- metadata
 +-- context snapshot
 +-- events
 +-- transport bindings
 |
 +-- AgentRun #1
 |    +-- Claude @ Node A
 |
 +-- AgentRun #2
 |    +-- Devin @ Node B
 |
 +-- AgentRun #3
      +-- Codex @ Node C
```

A session can contain multiple agent runs.

Hive does not define:

``` text
1 Hive Session = 1 Agent Session
```

That would unnecessarily inherit limitations from an underlying agent
runtime.

------------------------------------------------------------------------

# 11. Session State

Session state and AgentRun state are distinct.

Suggested session states:

``` text
active
idle
handoff
archived
```

Suggested AgentRun states:

``` text
created
queued
starting
running
completed
cancelled
failed
interrupted
```

State transitions must be validated by the domain layer rather than
represented as arbitrary strings.

A completed AgentRun does not imply that the Hive Session is completed.

## 11.1 Prompt routing

`session.prompt` targets an AgentRun rather than implicitly creating a new
runtime session on every message.

The preferred v1 rule is:

``` text
session.prompt
     |
     +-- explicit run_id
     |       |
     |       +--> target that AgentRun
     |
     +-- no run_id
             |
             +--> Session routing policy
                     |
                     +--> current/default interactive AgentRun
                     +--> create a new AgentRun when policy requires it
                     +--> reject as ambiguous when no safe target exists
```

A Session with one active interactive AgentRun should normally route a
prompt to that run. Creating a new AgentRun is an explicit routing
decision, not an accidental side effect of receiving a prompt.

The selected target must be fixed when the command is accepted. A retry
of the same `command_id` must resolve to the same logical target rather
than selecting a different AgentRun.

## 11.2 AgentRun ownership and recovery

The coordinator owns the authoritative AgentRun state machine. The node
owns the live execution instance.

A running AgentRun therefore has two related states:

``` text
Hive AgentRun state
        +
Node execution state
```

The node reports lifecycle transitions to the coordinator. The
coordinator persists the authoritative state transition.

If a node disconnects, the coordinator must not immediately assume that
the agent process has stopped. The run enters a connectivity-uncertain
condition until the node reconnects or a configured execution lease
expires.

If the node process itself confirms that the agent has terminated, the
coordinator can transition the AgentRun to `completed`, `failed`, or
`interrupted` according to the reported reason.

If the node is permanently lost and the execution lease expires, the
coordinator transitions the run to `interrupted` unless it has
sufficient evidence of a terminal state.

Hive must not automatically start a replacement AgentRun merely because
the original node disappeared. Automatic retry could duplicate
side-effects performed by the original agent. Recovery creates a new
AgentRun only through an explicit retry/resume policy that is safe for
the selected runtime.

## 11.3 Execution fencing

Commands sent to a node for an AgentRun carry the current AgentRun
execution generation.

A node rejects commands for an older generation. This prevents a late
command from a previous coordinator connection from being applied to a
new execution instance after recovery.

The execution generation identifies the logical execution instance owned
by the current AgentRun state. A new generation is created only by an
explicit recovery, retry, or resume decision; lease expiry alone does not
start a replacement execution.

The execution lease is a liveness signal for the current generation. The
node renews it while that generation is known to be alive. Lease expiry
allows the coordinator to classify the run as interrupted when terminal
evidence is unavailable, but does not authorize automatic replacement.

``` text
lease expiry
    |
    v
AgentRun = interrupted
    |
    X
no automatic new execution
```

An explicit retry/resume may create a new execution generation. Before
doing so, Hive must reconcile the old generation sufficiently to avoid
knowingly creating two active executions for the same AgentRun.

## 11.4 AgentRun start and recovery reconciliation

Starting an AgentRun is a coordinator-to-node side effect and therefore
cannot be made atomically identical to the coordinator database
transaction.

The coordinator first durably records the logical operation using its
`command_id` and intended AgentRun execution generation. The node then
performs the side effect idempotently.

If the coordinator loses the result of a start operation, the node must
be able to report the known state of that AgentRun generation during
reconnect or reconciliation:

``` text
absent
starting
running
terminal
ambiguous
```

A retry of the same start command must return or reconcile the existing
operation rather than spawn a second execution.

If node state is ambiguous, Hive must not start another execution merely
to "make sure it is running". The operation becomes retryable or
unavailable until the ambiguity can be resolved safely.

The minimum correlation data for reconciliation is:

``` text
agent_run_id
execution_generation
command_id
node execution identity
```

------------------------------------------------------------------------

# 12. Agent Runtime Abstraction

Hive defines a small Agent Runtime interface.

Conceptually:

``` go
type AgentRuntime interface {
    Start(ctx context.Context) error
    Stop(ctx context.Context) error

    CreateSession(ctx context.Context, req SessionRequest) (AgentSession, error)
    ResumeSession(ctx context.Context, req ResumeRequest) (AgentSession, error)

    Prompt(ctx context.Context, session AgentSession, prompt Prompt) error
    Cancel(ctx context.Context, session AgentSession) error

    RespondToRequest(ctx context.Context, req RequestResponse) error
}
```

The interface must remain small.

Runtime implementations must additionally expose enough
execution-identity/state for the node execution controller to reconcile a
lost start acknowledgement. This may be an optional runtime capability
rather than another mandatory high-level method.

The node execution controller must be able to correlate:

``` text
Hive AgentRun
execution_generation
node execution identity
runtime session identity
start command_id
```

It must not infer successful execution merely from a local `start sent`
flag.

The interface must not contain:

``` text
RunShell
WriteFile
GitCheckout
InstallPackage
BrowserAutomation
```

Those belong to the underlying agent runtime.

------------------------------------------------------------------------

# 13. ACP Integration

ACP is an implementation of the Hive Agent Runtime abstraction.

``` text
Hive
 |
 +-- Agent Runtime
      |
      +-- ACP
      +-- Future protocol
      +-- Remote agent
```

The ACP integration is generic.

Do not create:

``` text
hive-plugin-devin
hive-plugin-claude
hive-plugin-codex
```

Instead:

``` text
hive-plugin-acp
```

Agent identity is configuration:

``` toml
[agents.devin]
protocol = "acp"
command = ["devin", "acp"]

[agents.claude]
protocol = "acp"
command = ["claude", "..."]

[agents.codex]
protocol = "acp"
command = ["codex", "..."]
```

A future remote target can use the same agent configuration model:

``` toml
[agents.remote-devin]
protocol = "acp"
endpoint = "wss://..."
```

The Hive core must not contain vendor-specific knowledge.

------------------------------------------------------------------------

# 14. Same-Agent Restore

Same-agent restore and cross-agent handoff are different operations.

When the underlying runtime supports session restore:

``` text
Hive Session
    |
    v
same Agent Runtime
    |
    v
runtime session/load
```

When native restore is unavailable:

``` text
Hive Session
    |
    +-- Hive Context
    |
    v
new Agent Runtime Session
```

Hive must not assume that every agent supports native session
persistence.

------------------------------------------------------------------------

# 15. Cross-Agent Handoff

Cross-agent handoff is a first-class Hive capability.

Example:

``` text
Session #42

Claude
  |
  | architecture completed
  v
Handoff
  |
  v
Devin
  |
  | implementation
```

A handoff creates a new AgentRun rather than migrating an existing agent
process.

``` text
AgentRun #1 -> Claude
AgentRun #2 -> Devin
```

Hive must not attempt to convert a native Claude session into a native
Devin session.

Instead it builds a context package.

``` text
Source Agent
    |
    +-- optional summary
    |
    v
Hive Handoff Context
    |
    +-- session snapshot
    +-- relevant recent events
    +-- prior messages
    +-- workspace reference
    +-- artifacts/resources
    +-- optional agent summary
    |
    v
Target Agent
```

The handoff summary is optional. Handoff must remain valid when the
source agent cannot generate a summary.

## 15.1 Handoff lifecycle

A handoff is itself a durable operation with an explicit lifecycle:

``` text
requested
   |
   v
context_building
   |
   v
target_starting
   |
   +--> active
   +--> failed
   +--> cancelled
```

Creating the handoff record and assigning its target AgentRun must be
idempotent by `command_id`.

The source AgentRun is not considered successfully handed off merely
because the target AgentRun was created. The handoff reaches a usable
state only when the target execution has started and the required
context has been accepted.

If a failure occurs between source termination and target startup, Hive
records the handoff as incomplete/failed and does not silently claim
that the session has been transferred.

The source run's native runtime session ID and the target run's native
runtime session ID are always distinct fields.

------------------------------------------------------------------------

# 16. Handoff Context

The first implementation should keep handoff context explicit and
deterministic.

Conceptually:

``` go
type HandoffContext struct {
    Summary      string
    RecentEvents []Event
    Workspace    WorkspaceRef
    Artifacts    []ResourceRef
}
```

A future context builder may add:

``` text
token budgeting
relevance selection
summarization
compression
artifact resolution
```

Those are extensions, not prerequisites for the initial handoff
implementation.

------------------------------------------------------------------------

# 17. Permission Relay

Hive does not implement an agent tool-permission engine.

The underlying agent owns tool authorization semantics.

The flow is:

``` text
Agent
  |
  v
Agent Protocol
  |
  v
Hive Permission Relay
  |
  v
Canonical Event
  |
  +--> Slack
  +--> Telegram
  +--> Web
  +--> CLI
  |
  v
Permission Response
  |
  v
Hive
  |
  v
Agent Protocol
  |
  v
Agent
```

Hive treats permission requests as opaque protocol data except for the
metadata required to route, expire, authorize, and correlate the
response.

## 17.1 Permission state

A permission request is a one-time state transition:

``` text
pending
  |
  +--> approved
  +--> denied
  +--> expired
  +--> cancelled
```

Once resolved, it cannot transition to another terminal state.

Responses must be idempotent. Repeating the same response must not
produce duplicate authorization.

Concurrent responses are resolved with a single atomic terminal
transition (compare-and-set semantics on the permission request ID).
Only the first valid terminal transition wins; later responses observe
the already-resolved state.

Timeout defaults to a safe terminal state such as denied/cancelled,
depending on the underlying protocol semantics.

## 17.2 Coordinator failure during permission requests

A pending permission request is durable coordinator state.

If the coordinator disconnects while a permission request is pending:

- the node may keep the underlying agent execution blocked;
- the request remains pending until its expiration policy is reached;
- a replacement coordinator may recover the pending request from
  durable state;
- a response submitted after failover uses the same permission request
  ID and is therefore idempotent;
- if the request cannot be safely recovered, Hive must fail the request
  closed rather than authorize an unknown action.

The node must not treat loss of the coordinator connection as implicit
approval.

------------------------------------------------------------------------

# 18. Security Model

Hive security focuses on system boundaries, not agent tool semantics.

There are three primary identity classes:

``` text
User
Node
Plugin
```

## 18.1 User identity

External identities are mapped to Hive principals:

``` text
slack:U123
telegram:123
web:<client-id>
```

A principal may be authorized to:

``` text
access a session
use a transport
start an agent
access a node
request a handoff
```

Authentication establishes the principal; authorization determines what
that principal may do. A remote client transport must authenticate before
Hive accepts session-mutating commands. The deployment may use an
established mechanism such as mTLS or signed access tokens, but the
protocol must never treat an unverified, client-supplied principal
string as proof of identity.

Unknown access is denied by default.

For a personal installation, the default model is an owner principal.

Example:

``` toml
[security]
allowed_users = ["slack:U123"]
allowed_channels = ["C123"]
```

## 18.2 Node identity

Nodes use persistent public-key identity, such as Ed25519.

The coordinator records the node identity and establishes an
authenticated connection before accepting node operations.

Node authentication and node authorization are separate:

``` text
authenticated node != automatically authorized node
```

Enrollment must support an out-of-band approval/bootstrap step and must
support revocation and key rotation. A replaced key is not silently
treated as the same trusted node unless an explicit rotation procedure
proves continuity.

Every authenticated node connection receives a connection generation.
Reconnecting creates a new generation and invalidates the previous
connection for command acceptance.

## 18.3 Plugin trust

V1 plugins are trusted local processes.

Process isolation provides fault containment, not a complete security
sandbox.

The documentation must explicitly state that a trusted plugin has
powerful access to the Hive process through its plugin protocol.

Plugin trust is bound to a stable plugin identity and its registered
capabilities, not merely to a process name. A restart of the same trusted
plugin retains its identity; a different binary or identity must go
through the trust/enrollment path again according to configured policy.

A plugin may invoke only capabilities it has registered and been
authorized to use. Capability registration is descriptive and does not
turn the plugin protocol into a general-purpose policy language.

A trusted plugin identity and a plugin process instance are distinct:

``` text
plugin_id
   |
   +-- instance #1
   +-- instance #2 after restart
```

Capabilities are authorized to the stable plugin identity, while each
live instance receives a fresh authenticated process/session identity.
Stale plugin instances cannot continue invoking capabilities after their
process connection has been superseded.

A future sandboxed plugin model can be introduced later without
pretending that the first version already provides it.

------------------------------------------------------------------------

# 19. Plugin Architecture

Plugins always run as separate processes.

``` text
hive
 |
 +-- hive-plugin-acp
 +-- hive-plugin-slack
 +-- hive-plugin-telegram
 +-- hive-plugin-web
 +-- hive-plugin-tailscale
```

The core lifecycle is:

``` text
spawn
  |
handshake
  |
authenticate/trust
  |
register
  |
ready
  |
serve
  |
draining
  |
shutdown
```

If a plugin crashes:

``` text
plugin dies
   |
Hive detects failure
   |
restart according to policy
   |
re-handshake
   |
re-register
```

The plugin must not cause the Hive session manager to crash.

Do not use the Go `plugin` package.

Do not dynamically load `.so` files.

Do not require WASM for the initial plugin model.

Do not require containers.

Process isolation plus RPC is the baseline.

------------------------------------------------------------------------

# 20. Plugin Types

The architecture recognizes these extension categories:

``` text
transport
agent
network
ui
```

The initial ecosystem focuses on:

``` text
transport
agent
```

Network and UI plugin boundaries remain supported by the architecture
but are not required to be implemented before they are useful.

Core subsystems such as storage, sessions, security, and the event bus
remain core and are not plugins.

------------------------------------------------------------------------

# 21. Transport Architecture

A transport translates an external platform into Hive concepts.

It must not access session internals directly.

A transport has three inbound concerns:

``` text
1. message parsing
2. command/interaction parsing
3. platform identity + delivery correlation
```

The transport does not decide Hive business semantics. It normalizes
platform-native input into a small transport-neutral envelope.

Conceptually:

``` go
type IncomingMessage struct {
    ConversationID string
    Principal      Principal
    Text           string
    SourceID       string
    ReplyContext   ReplyContext
}

type IncomingCommand struct {
    ConversationID string
    Principal      Principal
    Name           string
    Args           []string
    SourceID       string
    ReplyContext   ReplyContext
}

type IncomingInteraction struct {
    ConversationID string
    Principal      Principal
    Action         string
    Value          string
    SourceID       string
    ReplyContext   ReplyContext
}
```

`SourceID` is the platform's stable identifier for the inbound delivery
when one exists, such as a Slack event/message ID. It is used to make
transport delivery itself idempotent.

The transport produces an inbound message, command, or interaction.

The Hive gateway then normalizes it into a Hive operation:

``` text
platform input
     |
     v
transport parser
     |
     +--> IncomingMessage
     +--> IncomingCommand
     +--> IncomingInteraction
     |
     v
Hive operation
```

For outbound communication, the transport receives Hive events and
renders them in its native UX.

Examples:

``` text
Slack      -> Block Kit
Telegram   -> messages + inline buttons
Web        -> structured event stream
CLI        -> terminal rendering
```

ACP is invisible to transports.

Transport plugins do not need to understand agents.

## 21.1 Transport Command and Interaction Layer

Messaging platforms need a way to express operations that are not normal
agent prompts.

Examples:

``` text
Slack:      @Hive /new_chat
Telegram:   /new_chat
Discord:    !new_chat
CLI:        hive session create
Web:        New Chat button
```

These are transport UX forms, not Hive Protocol method names.

The architectural rule is:

``` text
Transport owns:
  syntax
  prefixes
  mentions
  slash commands
  aliases
  buttons
  menus
  platform-specific interaction payloads

Hive owns:
  operation semantics
  authorization
  command_id
  session/run mutation
  result
  error semantics
```

Therefore:

``` text
@Hive /new_chat
       |
       v
Slack command parser
       |
       | name = "new_chat"
       | args = []
       v
transport command mapping
       |
       | method = "session.create"
       v
Hive Coordinator
```

A Slack plugin may map:

``` text
/new_chat       -> session.create
/agents         -> agent.list
/status         -> session.status
/cancel         -> session.cancel
/handoff        -> session.handoff
```

The exact aliases are transport configuration/UX. Hive Core must not
contain a Slack command registry.

The same Hive operation may therefore have different platform syntax:

``` text
Slack:
  @Hive /new_chat

Telegram:
  /new_chat

Discord:
  !new_chat

CLI:
  hive session create

Web:
  New Chat
```

All normalize to:

``` text
session.create
```

The reverse mapping is also transport-owned. Hive returns structured
operation results/events; Slack decides whether to render them as a
message, Block Kit card, thread reply, or other native UI.

### 21.1.1 Commands are not prompts

A transport must distinguish an operation command from ordinary user
text.

``` text
@Hive fix the login bug
        |
        +--> IncomingMessage
             |
             +--> session.prompt

@Hive /new_chat
        |
        +--> IncomingCommand
             |
             +--> session.create
```

A command must never be silently passed to the agent as a prompt when the
transport recognizes it as a Hive command.

Conversely, ordinary text that merely contains a slash-like token must
not be treated as a command unless the transport's command grammar says
so.

### 21.1.2 Mention and command normalization

A messaging transport may support both:

``` text
/new_chat
@Hive /new_chat
@Hive new_chat
```

if its platform UX allows them.

The parser first resolves platform-specific addressing:

``` text
mention / bot target / channel / thread
```

then parses the command syntax.

The normalized result must not retain platform-specific syntax as the
Hive operation name:

``` json
{
  "method": "session.create",
  "arguments": {}
}
```

`/new_chat` is therefore a presentation-level alias, not a Hive API
method.

### 21.1.3 Command aliases and discovery

Each transport may expose a discoverable command catalog generated from
the operations it chooses to expose.

For example:

``` text
/new_chat     Create a new Hive session
/agents       List available agents
/status       Show current session status
/cancel       Cancel the current run
/handoff      Handoff to another agent
```

The catalog is transport-owned. It may expose a subset of Hive
operations and may choose platform-native names.

Hive may expose operation metadata such as:

``` text
method
description
argument schema
required capability
transport capabilities
```

Transport capabilities may describe presentation features such as:

``` text
message_acknowledgement
reactions
buttons
threading
rich_messages
```

These capabilities affect rendering and delivery only. They do not change
Hive operation semantics and must not be required for core command
correctness.

Hive may therefore expose a capability such as:

``` text
message_acknowledgement:
  supported = true
  mode = reaction
```

while leaving the exact reaction syntax/emoji to the transport.

The catalog may also expose whether acknowledgement is enabled by
configuration for that transport.

It must not prescribe Slack/Telegram/Discord syntax.

This allows a future `/help` or bot command menu without coupling the
core to one messaging platform.

### 21.1.4 Command correlation and retries

A messaging platform may deliver the same inbound event more than once.

The transport therefore supplies a stable `SourceID` whenever the
platform provides one.

The Hive gateway derives or assigns the durable `command_id` once the
input is normalized:

``` text
transport SourceID
       |
       v
dedupe inbound delivery
       |
       v
single command_id
       |
       v
Hive command lifecycle
```

If the same platform event is delivered again, Hive must not create a
second logical command.

If a client intentionally submits the same logical operation again as a
new message, it receives a new `SourceID` and therefore a new
`command_id`.

This distinction is important:

``` text
duplicate delivery != user intent to repeat
```

### 21.1.5 Command results and asynchronous UX

A command can complete synchronously or asynchronously.

For example:

``` text
@Hive /new_chat
```

may immediately produce:

``` text
New chat created.
Session: sess_01...
```

while:

``` text
@Hive /handoff devin
```

may initially produce:

``` text
Handoff accepted.
Operation: cmd_01...
```

and later render completion/failure from Hive events.

The transport must not assume that the request/response lifetime is the
same as the operation lifetime.

This is the same command lifecycle defined by the Hive Protocol.

### 21.1.6 Message acknowledgement reactions

A transport may provide an immediate visual acknowledgement when a user
message has been accepted by the transport and Hive has become aware of
it.

Example:

``` text
User
  |
  | "fix the login bug"
  v
Slack Transport
  |
  +--> receive message
  +--> acknowledge message
  |       |
  |       +--> add 👀 reaction
  |
  v
Hive command/message processing
```

This is an optional transport capability, not a Hive event or agent
semantic.

The intended meaning is:

``` text
reaction present
    =
Hive has received/recognized the user's message
```

It must not mean:

``` text
agent started
agent is working
agent will succeed
agent has completed the task
```

The acknowledgement should happen as early as the transport can safely
confirm acceptance, and before waiting for the agent to produce output.

A transport may implement this with a platform-native reaction:

``` text
Slack      -> 👀 reaction
Discord    -> platform reaction
Telegram   -> supported reaction
Web        -> visual acknowledgement
CLI        -> no-op or local acknowledgement
```

The reaction is best-effort. If the platform does not support reactions,
the transport may omit it. If adding the reaction fails, the Hive command
must not fail merely because the acknowledgement could not be rendered.

The capability should therefore be represented as transport metadata:

``` text
message_acknowledgement
    supported: true/false
    mode: reaction | visual | none
```

The core should not assume a particular emoji or reaction name.

A transport may also remove or replace the acknowledgement later, but
that is a presentation concern. Hive does not use the reaction as a
source of truth for command or AgentRun state.

### 21.1.7 Interactive controls

Buttons, menus, modal submissions, and similar platform interactions are
commands or interaction envelopes, not a second business-logic system.

For example:

``` text
Slack:
  [Allow] [Deny]
       |
       v
IncomingInteraction
       |
       v
permission.respond
```

The interaction carries the authenticated principal and stable platform
source/action identity. Hive performs authorization and the domain state
transition.

The transport only renders the result.

## 21.2 Optional message acknowledgement

Acknowledgement is configured per transport:

``` toml
[transport.slack.acknowledgement]
enabled = true
mode = "reaction"
reaction = "eyes"
```

The `reaction` value is transport-specific presentation data. The Hive
core does not interpret `"eyes"` as a state.

Recommended lifecycle:

``` text
message received
      |
      v
transport accepts message
      |
      +--> best-effort acknowledgement
      |
      v
normalize -> Hive command/message
      |
      v
command lifecycle
```

If Hive rejects the message/command after parsing, the transport may
remove or replace the acknowledgement according to its UX policy. This
is optional and must not be required for correctness.

------------------------------------------------------------------------

# 22. Multiple Transports per Session

A Hive Session may have multiple transport bindings:

``` text
                    Session #42
                   /     |     \
               Slack  Telegram   Web
```

A transport binding has a Hive event cursor when the transport supports
durable replay. The binding cursor records the last Hive event sequence
that the transport has durably accepted for that binding. Transport-local
delivery offsets remain transport-owned.

After a plugin restart, Hive can resume the binding from the durable Hive
cursor without coupling the session lifecycle to the plugin process.
The cursor advances only when the transport plugin has durably accepted
that event for its own delivery workflow. Hive therefore provides
at-least-once delivery to the transport boundary, not exactly-once
external-platform delivery.

For example:

``` text
Slack     -> start task
Telegram  -> check progress
Web       -> inspect full stream
CLI       -> perform handoff
```

Each transport subscribes to relevant events.

The core does not need a separate notification-router subsystem if
transport-level event filtering is sufficient.

------------------------------------------------------------------------

# 23. Workspace Model

Hive manages workspace identity and node-local locations.

``` text
Workspace
  |
  +-- WorkspaceLocation
       +-- node_id
       +-- path
```

Example:

``` text
workspace: piceta

MacBook:
~/Projects/piceta

Mac Mini:
~/Projects/piceta
```

A workspace is not a Git abstraction.

Hive does not own:

``` text
git checkout
git branch
git worktree add
git merge
git rebase
```

Agents may use those mechanisms themselves.

Hive may detect that multiple active AgentRuns reference the same
workspace location and expose a warning, but it does not lock or
orchestrate Git.

------------------------------------------------------------------------

# 24. Agent Concurrency

Multiple AgentRuns may execute concurrently:

``` text
Session #42
  |
  +-- Claude -> design
  +-- Devin  -> implementation
  +-- Codex  -> review
```

Hive does not automatically serialize these runs.

Workspace coordination remains an agent/execution concern.

However, Hive should expose enough metadata to warn when multiple active
runs share the same workspace location.

The warning must not become a workspace lock.

------------------------------------------------------------------------

# 25. Coordinator-to-Node Networking

Nodes establish outbound connections to the coordinator.

``` text
Node
  -------------------->
       Coordinator
```

The connection is persistent and bidirectional:

``` text
WebSocket/TLS
```

A single connection can multiplex:

``` text
heartbeat
node status
agent event
agent start
agent prompt
agent cancel
permission response
handoff
event synchronization
```

### 25.1 Reconnect synchronization

Reconnect is a synchronization protocol, not merely a new WebSocket
connection.

A node presents:

``` text
node identity
current authenticated connection generation
last accepted coordinator term
last acknowledged durable event state
active AgentRun generations
buffered event IDs
```

The coordinator responds with the synchronization work required.

The old connection generation becomes invalid as soon as the new
connection is authenticated. A delayed packet or delayed command from the
old connection cannot be treated as a current node link.

Synchronization must be idempotent. Replaying a buffered event or retrying
a command must not create a second AgentRun, second handoff, or second
permission transition.

There is no requirement to combine:

``` text
REST
SSE
WebSocket
gRPC
external pub/sub
```

for the same node link.

A single bidirectional channel is the preferred baseline.

------------------------------------------------------------------------

# 26. Remote Networking

Hive supports multiple deployment networking strategies:

``` text
Single machine  -> localhost
Same LAN        -> LAN
Multiple nets   -> private overlay
Public gateway  -> outbound tunnel where appropriate
```

The Hive core should not be tightly coupled to a particular
infrastructure provider.

Target integrations may include:

``` text
direct
Tailscale
WireGuard
Cloudflare Tunnel
```

These are infrastructure options rather than requirements of the Hive
domain model.

The application should ultimately see an authenticated endpoint rather
than depend on a particular vendor.

------------------------------------------------------------------------

# 27. Node Discovery and Enrollment

LAN discovery may use:

``` text
mDNS
_hive._tcp.local
```

Discovery is a convenience mechanism, not the identity system.

A discovered node is not trusted automatically.

The enrollment flow is:

``` text
Node starts
    |
mDNS discovery
    |
Coordinator sees unknown node
    |
Administrator approves
    |
Public-key identity recorded
    |
Node reconnects using its identity
```

A manual enrollment mechanism should also exist for networks where mDNS
is unavailable.

The security decision is made through node identity and administrator
approval, not by the discovery protocol itself.

------------------------------------------------------------------------

# 28. Remote Agents

The architecture supports both local and remote agent targets:

``` text
AgentTarget
  |
  +-- local
  +-- remote
```

Local:

``` toml
command = ["devin", "acp"]
```

Remote:

``` toml
endpoint = "wss://..."
protocol = "acp"
```

Remote execution must use the same Agent Runtime abstraction.

Remote agent support should not require Hive core to know the vendor or
underlying implementation.

------------------------------------------------------------------------

# 29. Event Store and Retention

libSQL is the selected persistence technology.

Coordinator database:

``` text
~/.hive/data/coordinator.db
```

Node database:

``` text
~/.hive/data/node.db
```

Primary logical tables:

``` text
nodes
plugins
workspaces
sessions
agent_runs
events
session_snapshots
handoffs
permission_requests
conversation_bindings
```

The coordinator database is authoritative for coordinator-owned domain
state.

The node database is local execution state and an event buffer.

A node database is not an independent authoritative copy of sessions,
permissions, or handoff state. During coordinator failover it can provide
events and execution evidence for reconciliation, but it must not
silently become a second source of truth for arbitrary coordinator-owned
state.

Coordinator failover therefore requires an explicit state-recovery
procedure. The implementation must expose whether recovered state is:

``` text
complete
partially recovered
ambiguous
```

Ambiguous state must be surfaced as unavailable/retryable rather than
invented.

## 29.1 Retention

Event retention is configurable:

``` toml
[event_store]
retention_days = 30
```

Event retention and session retention are independent.

A session may remain for a long time while old events are pruned.

Snapshots retain the context required for practical resume/handoff
without requiring replay of an unbounded event history.

Retention must not be coupled to session lifecycle semantics.

------------------------------------------------------------------------

# 30. Persistence and Snapshots

Snapshots are point-in-time representations of the context required to
resume or hand off a session.

A snapshot is not a replacement for the event log.

``` text
Session
  |
  +-- Snapshot
  +-- Recent Events
  +-- Historical Events
```

Snapshots should be versioned.

A snapshot must identify the schema/context version used to create it so
future versions can migrate or reject it explicitly.

------------------------------------------------------------------------

# 31. Storage Technology

libSQL is the selected storage layer.

The Go implementation should use the maintained `go-libsql` direction
rather than the deprecated older Go client.

The use of CGO/native libraries is an explicit build and release
consideration.

Release automation must cover the supported operating-system and
architecture combinations.

No external Redis, Kafka, NATS, Postgres, or message queue is required
for the intended deployment model.

------------------------------------------------------------------------

# 32. TUI

The TUI is the management plane, not the primary chat UI.

The target management surface includes:

``` text
Sessions
Agents
Nodes
Plugins
Network
Security
Events
Config
Logs
```

Example:

``` text
Hive

● Coordinator       Mac Mini
● 3 Nodes
● 5 Agents
● 8 Sessions
● 2 Pending permissions
```

Session detail:

``` text
Session #42

Workspace: piceta
Current: Devin
Node: Mac Mini

Runs:
  ✓ Claude
  ● Devin
  ○ Codex

[Handoff]
[Cancel]
[Open]
```

The CLI/TUI should remain a client of Hive APIs rather than contain
independent orchestration logic.

------------------------------------------------------------------------

# 33. Web UI

Web UI is not a core architectural requirement.

The initial management surface is the CLI/TUI.

A future Web UI should use the same Hive protocol and should not
introduce a second backend.

A possible future implementation is:

``` text
hive-plugin-web
```

with the same security and event semantics as other transports.

------------------------------------------------------------------------

# 34. Go Implementation

Go is the selected implementation language.

The system needs:

``` text
daemon
networking
subprocess management
plugins
WebSocket
TUI
SQLite/libSQL
cross-platform builds
```

Go provides a good balance of implementation speed, operational
simplicity, and runtime characteristics for this workload.

Rust remains a possible future implementation choice if actual
measurements demonstrate a meaningful need, but performance assumptions
should not drive an unnecessary language change before profiling.

------------------------------------------------------------------------

# 35. Repository Structure

The repository should remain a monorepo.

Recommended initial structure:

``` text
hive/
├── cmd/
│   └── hive/
│       └── main.go
│
├── internal/
│   ├── session/
│   ├── agent/
│   ├── event/
│   ├── node/
│   ├── plugin/
│   ├── transport/
│   ├── security/
│   ├── storage/
│   ├── discovery/
│   ├── network/
│   ├── cluster/
│   └── tui/
│
├── protocol/
│   ├── hive/
│   │   ├── v1/
│   │   ├── schema/
│   │   └── docs/
│   └── events/
│
├── plugins/
│   ├── acp/
│   ├── slack/
│   ├── telegram/
│   ├── web/
│   ├── tailscale/
│   └── wireguard/
│
├── sdk/
│   ├── typescript/
│   ├── rust/
│   └── python/
│
├── migrations/
│
├── docs/
│   ├── architecture/
│   ├── protocol/
│   └── plugins/
│
└── go.mod
```

Directories may be introduced incrementally as implementations become
substantial, but the architectural package boundaries should remain
aligned with the above domains.

SDKs do not need complete implementations on day one. Protocol schemas
and documentation are the source of truth.

------------------------------------------------------------------------

# 36. Core APIs

Session management:

``` go
type SessionManager interface {
    Create(ctx context.Context, req CreateSession) (*Session, error)
    Prompt(ctx context.Context, sessionID string, prompt Prompt) error
    Cancel(ctx context.Context, sessionID string) error
    Handoff(ctx context.Context, req HandoffRequest) (*AgentRun, error)
    Resume(ctx context.Context, sessionID string) error
}
```

Agent runtime:

``` go
type AgentRuntime interface {
    Start(ctx context.Context) error
    Stop(ctx context.Context) error

    CreateSession(ctx context.Context, req SessionRequest) (AgentSession, error)
    ResumeSession(ctx context.Context, req ResumeRequest) (AgentSession, error)

    Prompt(ctx context.Context, session AgentSession, prompt Prompt) error
    Cancel(ctx context.Context, session AgentSession) error
    RespondToRequest(ctx context.Context, req RequestResponse) error
}
```

The interfaces must remain capability-focused.

Do not add agent-specific tool methods to the Hive core.

------------------------------------------------------------------------

# 37. Canonical Data Model

## Command

``` text
id
actor
source_id
method
accepted_term
state
result
error
created_at
updated_at
```

`id` is the idempotency key for the logical mutating operation.
`source_id` correlates the command with the originating transport
delivery when the transport provides a stable identifier.

## ClusterAuthority

``` text
current_term
current_coordinator_id
lease_expires_at
control_sequence
updated_at
```

The durable record describes the currently recognized coordinator
authority boundary. Standby replicas also keep their recovered control
snapshot/journal boundary.

## ControlSnapshot

``` text
snapshot_id
control_sequence
term
created_at
payload
```

## ControlJournalEntry

``` text
control_sequence
term
command_id
method
domain_event_id
payload
created_at
```

A standby applies journal entries idempotently by `control_sequence` and
records the highest contiguous durable prefix it has recovered.

## NodeConnection

``` text
node_id
coordinator_id
connection_generation
term
state
connected_at
last_heartbeat_at
```

A superseded connection generation cannot accept new authoritative
commands.

## Session

``` text
id
metadata
workspace_id
state
created_at
updated_at
```

## AgentRun

``` text
id
session_id
agent_id
node_id
protocol
runtime_session_id
execution_generation
node_execution_id
state
started_at
ended_at
```

`execution_generation` fences logical execution retries/resumes.
`node_execution_id` identifies the concrete execution instance on the
node and is used during start/reconnect reconciliation.

## Handoff

``` text
id
session_id
source_run_id
target_run_id
summary
context_snapshot_id
created_at
```

## Event

``` text
id
sequence
session_id
run_id
origin_node
timestamp
type
version
protocol
method
payload
```

## PermissionRequest

``` text
id
session_id
run_id
agent_request_id
payload
state
expires_at
created_at
resolved_at
```

## TransportBinding

``` text
id
session_id
transport
conversation_id
principal
event_cursor
created_at
updated_at
```

`event_cursor` is the last durably accepted Hive event sequence for this
binding when durable replay is supported.

Transport-specific source IDs are delivery correlation data and are not
Hive event IDs. They may be retained with commands/interactions for
deduplication and audit.

Message acknowledgement settings are transport/plugin configuration, not
Session or Command state.

## Plugin

``` text
id
type
identity
version
trust_state
created_at
updated_at
```

## PluginInstance

``` text
id
plugin_id
process_identity
connection_generation
state
started_at
ended_at
```

## CapabilityRegistration

``` text
plugin_id
capability
version
registration_state
created_at
updated_at
```

## Workspace

``` text
id
name
created_at
updated_at
```

## WorkspaceLocation

``` text
workspace_id
node_id
path
```

------------------------------------------------------------------------

# 38. Full Example Flow

A complete flow looks like:

``` text
Slack
  |
  | "design auth"
  v
Slack Plugin
  |
  +-- parse message/command/interaction
  |
  +-- normalize platform identity + source_id
  |
  v
Hive Coordinator
  |
  +-- Security
  |
  +-- Session #42
  |
  +-- Route -> Node A
             |
             v
         Claude ACP
             |
             +-- events
             |
             +-- design completed
                     |
                     v
                  Handoff
                     |
             optional summary
                     |
                     v
              Hive Context
                     |
                     v
                  Node B
                     |
                     v
                 Devin ACP
                     |
             permission request
                     |
                     v
                Coordinator
                     |
                     v
                   Slack
              [Allow] [Deny]
                     |
                     v
                   Devin
```

Throughout this flow:

``` text
Slack does not know ACP.
ACP does not know Slack.
Devin does not need to know Hive.
Hive does not need vendor-specific Devin logic.
```

------------------------------------------------------------------------

## 38.1 Messaging Command Flow

A command flow uses the same architectural boundaries:

``` text
Slack
  |
  | "@Hive /new_chat"
  v
Slack Transport
  |
  | parse + normalize
  | source_id = slack:event:...
  | method = session.create
  v
Hive Coordinator
  |
  +-- authenticate principal
  +-- authorize session.create
  +-- dedupe source_id/command_id
  +-- execute session.create
  |
  v
Command result + events
  |
  v
Slack Transport
  |
  +-- render "New chat created"
```

The important distinction is:

``` text
"/new_chat"        = transport syntax
"session.create"   = Hive operation
"command_id"       = logical operation identity
"source_id"        = inbound delivery identity
```

------------------------------------------------------------------------

# 39. Explicit Non-Goals

The following are deliberately outside the core responsibility of Hive:

``` text
Git orchestration
tool permission engine
agent-specific business logic
vendor-specific agent implementations
Slack-specific command semantics
agent reasoning
agent tool execution
workspace locking
```

The architecture nevertheless leaves extension points for:

``` text
remote agents
custom agent protocols
additional transports
network integrations
Web UI
SDKs
```

------------------------------------------------------------------------

# 40. End-to-End Failure Trace

The major failure paths are defined by the following stable sequences.

## 40.1 Normal prompt

``` text
Transport
  -> parse message/command/interaction
  -> normalize source_id + principal
  -> authenticated Hive command
  -> authorize + dedupe command_id
  -> choose Session / AgentRun
  -> durable command/domain mutation
  -> Node command with term + connection generation + execution generation
  -> Agent Runtime
  -> events
  -> Event Store + Event Bus
  -> transport bindings
```

The coordinator owns logical state; the node owns the live execution.

## 40.2 Coordinator crash during start

``` text
command accepted
  -> logical AgentRun + start operation durable
  -> node receives start
  -> coordinator dies before acknowledgement
  -> new coordinator recovers replicated control state
  -> node reconnects with execution evidence
  -> reconcile by command_id + generation + node_execution_id
  -> complete existing start OR classify ambiguous
```

The replacement coordinator never sends an unconditional second start.

## 40.3 Node disconnect during execution

``` text
Node disconnects
   -> existing execution remains connectivity-uncertain
   -> node reconnects
       -> reconcile generation + runtime state
   OR
   -> execution lease expires
       -> AgentRun = interrupted
```

No automatic replacement execution is implied.

## 40.4 Permission race

``` text
permission = pending
   -> response A \
                  +--> atomic terminal transition --> approved/denied
   -> response B /
```

Exactly one valid terminal transition wins.

## 40.5 Event cursor after retention

``` text
client cursor = old
   -> requested sequence pruned
   -> cursor_expired + snapshot boundary
   -> client rehydrates snapshot
   -> client resumes from next valid sequence
```

This avoids pretending that historical cursors survive retention forever.

## 40.6 Message acknowledgement

``` text
user message
   -> transport receives it
   -> optional best-effort 👀 reaction
   -> Hive processes message
```

The reaction is presentation feedback only. Failure to add it does not
fail the message, command, Session, or AgentRun.

## 40.7 Duplicate messaging delivery

``` text
Slack sends event E1
   -> transport parses @Hive /new_chat
   -> source_id = E1
   -> command_id = C1
   -> session.create executes

Slack redelivers E1
   -> same source_id
   -> existing command C1 returned
   -> no second Session
```

A new user message containing `/new_chat` has a different source ID and
therefore represents a new command.

## 40.8 Stale coordinator/node/plugin connection

``` text
old connection
   -> new authenticated connection established
   -> connection generation increments
   -> old generation invalidated
   -> stale message rejected
```

Term fencing handles stale coordinator authority; connection-generation
fencing handles stale live channels.

## 40.9 Coordinator failover with incomplete state

``` text
candidate
   -> recover replicated control snapshot/journal
   -> record recovered boundary
   -> reconcile node execution evidence/events
   -> safe operations become available
   -> incomplete/ambiguous operations remain unavailable/retryable
```

A failover never implies that missing state has been reconstructed by
guesswork.

------------------------------------------------------------------------

# 41. Things Designed from Day One

The following must be treated as stable architectural boundaries from
the beginning:

``` text
1. Hive Protocol
2. Plugin process boundary
3. Session / AgentRun model
4. Event model
5. Handoff model
6. Security boundary
7. Node identity
8. Coordinator <-> Node wire protocol
9. Agent Runtime interface
10. Transport interface
11. Transport command/interaction normalization
```

These boundaries should be tested independently.

------------------------------------------------------------------------

# 42. Implementation Roadmap

The architecture is complete even when implementation is incremental.

## Phase 1 --- Core Foundation

Implement:

``` text
Go daemon
libSQL
Session model
AgentRun model
Event model
Event bus
Storage
Hive Protocol v1
Agent Runtime interface
CLI/TUI foundation
```

The goal is a working single-machine Hive.

## Phase 2 --- Plugin and ACP Runtime

Implement:

``` text
plugin supervisor
plugin handshake
plugin lifecycle
ACP plugin
generic agent profiles
permission relay
```

At this stage a local Hive can run multiple ACP agents.

## Phase 3 --- Transport

Implement:

``` text
one production transport (Slack)
```

as an independent transport plugin.

A second transport is a compatibility proof, not a v1 product requirement.
Every transport must drive the same Session model and implement the
transport command/interaction boundary.

Minimum transport tests include:

``` text
ordinary message -> session.prompt
/new_chat -> session.create
@Hive /new_chat -> session.create
unknown command -> transport-level command error
duplicate inbound event -> same command_id
new repeated user message -> new command_id
button interaction -> correct Hive operation
acknowledgement enabled -> reaction/visual acknowledgement attempted
acknowledgement unsupported -> message still processes normally
acknowledgement failure -> message still processes normally
```

## Phase 4 --- Multi-Node

Implement:

``` text
Node protocol
node identity
node enrollment
outbound node connection
reconnect
event synchronization
remote execution
```

The coordinator remains the authoritative control-plane instance.

## Phase 5 --- Handoff

Implement:

``` text
session snapshots
handoff context
agent summaries
cross-agent handoff
cross-node execution
```

## Phase 6 --- Failover

Deferred: this phase is not required by v1. It is retained as target
architecture for a future deployment capability.

Implement:

``` text
heartbeat
lease
priority
term
coordinator election
authority fencing
state reconciliation
command deduplication
event synchronization
control-state replication for promotion
connection-generation fencing
```

This phase must include failure-injection tests for:

``` text
coordinator crash
node disconnect
network partition
duplicate command
duplicate event
stale coordinator
reconnection
```

## Phase 7 --- Networking and Discovery

Implement:

``` text
LAN
mDNS
Tailscale
WireGuard
Cloudflare Tunnel
```

as infrastructure integrations without coupling them to the core session
model.

## Phase 8 --- Ecosystem

Implement:

``` text
Web UI
plugin SDKs
custom transport examples
custom agent protocol examples
remote agent integrations
additional plugin types
```

------------------------------------------------------------------------

# 43. Required Correctness Properties

The implementation should have explicit tests for these invariants.

### Session isolation

An unauthorized principal cannot read or mutate a session.

### Agent isolation

One AgentRun cannot accidentally receive events or commands belonging to
another AgentRun.

### Transport isolation

A transport cannot access arbitrary internal session state.

### Plugin isolation

A plugin crash does not terminate the coordinator or unrelated sessions.

### Event idempotency

Processing the same event twice does not create an incorrect duplicate
side effect.

### Permission idempotency

A permission response is resolved at most once.

### Reconnect safety

A node can disconnect and reconnect without creating duplicate node
identities or duplicate AgentRuns.

### Command idempotency

Retrying the same mutating command with the same `command_id` does not
create a second logical operation.

### Event reconciliation

A buffered node event can be replayed after reconnect without creating
a duplicate durable event.

### Execution fencing

A stale command from an older AgentRun generation cannot control a newer
execution instance.

### Permission safety

Coordinator loss cannot convert a pending permission request into an
implicit approval.

### Handoff recovery

A handoff that crosses a coordinator, node, or process failure cannot be
reported as successful unless the target execution and required context
are durably established.

### AgentRun start reconciliation

A lost acknowledgement for an AgentRun start cannot cause a second
execution for the same generation. Ambiguous execution state must be
reconciled or fail closed before a replacement generation is started.


A crash at any point in handoff creation/recovery leaves a durable,
inspectable handoff state and never silently claims successful transfer.

### Coordinator failover

A stale coordinator cannot continue authoritative writes after a higher
valid term is observed. Promotion uses the candidate's recovered control
boundary explicitly; incomplete state is not fabricated.

### Control-state replication

A standby can identify exactly which committed control mutations it has
recovered. Promotion never treats an unreplicated control mutation as
known durable state.

### Connection fencing

A superseded coordinator-to-node or plugin process connection cannot
issue new authoritative commands after a newer authenticated connection
generation has taken over.

### Cursor correctness

A client or transport binding either replays from a valid durable cursor
or receives an explicit cursor-expired/snapshot boundary response.

### Transport command correctness

Transport-specific command syntax normalizes to a valid Hive operation,
while ordinary messages remain prompts. Platform redelivery of the same
source event does not create a second logical command.

### Message acknowledgement correctness

An optional transport acknowledgement may provide immediate visual
feedback that a message was received/recognized. Its failure cannot fail
the underlying Hive operation, and its presence must never be interpreted
as proof of AgentRun execution or completion.

### Authentication and authorization

A caller cannot obtain authority merely by naming a principal. Each
accepted command is tied to an authenticated actor and an authorization
decision appropriate to the target resource.

### Handoff correctness

A handoff creates a distinct target AgentRun and never attempts to
reinterpret the source runtime's native session ID as the target
runtime's session ID.

### Session continuity

A completed AgentRun does not destroy the Hive Session.

### Unknown protocol data

Unknown agent protocol events can be preserved without crashing or
requiring an immediate Hive schema change.

------------------------------------------------------------------------

# 44. Failure Semantics Matrix

The following cases are part of the architecture rather than
implementation-specific behavior.

| Failure | Expected behavior |
|---|---|
| Transport crashes | Unrelated sessions and agent runs continue. Transport reconnects and resumes from its binding/cursor policy. |
| Plugin crashes | Plugin is restarted according to policy; unrelated core state continues. |
| Agent process crashes | AgentRun becomes `failed` or `interrupted` according to observed execution state; Hive Session remains intact. |
| Node loses coordinator connection | Existing agent processes may continue; events buffer locally; new control-plane operations may be unavailable. |
| Coordinator crashes | Eligible coordinator-capable node may promote after lease/term rules; agent processes are not implicitly terminated. |
| Network partition | Conflicting coordinator candidates may temporarily exist; the system does not claim strong consistency. |
| Stale coordinator reconnects | It observes the higher term, relinquishes authority, and stops authoritative writes. |
| Duplicate command | Same `command_id` resolves to the same logical operation/result. |
| Duplicate event | Same `event_id` is not persisted as a second durable event. |
| Pending permission during coordinator failure | Remains pending until timeout/recovery; concurrent responses use one atomic terminal transition; never becomes implicit approval. |
| Handoff interrupted | Handoff remains inspectable as incomplete/failed; no false success is recorded. |
| Node permanently disappears | Active runs eventually become `interrupted` after the configured execution lease; Hive does not automatically duplicate execution. |
| Old node connection remains alive | Superseded connection generation is rejected for new authoritative commands. |
| Plugin process restarts | Stable plugin trust identity remains; old process instance loses invocation authority; new instance re-handshakes and re-registers. |
| Client asks for pruned event cursor | Hive returns cursor-expired/snapshot boundary instead of silently skipping history. |
| Candidate coordinator has stale replicated state | Promotion records the recovered control-sequence boundary; affected operations remain incomplete/unavailable until reconciled. |
| Standby misses a control-journal entry | It remains behind the active control boundary and cannot claim the missing mutation was durable on the standby. |

These rules define the minimum failure contract that clients and
integrations may rely on.

------------------------------------------------------------------------

# 45. Operational Principles

Hive should behave like a background system rather than a distributed
infrastructure project.

A normal single-machine installation should require approximately:

``` text
hive
```

A multi-node installation should primarily add:

``` text
node enrollment
coordinator endpoint
identity
```

The user should not need:

``` text
Redis
Kafka
NATS
Postgres
Kubernetes
service mesh
external consensus
```

to operate a personal Hive.

The architecture remains capable of adding those technologies later if
the scale or deployment model genuinely requires them.

------------------------------------------------------------------------

# 46. Final Architecture

The final conceptual model is:

``` text
                         HIVE
              Personal Agent Gateway
                         |
             +-----------+-----------+
             |                       |
        Control Plane          Execution Plane
             |                       |
        Coordinator                 Nodes
             |                       |
      +------+-------+         +-----+-----+
      |      |       |         |           |
  Sessions Events Transports  ACP        Remote
      |                         |           |
      |                      Claude       Agent
      |                      Devin
      |                      Codex
      |
      +-- AgentRuns
      +-- Handoffs
      +-- Snapshots
      +-- Bindings
```

Ownership remains:

``` text
Hive owns:
  identity
  sessions
  routing
  context
  events
  handoff
  node management
  external bindings

Agent owns:
  reasoning
  tools
  filesystem operations
  shell
  native permission semantics

Transport owns:
  presentation
  message delivery
  platform interactions

Plugin owns:
  integration boundary
```

The most important architectural invariant is that these
responsibilities remain independent.

Hive is not an agent runtime.

Hive is not a Git manager.

Hive is not a messaging platform.

Hive is not a tool-permission engine.

Hive is not OpenACP rewritten in Go.

Hive is the orchestration layer that connects these systems into a
single session-centric, multi-node agent environment.

------------------------------------------------------------------------

# 47. Version History

## v7 design updates

This revision records the v1 scope decision. The architecture is
unchanged; the failover posture is clarified.

The important changes are:

``` text
1. Coordinator failover is restated as a future deployment capability
2. The v1 architecture does not require coordinator failover
3. Phase 3 targets one production transport instead of two
4. Phase 6 is marked as deferred for v1
5. Section numbering corrected (two sections were numbered 40 and 46 was
   missing)
```

The v1 cut line, resolved decisions, and milestone tracking live in
hive-v1-implementation-plan.md.

## v6 design updates

This revision adds optional transport-level message acknowledgement.

The important addition is:

``` text
1. Optional immediate message acknowledgement/reaction
2. Explicit distinction between "Hive received/recognized" and
   "AgentRun started/completed"
3. Best-effort acknowledgement semantics
4. Per-transport acknowledgement configuration/capability
5. Failure and compatibility tests for acknowledgement delivery
```

The acknowledgement is deliberately not part of Hive domain state.

## v5 design updates

This revision closes the transport-command abstraction gap identified in
the v4 review.

The important additions are:

``` text
1. Explicit IncomingMessage / IncomingCommand / IncomingInteraction boundary
2. Transport command and interaction normalization
3. Slack-style @Hive /new_chat -> session.create flow
4. Separation of transport syntax from Hive operation semantics
5. Command source_id for inbound delivery correlation
6. Duplicate delivery vs intentional repeated command semantics
7. Command discovery/operation metadata without a core command registry
8. Native interactive controls mapped into Hive operations
9. Command parsing/retry tests added to the transport roadmap
```

The design does not add Slack-specific command semantics to Hive Core.

## v4 design updates

This revision completes the second failure-trace pass across security,
plugin lifecycle, remote nodes, event replay, and coordinator promotion.

The important additions are:

``` text
1. Best-effort replication of coordinator-owned durable state for failover
2. Coordinator-to-node connection-generation fencing
3. Explicit durable command status/query semantics
4. Event cursor-expiration and snapshot recovery semantics
5. Authenticated actor context and client authorization boundary
6. Node enrollment, key rotation/revocation, and connection fencing
7. Durable transport-binding event cursors
8. Stable plugin identity vs plugin process-instance identity
9. Atomic first-wins permission resolution
10. Runtime execution identity for start reconciliation
11. Expanded canonical data model for commands, authority, connections,
    plugin instances, and cursors
12. End-to-end failure traces for the major lifecycle paths
```

The design still intentionally does not add distributed consensus,
workspace locking, automatic AgentRun replacement, or vendor-specific
agent semantics merely to cover small edge cases.

## v3 design updates

This revision incorporates the first failure-trace review of v2 while
preserving the existing target capabilities and architectural boundaries.

The important additions are:

``` text
1. AgentRun start/recovery reconciliation
2. Explicit session.prompt routing semantics
3. Execution lease and execution-generation relationship
4. Coordinator command/domain transaction boundary
5. Session event sequence explicitly defined as ordering, not causality
```

The design intentionally does not add distributed consensus, workspace
locking, automatic AgentRun replacement, or other infrastructure merely
to cover small edge cases.

------------------------------------------------------------------------

# 48. Open-Source Quality Bar

Before calling the first stable release, the project should provide:

``` text
Architecture documentation
Protocol specification
Plugin specification
Security model
Configuration reference
Failure semantics
Authenticated actor model
Transport command/interaction contract
Coordinator promotion/recovery procedure
Cursor retention/replay procedure
Failure-injection test suite
Transport command redelivery/interaction test suite
Optional transport acknowledgement test suite
State recovery and reconciliation rules
Migration/versioning rules
Example plugin
Example custom client
Example multi-node deployment
Integration tests
Failure-injection tests
Cross-platform release builds
```

The public documentation must clearly distinguish:

``` text
guaranteed behavior
best-effort behavior
optional integrations
future extension points
```

This is especially important for failover, event delivery, plugin trust,
remote networking, and agent runtime compatibility.

The result should be a system that is small enough to run quietly on a
personal machine, but whose boundaries are explicit enough that
independent developers can build transports, agent adapters, clients,
and integrations without modifying the Hive core.
