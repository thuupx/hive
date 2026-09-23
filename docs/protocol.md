# Hive Protocol v1

Hive Protocol is the public application protocol. ACP is not the Hive protocol:
ACP is one agent runtime that Hive adapts to.

## Wire format

```text
JSON-RPC 2.0, transport independent
```

The base framing for a stream transport (plugin stdio, unix socket, node link) is
one message per line. A WebSocket transport carries one message per frame.

Hive adds one extension to the envelope: a `meta` member carrying context that is
not part of JSON-RPC.

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "session.create",
  "params": { "commandId": "cmd_01H..." },
  "meta": {
    "protocolVersion": { "family": "hive", "major": 1, "minor": 0 },
    "actor": "slack:U123"
  }
}
```

`actor` is a **claimed** identity used for routing context. Authentication is
transport-specific, and the transport establishes the authenticated principal. A
claimed actor is never proof of identity.

## Versioning

The protocol distinguishes the family/version from individual message and event
versions. Compatibility requires the same family and major version; a lower minor
version is compatible because minor bumps are additive.

Unknown protocol data is preserved rather than dropped, so a newer peer does not
require an immediate schema change on this one.

## API domains

```text
Control API   session.*  agent.*  node.*  workspace.*  command.*  permission.list
Node API      node.*  execution.*  event.*
Plugin API    plugin.*  capability.*  lifecycle.*  event.subscribe  event.ack
Transport API transport.inbound
```

A connection may invoke only what its role and granted capabilities allow. A
plugin does not reach `session.*` by default: a transport plugin is granted the
capabilities it needs and may assert only principals of its own transport.

## Commands

Every mutating command carries a `command_id`, which is the idempotency key and is
stable across retries of the same logical operation.

```text
received -> accepted -> completed
                    \-> rejected
                    \-> failed
                    \-> retryable
```

The durable command record is the operation status resource. A client queries it
through `command.get` rather than holding a request open until the operation
finishes, because a command's lifetime and a request's lifetime are different
things.

Command acceptance, command identity, and the authoritative domain mutation
commit in a single transaction. Realtime publication happens after commit, and is
therefore at-least-once.

## Events

An event envelope:

```text
event_id  sequence  timestamp  session_id  run_id  source  type  version
protocol  method    payload
```

`sequence` is assigned by the authoritative event store when the event becomes
durable, and is strictly monotonic within a session stream.

`sequence` is stream ordering only. It is **not** a causal ordering between
AgentRuns, nodes, or external side effects: a higher sequence does not imply that
the later event observed or depends on the earlier one.

Core event types are deliberately few:

```text
message  status  run.started  run.finished
permission.requested  permission.responded  handoff.created  error  agent.raw
```

Agent-specific streaming updates stay protocol-native and travel as `agent.raw`
unless Hive itself needs to act on them. That is what prevents schema explosion as
an agent protocol grows.

## Reconnection

A client maintains a stream cursor and asks to replay from it. When the requested
sequence has been pruned, Hive answers `cursor_expired` with the latest available
snapshot boundary and the next valid sequence, rather than silently skipping
history.

## Raw protocol data

```json
{
  "type": "agent.raw",
  "version": 1,
  "protocol": "acp",
  "method": "future/thing",
  "payload": {}
}
```

Hive does not create a new core event type merely because an agent protocol adds
a feature.

## Agent runtime interface

The interface Hive requires from an agent runtime is deliberately small:

```go
Start, Stop
CreateSession, ResumeSession
Prompt, Cancel
RespondToRequest
```

It carries no agent-specific tool methods. Running shells, writing files,
checking out Git, and installing packages belong to the agent runtime. A runtime
may additionally implement a reconciliation capability so the node execution
controller can resolve a lost start acknowledgement instead of inferring success
from a local flag.
