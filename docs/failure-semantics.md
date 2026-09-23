# Failure semantics

This is the minimum failure contract clients and integrations may rely on. It
separates **guaranteed** behaviour from **best-effort** behaviour, because the
difference matters more than the happy path.

## Guaranteed

These hold regardless of ordering, redelivery, or which process failed.

| Situation | Behaviour |
|---|---|
| Duplicate command | The same `command_id` resolves to the same logical operation. A retry never creates a second session, AgentRun, handoff, or permission transition. |
| Duplicate event | The same `event_id` is not persisted as a second durable event, and consumes no stream sequence. |
| Transport redelivery | The same platform delivery derives the same `command_id` through its `source_id`. A user deliberately repeating an action produces a new one. |
| Node reconnect | Reconnecting creates a new connection generation, invalidates the previous one, and creates no duplicate AgentRun. |
| Execution fencing | A command for an older execution generation is refused, so a late command cannot control a newer execution. |
| Stale coordinator connection | A superseded connection generation is refused for new authoritative commands. |
| Permission resolution | Exactly one terminal transition wins. A repeated response observes the resolved state. |
| Permission safety | A pending request never becomes an implicit approval, including on coordinator loss. |
| Handoff recovery | A handoff is not reported successful unless the target execution started. A crash leaves an inspectable state. |
| Cursor correctness | A replay either serves a valid durable cursor or reports `cursor_expired` with the snapshot boundary and the next valid sequence. |
| Session continuity | A completed AgentRun does not destroy the session. |
| Unknown protocol data | An unknown agent protocol event is preserved as raw data and does not crash Hive or require an immediate schema change. |
| Authentication | Authority comes from the authenticated connection, never from a claimed principal string. |
| Plugin isolation | A plugin crash does not terminate the coordinator or unrelated sessions. |

## Best-effort

These can fail or lag, and the system says so rather than pretending otherwise.

| Situation | Behaviour |
|---|---|
| Realtime delivery | A subscriber whose bounded queue is full loses events and is reported as lagging. It recovers from its durable cursor. |
| Event publication | Events are durable before they are published, and publication is at-least-once: a crash between publishing and marking republishes, and consumers deduplicate by `event_id`. |
| Transport delivery to the platform | Hive provides at-least-once delivery to the transport boundary, not exactly-once delivery to the external platform. |
| Acknowledgement reaction | Optional, and its failure never fails the underlying operation. |
| Shared workspace location | Hive warns; it never locks. Concurrent runs on one location are a real workflow. |

## Explicitly not claimed

- **Strong consistency across partitioned coordinators.** A partition can
  temporarily produce competing coordinator candidates. The system does not claim
  linearizable distributed state.
- **Coordinator failover.** It is not part of v1. A coordinator is a single
  authority and recovery is a restart. Terms, leases, and election are target
  architecture, not a v1 guarantee.
- **Exactly-once side effects.** The agent runtime owns tool execution, and Hive
  cannot make an external side effect idempotent on its behalf.

## Failure traces

### Coordinator crash during a start

```text
command accepted
  -> logical AgentRun and start operation durable
  -> node receives start
  -> coordinator dies before the acknowledgement
  -> node reports what it actually has on reconnect
  -> reconcile by command id, generation, and node execution id
```

A replacement never sends an unconditional second start. v1 has no failover, so
the trace ends at "the run is reconciled or classified interrupted".

### Node disconnect during execution

```text
node disconnects
  -> the execution stays connectivity-uncertain
  -> the node reconnects and reconciles the generation
  OR
  -> the execution lease expires
  -> AgentRun = interrupted
```

Lease expiry never starts a replacement execution. The original execution may
still be alive, and duplicating it could repeat side effects.

### Plugin crash

```text
plugin exits
  -> the supervisor notices
  -> restart according to policy
  -> handshake, trust, register, ready
```

Unrelated sessions keep running. A transport that cannot start does not stop the
coordinator.

### Cursor after retention

```text
client cursor = old
  -> the requested sequence has been pruned
  -> cursor_expired plus the snapshot boundary and next valid sequence
  -> client rehydrates, then resumes
```

Hive does not pretend a historical cursor survives retention forever.
