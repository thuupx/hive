# Hive v1 — Implementation Plan

Tracking artifact for the transition from architecture document
(`hive-solution-design-v6.md`) to an implementable v1.

- Checkbox = a task that must be completed.
- A milestone is done only when its **Exit criteria** are all satisfied.
- Section references (`§N`) point at `hive-solution-design-v6.md`.

------------------------------------------------------------------------

## 1. v1 Cut Line

v1 is a **single-machine, single-transport, multi-agent** Hive.

```text
IN v1
  single machine
  coordinator (single authority) + node (child process)
  ACP agent runtime via hive-plugin-acp
  generic agent profiles
  permission relay (no tool-permission engine)
  Slack transport (single production transport)
  CLI/TUI as a Hive protocol client (NOT a transport plugin)
  Hive Session + multiple AgentRuns
  deterministic prompt routing
  handoff + context snapshots
  same-agent restore
  durable command_id / event_id / execution_generation
  libSQL storage

OUT of v1 (architecture keeps the boundary; implementation deferred)
  coordinator failover (terms, leases, election, fencing)
  control-state replication (snapshot + control journal)
  multi-machine nodes, node enrollment, node discovery
  mDNS / Tailscale / WireGuard / Cloudflare Tunnel
  remote agents
  second transport (Telegram / Discord)
  Web UI
  SDKs (TS / Rust / Python)
  workspace locking, Git orchestration
```

Rationale: §41 Phases 1–3 produce the usable product; handoff is pulled
forward because it is the only feature that validates the central claim
`Hive Session != Agent Session`.

------------------------------------------------------------------------

## 2. Locked Decisions (ADR summary)

### D1 — Single-machine topology: `hive` spawns the node as a child process

`hive` runs the coordinator and spawns/manages a node child process. The
two speak the real Node Protocol over a loopback WebSocket.

- Keeps §44 UX (single `hive` command to run everything).
- Makes §2.1 `Control Plane != Execution Plane` and §40 item 8
  (`Coordinator <-> Node wire protocol`) real and testable in v1.
- Consequence: node lifecycle, connection generation, execution lease,
  and reconnect synchronization are exercised from day one.

### D2 — Slack is the single production transport; CLI/TUI is a client

- Slack exercises §21 transport normalization
  (`IncomingMessage` / `IncomingCommand` / `IncomingInteraction`) and the
  plugin process boundary.
- CLI/TUI talks the Hive Protocol directly over a Unix socket and is a
  client of Hive APIs (§32). It is **not** a transport plugin and must not
  contain orchestration logic.

### D3 — Storage: libSQL via `go-libsql` (CGO accepted)

- §31 direction: use the maintained `go-libsql`, not the deprecated older
  Go client.
- Accepted cost: CGO + native library, heavier release automation,
  no trivial cross-compilation.
- Mitigations: build on native runners per OS/arch instead of
  cross-compiling; pin the toolchain in CI; verify a static/prebuilt
  library path; document the build requirement for contributors.
- Not used in v1: libSQL embedded replicas / remote replication.
  Failover is deferred, so these do not justify the cost — they are
  accepted as future upside, not as the reason for the choice.

### D4 — Failover is not in v1, and is not a v1 guarantee

- Reword §5 / §41 accordingly: Hive *may* support best-effort coordinator
  failover as a future deployment capability.
- v1 posture: coordinator is single-authority; recovery = restart;
  nodes keep running agents; execution state is reconcilable.
- Retained primitives (model, not topology): `command_id`, `event_id`,
  `execution_generation`, `node_execution_id`.
- Consequence: `ClusterAuthority`, `ControlJournalEntry`,
  `ControlSnapshot`, and term/lease columns are **not** in v1 migrations.
  `Command.accepted_term` is present but nullable / `0`.
  Connection generation **is** in v1 (stale plugin/node instance fencing),
  but coordinator term fencing is not.

### D5 — Plugin protocol is the Hive Protocol envelope, capability-gated

```text
Hive Protocol
  ├── Control API   (session.*, agent.*, node.*, workspace.*, command.*)
  ├── Node API      (node.*, execution.*, event.*)
  └── Plugin API    (plugin.*, capability.*, lifecycle.*, subscription)
```

- A plugin connection may call only what its registered capabilities and
  role allow. `session.*` / `node.*` / `cluster.*` are not freely callable.
- `hive-plugin-*` depends on `protocol/hive/v1` and must **not** import
  `internal/core`. Enforced by a dependency test.

### D6 — ACP adapter is the compatibility boundary

- Hive core must not version-convert ACP.
- Policy: known+supported → normalize; known-but-unsupported capability →
  explicit unsupported result; unknown method/event → preserve as raw
  protocol data; incompatible ACP version → explicit handshake failure.
- No ACP version is pinned in this plan; see Resolved Decision O1.
  (Resolved: ACP protocol version `1`.)

### D7 — Deterministic prompt routing

```text
session.prompt
  ├── run_id specified            -> target that AgentRun
  └── no run_id
        ├── session.default_interactive_run_id set and non-terminal
        │                            -> target it
        └── otherwise                -> create initial interactive AgentRun
```

- No timestamp / "most recent" routing.
- `default_interactive_run_id` is a **domain field** on Session, not
  metadata.
- Terminal-run behavior is Open Decision O4.

### D8 — Handoff is in v1

Consequences pulled into v1: snapshot versioning (§30),
`HandoffContext` (§16), handoff lifecycle state machine (§15.1), and an
atomic update of `default_interactive_run_id` on handoff **activation**
(not on handoff record creation).

### D9 — Doc hygiene (pending)

`hive-solution-design-v6.md` has two `# 40` sections and skips `# 46`.
Renumber §40–§48 and add a v7 version-history entry. No content change.

------------------------------------------------------------------------

## 3. Resolved Decisions (O1–O7)

All open decisions are resolved. No blockers remain.

| # | Decision | Resolution |
|---|---|---|
| O1 | Which ACP version to pin | **ACP protocol version 1.** Verified against the ACP specification: the version is a single integer MAJOR, bumped only for breaking changes, negotiated in the `initialize` request/response. Hive pins version `1`, negotiates on connect, and fails the handshake explicitly on incompatibility. Capabilities (e.g. `loadSession`) carry non-breaking features. |
| O2 | Same-agent restore (§14) in v1 | **Yes.** Implemented via ACP `session/load`, gated on the agent's `loadSession` capability. When unsupported, fall back to a new runtime session seeded from the Hive context. |
| O3 | Definition of "interactive AgentRun" | **A run that accepts `session.prompt`.** v1 has no non-interactive run kind; all runs are interactive. The concept exists so a future background/one-shot run kind can be excluded from prompt routing without changing the routing rule. |
| O4 | Prompt to a terminal default run | **Create a new AgentRun** using the session's default agent. This is new user intent, not the automatic replacement forbidden by §11.2. A retry of the same `command_id` still resolves to the original target. |
| O5 | Node loopback transport security | **TLS with a locally generated certificate.** The same authenticated path is used on one machine and across machines, so the loopback path is not a special case that can rot. |
| O6 | Slack inbound mode | **Socket Mode.** No public endpoint or inbound tunnel is required for a personal install. |
| O7 | Acknowledgement default (§21.2) | **Off by default, opt-in per transport.** Consistent with §21.2: best-effort, never required for correctness. |

### O1 consequence

ACP `initialize` negotiation is an explicit v1 handshake step, not an
implicit assumption. The adapter must:

- send the latest protocol version it supports (`1`),
- accept the agent's negotiated version only if supported,
- close the connection and report explicitly if the agent returns an
  unsupported version,
- treat omitted capabilities as unsupported (per the ACP spec).

------------------------------------------------------------------------

## 4. Milestones

### M0 — Repository and foundations

- [x] Initialize Go module and repo skeleton aligned with §35
      (`cmd/`, `internal/`, `protocol/`, `plugins/`, `migrations/`, `docs/`)
- [x] `protocol/hive/v1`: JSON-RPC 2.0 envelope, request/response
      correlation, notifications, error model, protocol version fields
- [x] Dependency rule test: `protocol/hive/v1` has no dependency on
      `internal/**` (plus a stdlib-only test)
- [x] TOML config loading with unknown-key rejection and validation
- [ ] Config reference document (moved to M12 docs)
- [x] Structured logging with a stable field schema
- [x] Makefile / task runner (`build`, `test`, `vet`, `fmt`, `tidy`, `run`,
      `clean`, `ci`)
- [x] CI skeleton: build + test on linux/darwin with CGO enabled
- [x] Renumber the design document (D9)

**Exit criteria:** `go build ./...` and `go test ./...` pass in CI on all
target platform pairs; the dependency rule test passes.

Status: met locally. The CI matrix was completed in M1, once libSQL made
the concrete platform set known.

Implementation notes:

- Module path is `github.com/thupham/hive`. Change it before the first
  push if the repository will live elsewhere.
- `plugins/` and the root `migrations/` directory exist but are empty, so
  git does not track them yet. Storage migrations live in
  `internal/storage/migrations/` so they can be embedded.

### M1 — Storage (libSQL)

- [x] Write `docs/adr/0001-storage-libsql.md` recording D3 and its costs
- [x] Migration framework with forward-only, versioned migrations
- [x] Tables: `sessions`, `agent_runs`, `events`, `session_snapshots`,
      `handoffs`, `permission_requests`, `commands`,
      `conversation_bindings`, `workspaces`, `workspace_locations`,
      `plugins`, `plugin_instances`, `capability_registrations`
- [x] Add the `outbox` table required by the §7.1.2 transaction boundary
- [x] Explicitly omit `cluster_authority`, `control_journal`,
      `control_snapshot` in v1 migrations (D4)
- [x] Repository operations over `database/sql`, with an explicit `Execer`
      so writes compose into a single transaction
- [x] Event store: append, idempotent by `event_id`, assign authoritative
      session-scoped monotonic `sequence`
- [x] Command store: durable command record, dedupe by `command_id`
- [x] Snapshot store with an explicit `schema_version` (§30)
- [x] Single-transaction boundary for command + domain mutation + outbox
      record (§7.1.2)
- [x] CI matrix covering the platforms the libSQL driver supports
- [x] Tests: `event_id` idempotency, sequence monotonicity under
      concurrency, command dedupe, transaction atomicity on failure

**Exit criteria:** §42 "Event idempotency", "Command idempotency", and
"Cursor correctness" (storage half) have passing tests.

Status: met. `make ci` is green with 51 passing tests; the suite is clean
under `-race`.

Implementation notes:

- Minimal session persistence was added (`InsertSession`, `GetSession`)
  because `events` references `sessions`. Session state machines are M2.
- `Store.Tables` and `Store.MigrationVersion` exist for operations and the
  future TUI storage view.
- Write transactions are serialized in-process and issued as
  `BEGIN IMMEDIATE`. The coordinator is the single writer for its database,
  so this keeps sequence assignment correct without distributed
  coordination.
- `journal_mode=WAL` is applied once per database, not per connection: it
  is a persistent database property and changing it takes a lock.
- The driver executes one statement per call, so migrations are split
  explicitly in `internal/storage/sqlsplit.go`, which has direct
  white-box tests in `internal/storage/sqlsplit_test.go`.
- The CI matrix includes `ubuntu-24.04-arm`, which requires GitHub arm64
  hosted runners (free for public repositories). Drop it if the repository
  cannot use them.

### M2 — Domain: Session, AgentRun, Command

- [x] Session state machine: `active`, `idle`, `handoff`, `archived`
- [x] AgentRun state machine: `created`, `queued`, `starting`, `running`,
      `completed`, `cancelled`, `failed`, `interrupted`
- [x] Domain-validated transitions only; no arbitrary state strings
- [x] Session field `default_interactive_run_id` (domain field)
- [x] Define "interactive AgentRun" (O3) and encode it
- [x] Prompt routing resolution per D7, including terminal-run rule (O4)
- [x] `execution_generation` and `node_execution_id` on AgentRun
- [x] Command lifecycle: `received`, `accepted`, `completed`, `rejected`,
      `failed`, `retryable`
- [x] Command status resource at the domain and storage level
- [x] Tests: routing determinism, retry with same `command_id` returns the
      same logical result, completed AgentRun does not destroy Session,
      execution fencing rejects older generation

**Exit criteria:** §42 "Session continuity", "Command idempotency",
"Execution fencing", and "Agent isolation" have passing tests.

Status: met. `make ci` is green with 95 passing tests; the suite is clean
under `-race`.

Implementation notes:

- **Dependency direction changed.** Domain packages now own the entity
  types, and `internal/storage` is an adapter over them. This is the
  opposite of the M1 arrangement, where storage owned the types. It was
  changed now because every later milestone (event bus, agent runtime, node,
  transports) would otherwise have to import the database package.
  `internal/storage/types.go` was removed; the entities moved to
  `internal/agent`, `internal/command`, `internal/event`, and
  `internal/session`.
- New packages: `internal/agent`, `internal/command`, `internal/event`,
  `internal/session`.
- `interrupted -> starting` is deliberately absent from the AgentRun
  transition table. Only `Recover` can make that move, and it increments
  `execution_generation`. This is how §11.3 is encoded: lease expiry
  classifies a run as interrupted without authorizing automatic
  replacement, because the original execution may still be alive.
- `archived -> active` is allowed so a session can be unarchived; every
  other transition out of `archived` is rejected.
- Prompt routing takes a lookup function instead of a repository, so routing
  is pure and depends on nothing but the session and the runs.
- AgentRun persistence was added to storage so session continuity and
  generation fencing are covered end to end.
- The `command.get` protocol method itself is a Control API concern and
  lands in M7. M2 provides the durable record plus `IsTerminal`/`IsPending`.

### M3 — Event architecture

- [x] Event envelope per §7.2 (with `origin_node` retained for future
      reconciliation)
- [x] In-process event bus with bounded queues; a slow subscriber cannot
      block the agent execution path (§8)
- [x] Separate realtime path and durability path
- [x] Core event types: `message`, `status`, `run.started`, `run.finished`,
      `permission.requested`, `permission.responded`, `handoff.created`,
      `error`, `agent.raw`
- [x] `agent.raw` preservation for unknown protocol data (§2.6, §7.3)
- [x] Stream cursor + replay
- [x] `cursor_expired` response carrying the latest snapshot boundary and
      next valid sequence (§8.3)
- [x] Durable per-session prune watermark so a fully pruned stream is not
      mistaken for an empty one
- [x] Retention pruning that never drops an unpublished event
- [x] Tests: duplicate event, replay after retention gap, at-least-once
      consumer idempotency

**Exit criteria:** §42 "Unknown protocol data", "Event reconciliation",
and "Cursor correctness" (replay half) have passing tests.

Status: met. `make ci` is green with 113 passing tests; the suite is clean
under `-race`.

Implementation notes:

- **Publication order.** §8 describes the bus as receiving an event before
  persistence, while §7.1.2 requires the authoritative event to commit in the
  same transaction as the command that caused it. v1 resolves that in favour
  of §7.1.2: events are durable first and published from the outbox
  afterwards. A subscriber that has seen an event can therefore always find it
  in the store, and a crash between publish and mark simply republishes.
  Publication stays at-least-once; consumers deduplicate by event id.
- A subscriber whose bounded queue is full drops the event and is reported as
  lagging via `Subscription.Lagged`/`Dropped`. It recovers by replaying from
  its durable cursor. `Publish` never blocks.
- **Migration 0002 added `stream_watermarks`.** Without a durable prune
  watermark, a stream whose events were all pruned looks identical to an empty
  stream, so a stale cursor would be served an empty result instead of
  `cursor_expired`. `PrunedThrough` is the replay decision input;
  `EarliestSequence` is introspection only.
- Pruning only removes events that have already been published. Dropping an
  unpublished event would lose realtime delivery that has not happened yet.
- Retention is implemented as a store capability with tests; scheduling it is
  wiring work that lands with the daemon in M6/M7.

### M4 — Agent Runtime and ACP adapter

- [x] `AgentRuntime` interface per §12/§36, kept small
- [x] Execution identity/state reporting types for start reconciliation:
      `absent`, `starting`, `running`, `terminal`, `ambiguous`, plus an
      optional `Reconciler` runtime capability
- [x] ACP version policy per D6: capability negotiation, raw preservation,
      explicit handshake failure
- [x] Pin ACP protocol version 1 in the adapter contract
- [x] Generic agent profiles in config (`[agents.<name>]` with `protocol`,
      `command`, or `endpoint`); no vendor logic in core (delivered in M0)
- [x] Same-agent restore via ACP `session/load` (O2 = yes)
- [x] Permission relay: opaque payload, one-time terminal transition
      (first-wins CAS), fail-closed on unrecoverable state (§17)
- [x] Tests: permission idempotency, concurrent permission responses,
      coordinator loss does not become implicit approval, unsupported ACP
      version fails the handshake, unknown protocol data survives

**Exit criteria:** §42 "Permission idempotency", "Permission safety", and
"Unknown protocol data" have passing tests.

Status: met. `make ci` is green with 144 passing tests; the suite is clean
under `-race`.

Scope adjustments, stated explicitly:

- The `hive-plugin-acp` binary moves to M5. A plugin binary needs the Plugin
  API and the supervisor to talk to, so the adapter is delivered here as a
  library and M5 wires the process.
- The ACP adapter lives at `plugins/acp`, not under `internal/`, so the plugin
  boundary is structural rather than a convention. A test asserts it imports
  no internal package.
- `ExecutionState` and `Reconciler` are delivered here; the reconciliation
  logic that consumes them belongs to the node execution controller in M6, so
  the §42 "AgentRun start reconciliation" tests land there.

Implementation notes:

- Method names, parameter shapes, and the permission option kinds were taken
  from the ACP v1 schema, not invented: `initialize`, `session/new`,
  `session/load`, `session/prompt` (returns `stopReason`), `session/cancel`,
  `session/update`, and `session/request_permission`.
- Hive advertises no filesystem, terminal, or elicitation capability, because
  the agent owns tool execution. An inbound call for one of those methods is
  answered with a JSON-RPC method-not-found error rather than being pretended.
- The adapter resolves an approval into an ACP outcome using the options the
  agent actually offered. If nothing matches, the request is answered
  `cancelled`: the agent aborts instead of proceeding on an approval the user
  never gave.
- When the permission queue is full, the adapter answers `cancelled`
  immediately. Leaving a permission request unanswered, or answering it
  `selected`, would be worse than failing closed.
- `permission.Decision` carries an optional `OptionID` so a transport that
  renders the agent's own options can answer with exactly one of them.

### M5 — Plugin supervisor and process boundary

- [x] Lifecycle: spawn, handshake, ready, serve, shutdown (§19)
- [x] Stable `plugin_id` vs plugin process instance (§18.3)
- [x] Connection generation per plugin instance; a superseded instance
      cannot invoke capabilities
- [x] Capability authorization enforced in the **core**, not in the plugin
- [x] Plugin API event subscription surface: subscribe / deliver / ack,
      capability-gated (§22)
- [x] Crash detection + restart policy; plugin crash does not terminate the
      coordinator or unrelated sessions
- [x] `hive-plugin-acp` binary (deferred from M4) plus a plugin SDK
- [x] Tests: plugin crash isolation, stale instance rejection, unauthorized
      capability invocation is denied, subscription is capability-gated and
      scoped

**Exit criteria:** §42 "Plugin isolation" and "Connection fencing" (plugin
half) have passing tests.

Status: met. `make ci` is green with 165 passing tests across 16 packages;
the suite is clean under `-race`.

Implementation notes:

- The Plugin API and the request/response correlator (`v1.Peer`) live in
  `protocol/hive/v1`, not under `internal/`, so a plugin uses them without
  linking the core. `plugins/sdk` is the plugin-side helper, and a test asserts
  it imports no internal package.
- Authorization is a core-side decision. A plugin declares what it wants; the
  core intersects that with what its plugin type may ever hold, and every
  inbound call is checked against the granted set. Registration is descriptive
  and never a grant.
- `Authorize` also rejects an instance that is no longer current for its plugin
  identity, which is what stops a superseded connection from invoking
  capabilities after a restart.
- Event delivery is at-least-once and driven by the acknowledgement cursor. A
  plugin that cannot keep up loses realtime delivery and recovers from the
  durable store, and re-authorization on each delivery drops a superseded
  instance.
- `plugins/acpbridge` is the composition point: the core speaks the Plugin API
  to it and it speaks ACP to the agent, so neither side learns the other's
  protocol. It reports `absent` when a start side effect did not happen, so the
  core can reconcile rather than start a second execution.

Deliberately not in M5, stated so it is not mistaken for done:

- `plugin.drain` is defined but draining is not implemented: `Stop` sends
  `plugin.shutdown` and closes the link. Graceful drain belongs with the daemon
  lifecycle in M6/M7.
- Plugin trust is "trusted local process", as §18.3 requires the documentation
  to admit. Enrollment, revocation, and key rotation are not implemented, and
  the `plugins` / `plugin_instances` / `capability_registrations` tables are not
  yet written to.
- The subscription cursor is in-memory. Durable per-binding cursors land in M8,
  where `conversation_bindings` gives a binding to attach a cursor to.

### M6 — Coordinator and node (child process)

- [x] Node Protocol over WebSocket with TLS and a locally generated
      certificate (O5)
- [x] Node API domains: `node.*`, `execution.*`, `event.*`
- [x] Multiplexed single bidirectional channel (heartbeat, execution
      commands, sync)
- [x] Node-local event buffer with stable event ids
- [x] Reconnect synchronization: identity, connection generation, active
      executions, buffered event ids; idempotent
- [x] Execution lease as a liveness signal only; expiry reports the run as
      interrupted and starts **no** replacement execution
- [x] Stale connection generation rejected; a new authenticated connection
      invalidates the previous one
- [x] `hive` role selection (`coordinator`, `node`, `auto`) and spawning the
      node child process
- [x] Node-local database for execution evidence and the event buffer
- [x] Node executor wired to the plugin supervisor, so a node runs an ACP
      agent
- [x] Coordinator composition: event ingest, permission ingest, execution
      reports, lease sweep
- [x] Tests: reconnect creates no duplicate execution, lease expiry reports
      without replacing, stale generation rejected, sync requests only
      missing events, full stack end to end

**Exit criteria:** §42 "Reconnect safety" and "Execution fencing" have
passing tests; killing the node child does not kill the coordinator.

Status: met. `make ci` is green with 175 passing tests; the suite is clean
under `-race`. One `hive serve` brings up the coordinator, a node child
process, and the agent plugin, and they connect and synchronize.

Verified by running it, not only by tests:

```text
coordinator listening url=wss://127.0.0.1:58848/node
node child started pid=80419
node starting node=... agent=claude plugins=1
plugin ready plugin=claude type=agent instance=claude#1 generation=1
node connected node=... generation=1
node synchronized generation=1 active=0 buffered=0
```

Implementation notes:

- The link uses TLS with a locally generated certificate even on one machine,
  so the loopback path is not a special case that can rot while only the
  multi-machine path is exercised.
- The handshake is an ordinary request/response (`node.hello` /
  `node.ready`), not a bootstrap exchange outside the peer, so the node link
  reuses the same correlator as every other connection.
- A new authenticated connection increments the connection generation and
  invalidates the previous one, so a delayed packet from the old link cannot be
  treated as a current node link. The superseded `Node` refuses commands.
- The node owns the event buffer and the durable upload, so it assigns the
  stable event id that survives a replay. The id carries a per-process random
  prefix: a bare counter would restart at 1 after a node restart and a new event
  could be deduplicated as a duplicate of an old one.
- An event is buffered durably **before** the upload is attempted, so a node
  never loses an event merely because the coordinator is unreachable. A failed
  upload is not an error for the agent; the buffer replays.
- Permission requests are deliberately **not** buffered. A request that cannot
  be relayed must fail closed rather than be answered from a stale local view.
- Lease expiry classifies a run as interrupted and never starts a replacement.
  `WatchLeases` reports every expired execution on every tick because the
  transition it drives is already idempotent.
- Composition lives in `internal/daemon`, not in `main`, so the whole stack is
  testable in process.

One domain rule changed while wiring this up, and it is worth recording:
`starting` may now transition straight to `completed`. The coordinator's view is
derived from what the node reports, and a "running" report can be coalesced away
or lost while the terminal report survives. Refusing the terminal report would
have left the run stuck in `starting`, which is worse than accepting that it
finished quickly. The previous table was too strict and the end-to-end test
caught it.

Deliberately not in M6:

- Remote nodes and node enrollment. v1 runs the node on the same machine and
  shares the locally generated certificate, so node identity enrollment,
  revocation, and key rotation are not implemented. §18.2 requires the
  documentation to say so.
- Coordinator failover, and therefore any term or connection-generation fencing
  beyond the node link's own generation.

### M7 — Control API and gateway

- [x] Control API domains: `session.*`, `agent.*`, `node.*`, `command.*`
      (`workspace.*` lands with M11)
- [x] Authentication: owner principal model, `allowed_users` (§18.1)
- [x] Authorization evaluated before accepting a mutating operation; deny by
      default
- [x] An unverified client-supplied principal string is never treated as
      identity
- [x] `source_id` → `command_id` dedupe at the authoritative domain boundary
- [x] Unix socket endpoint for the local CLI/TUI, owner-only
- [x] Session orchestration: create a session and its run, start the
      execution on a node, route prompts deterministically
- [x] Tests: session isolation, authentication and authorization, duplicate
      inbound delivery yields one command, new user message yields a new
      command

**Exit criteria:** §42 "Authentication and authorization", "Session
isolation", and "Command idempotency" have passing tests.

Status: met. `make ci` is green with 197 passing tests; the suite is clean
under `-race`. Verified against a running daemon, not only in tests:

```text
$ hive serve
coordinator ready url=wss://127.0.0.1:60582/node agents=1 control=.../hive.sock
$ # over the owner-only unix socket
agent.list      -> {"agents":[{"id":"claude","protocol":"acp"}]}
node.list       -> {"nodes":[{"nodeId":"...","connected":true,...}]}
session.create  -> {"sessionId":"sess_...","runId":"run_...","agentId":"claude",...}
```

Implementation notes:

- The Control API shares method names with the plugin-invokable set. The same
  operation reached over a different transport is the same operation, so
  `session.create` means one thing whether a client or a transport plugin asks
  for it.
- Authorization runs before the handler, so a denied caller cannot reach one.
- A principal is resolved from the **connection**, not from the message. A
  trusted transport may assert a principal per message, but only one prefixed
  with its own transport name, so a client-supplied string never grants
  authority on its own.
- A non-owner reaches a session only through a binding it holds, and an
  unreachable session is reported as not-found rather than unauthorized, so a
  principal cannot learn that a session it cannot reach exists.
- `session.create` commits the session, the run, and the command in one
  transaction, then starts the execution as a post-commit side effect. When the
  agent cannot be started the session and run remain durable and reconcilable
  instead of leaving a half-created operation. The smoke test above exercises
  exactly that path, because `claude` is not installed on the test machine.
- The created run becomes the session's default interactive run, so the first
  prompt routes to it rather than creating a second run.
- Two bugs were found and fixed while wiring this up:
  - `startRun` persisted the whole AgentRun row from an in-memory copy whose
    state was still `created`, silently overwriting the `starting` state the
    node had just reported. Read-modify-write now happens inside one
    transaction (`UpdateAgentRunWith`), because a run's state and generation are
    written by the node, the lease sweep, and the control plane.
  - Storage errors were surfaced as internal failures instead of not-found,
    because `v1.AsError` does not know about `storage.ErrNotFound`. The mapping
    now lives in one place, `internal/apierr`.
- The unix socket path is bounded by the platform's `sockaddr_un`, so
  `ListenUnix` rejects an over-long path with a message that names the cause.

Deliberately not in M7:

- Transport-neutral normalization (`IncomingMessage` / `IncomingCommand` /
  `IncomingInteraction`) and the `source_id` → `command_id` derivation belong to
  the transport layer and land in M8. M7 delivers the boundary those normalize
  into: the same command id resolves to the same logical operation.
- `session.handoff` is M9. `workspace.*` is M11.

### M8 — Slack transport plugin

- [x] `hive-plugin-slack` as a separate process; imports no internal package
- [x] Inbound via Socket Mode (O6), so no public endpoint or inbound tunnel is
      needed
- [x] `source_id` from the Slack channel and timestamp; delivery dedupe
- [x] Command grammar: `@Hive /new_chat` to `session.create`, plus `/agents`,
      `/nodes`, `/status`, `/cancel`, and the bare-name form
- [x] Ordinary message to `session.prompt`; a recognized command is never
      silently forwarded as a prompt
- [x] Interactive controls: `[Allow]` / `[Deny]` to `permission.respond`
- [x] Outbound rendering to Block Kit from Hive events
- [x] Durable conversation binding, resolved from the coordinator rather than
      remembered across a restart
- [x] Optional acknowledgement reaction per 21.2 (O7): best-effort, and its
      failure never fails the operation
- [x] Tests: the Phase 3 minimum set, parsing, rendering, and the transport
      boundary end to end through the plugin link

**Exit criteria:** §42 "Transport command correctness" and "Message
acknowledgement correctness" have passing tests.

Status: met, with one part stated honestly below. `make ci` is green with 218
passing tests; the suite is clean under `-race`.

Verified by running it: a transport plugin that cannot start does not take the
coordinator down.

```text
plugin ready plugin=slack type=transport instance=slack#1 capabilities=7
transport plugin ready plugin=slack
coordinator ready url=wss://127.0.0.1:61812/node agents=1
node connected node=... generation=1
$ # the Control API still answers while the transport is dead
node.list -> {"nodes":[{"nodeId":"...","connected":true,...}]}
hive-plugin-slack: slack: an app token is required for Socket Mode
```

Implementation notes:

- The envelope types live in `protocol/hive/v1`, not under `internal/`. A
  transport plugin builds them, and a plugin does not link the core. The same
  applies to the core event type names, so `internal/event` aliases the protocol
  constants and they cannot drift.
- The transport resolves its own aliases: `/new_chat` is a presentation-level
  name and `session.create` is the operation. Hive core contains no Slack command
  registry, and the discoverable catalog is generated from the transport's own
  command map so the two cannot diverge.
- A recognized command the transport does not expose carries an empty method,
  which is reported as a command error rather than forwarded to the agent as a
  prompt. Conversely, text that merely contains a slash-like token stays a prompt,
  because only the first token is examined.
- A conversation with no session yet gets one when a message arrives, because the
  first thing a user says has to land somewhere. That is a routing decision, not
  an accident of receiving text.
- A transport speaks only for its own transport, and may assert only a principal
  of that transport. Both are enforced in the coordinator before routing.
- A delivery that no node can serve is refused rather than creating a session
  nothing can run.
- Transport options are opaque key/value pairs passed to the plugin, so Hive
  configuration holds no vendor-specific fields.

Deliberately not in M8, stated so it is not mistaken for done:

- **The live Socket Mode path is implemented but not exercised.** It needs a
  Slack app token and bot token, which a test environment does not have. Parsing,
  routing, rendering, and the boundary are covered by tests and by the end-to-end
  transport test; the WebSocket connection to Slack itself is not.
- The durable per-binding `event_cursor` is not yet advanced from the delivery
  path. `AdvanceBindingCursor` exists and is tested at the storage level, and the
  plugin acknowledges through the in-memory subscription cursor from M5. Wiring
  the binding cursor to the acknowledgement belongs with M10, where the CLI needs
  the same replay guarantees.
- `/handoff` is M9.

### M9 — Handoff and snapshots

- [x] Snapshot creation with `schema_version`; a snapshot is not a replacement
      for the event log (30)
- [x] `Context`: summary, recent events, prior messages, workspace ref,
      artifacts (16); the summary is optional
- [x] Handoff lifecycle: `requested`, `context_building`, `target_starting`,
      then `active` / `failed` / `cancelled` (15.1)
- [x] Handoff record and target AgentRun assignment idempotent by
      `command_id`
- [x] Source and target `runtime_session_id` are always distinct fields
- [x] `default_interactive_run_id` updates on handoff **activation**, not on
      record creation (D8)
- [x] A handoff is not reported successful unless the target execution started
- [x] The transport exposes `/handoff`
- [x] Tests: no false success, distinct runtime session ids, retry creates no
      second handoff, a failed handoff leaves the session usable

**Exit criteria:** §42 "Handoff correctness" and "Handoff recovery" have
passing tests.

Status: met. `make ci` is green with 229 passing tests; the suite is clean under
`-race`.

Implementation notes:

- A handoff creates a new AgentRun. Hive never converts one runtime's native
  session into another's, and the two `runtime_session_id` values are separate
  fields on separate runs. The end-to-end test asserts they differ.
- The handoff record and the target run commit in one transaction, so a retry
  cannot create a second handoff. Everything after that is an explicit lifecycle
  step, so a crash leaves an inspectable state instead of a silent claim that the
  session moved.
- The routing pointer moves only on activation, after the target execution has
  started. A prompt arriving before that still reaches the source run, which can
  serve it.
- While the transfer is in progress the session is in `handoff`, so routing does
  not send new prompts to a run that is being replaced. A failed handoff returns
  the session to `active`.
- The context package is stored as a versioned snapshot, so a later context schema
  can migrate or reject it explicitly.
- The transport maps `/handoff <agent>` to `session.handoff`; the alias stays in
  the transport.

Two real bugs were found while wiring this up, both worth recording:

- **A lost update, for the third time.** Building the context package persisted
  the whole handoff row from a stale in-memory copy, undoing the state transition
  that had just been written. The targeted update (`UpdateHandoffWith`) is the fix,
  and the pattern is already recorded in AGENTS.md: read-modify-write belongs
  inside one transaction.
- **The node ignored which agent a run belongs to.** `pluginExecutor` dispatched
  every execution to the first configured plugin, so a handoff to another agent
  still ran on the old one. The execution surface now names the agent, and the node
  picks the plugin by that name. This was invisible until a session had two agents.

Deliberately not in M9:

- Cancelling an in-flight handoff. The `cancelled` state exists in the lifecycle
  and is reachable by transition, but no operation drives it yet.
- The target agent does not yet receive the context package as a prompt. The
  package is built, versioned, and stored; feeding it to the target run's first
  prompt is part of the runtime wiring, together with the CLI in M10.

### M10 — CLI and TUI

- [x] A Control API client, so the CLI and the TUI are clients of Hive APIs
      rather than independent orchestration logic
- [x] CLI commands: `version`, `config`, `serve`, `tui`, `session
      create|list|status|prompt|cancel|handoff|events`, `agent list`,
      `node list`, `command get`
- [x] An explicit `-command-id` on mutating commands, so a caller can retry the
      same logical operation deliberately
- [x] `session events` reports a pruned cursor explicitly instead of skipping
      history silently
- [x] TUI management plane: overview and session detail, rendering once or on
      an interval
- [x] Durable transport binding cursor advances with the acknowledgement
- [x] Handoff context reaches the target agent as a preamble to its first prompt
- [x] Tests: the client surface end to end over the socket, the management plane
      rendering, and the binding cursor

**Exit criteria:** a full flow is drivable from the CLI with no core logic in the
client.

Status: met. `make ci` is green with 234 passing tests; the suite is clean under
`-race`. Verified by running it:

```text
$ hive version
hive 0.1.0-dev
hive protocol hive/1.0

$ hive agent list
claude               acp

$ hive node list
Thus-MacBook-Pro.local   connected  generation 1  0 executions  0.1.0-dev

$ hive session create
session: sess_b1d8d6dd8a860f4b
run:     run_305870203fb38fd5
agent:   claude
node:    Thus-MacBook-Pro.local
command: cmd_b31f7baa139d552e

$ hive session list
sess_b1d8d6dd8a860f4b  active    1 runs  updated 2026-09-23T05:00:34Z

$ hive tui
Hive

● 1 of 1 nodes
● 1 agents
● 1 sessions
○ 0 pending permissions
```

Implementation notes:

- The CLI and the TUI share one client package, so neither contains orchestration
  logic. The CLI generates a fresh command id per invocation, because an
  invocation is a new logical operation; `-command-id` is what a caller passes
  when retrying the same one.
- `session events` surfaces `cursor_expired` on stderr with the snapshot boundary
  and exits non-zero, rather than pretending the stream was empty.
- The TUI renders a dashboard rather than owning the terminal. Interactive
  navigation is not implemented, and the management plane is still useful as a
  refreshed view.
- The handoff context is rendered to text by the core and travels with the target
  start, so the agent adapter stays protocol-agnostic. The ACP adapter prepends it
  to exactly one prompt, because ACP has no field for seeding a session.
- The binding cursor moves only when the transport durably accepted the event for
  its own delivery workflow, which is what lets a restarted transport resume.

A serious bug was found by running the CLI, not by a test:

- **`serve` dropped the flags that followed it.** A spawned node child is started
  with `-role node -coordinator-url ...`, and those arguments were parsed but never
  applied, so the child started as a second coordinator and spawned a node child of
  its own. That is an unbounded chain of processes. `serve` now applies the global
  flags, and a coordinator refuses to spawn a node child when it is itself one.

Deliberately not in M10:

- Interactive TUI navigation and in-place terminal control.
- The `session handoff` CLI does not yet show the pending handoff while it is in
  flight; the operation is synchronous from the caller's point of view.

### M11 — Workspace

- [x] `Workspace` identity plus `WorkspaceLocation` (`node_id`, `path`)
- [x] A workspace is not a Git abstraction: Hive owns no checkout, branch,
      worktree, merge, or rebase
- [x] Naming a workspace on a session resolves it, and registers it on first use
- [x] The run works at the location registered for the node it runs on
- [x] Shared-location warning when several active runs reference one location
- [x] The warning is never a lock, and a finished run does not overlap with
      anything
- [x] `workspace.create` and `workspace.list` over the Control API and the CLI
- [x] Tests: the same name resolves to the same workspace, a shared location
      warns without locking, a terminal run produces no warning

**Exit criteria:** §42 "Agent isolation" is unaffected by concurrent runs sharing
a workspace.

Status: met. `make ci` is green with 245 passing tests; the suite is clean under
`-race`.

Implementation notes:

- A workspace is identity; a location is where it lives on one node. The same
  workspace may live at different paths on different machines, so the path belongs
  to the pair rather than to the workspace.
- The warning is computed from active runs only. The terminal predicate comes from
  the domain rather than being restated in SQL, so a run cannot be "active" in one
  place and terminal in another.
- Warnings are sorted, so the management plane does not flicker between refreshes.
- The shared-location test asserts both directions: the warning appears, and the
  runs and sessions are untouched. A warning that quietly became a lock would fail
  it.

### M12 — Release and open-source quality bar

- [ ] Cross-platform release builds with CGO enabled
      (linux/darwin, amd64/arm64) via native runners (D3)
- [ ] Document the CGO toolchain requirement for contributors
- [ ] Docs: protocol specification, plugin specification, security model,
      configuration reference, failure semantics, cursor
      retention/replay procedure
- [ ] Explicitly separate guaranteed / best-effort / optional / future in
      the docs (§48)
- [ ] Example plugin and example custom client
- [ ] Failure-injection suite for the v1-applicable cases (§41 Phase 6
      subset): duplicate command, duplicate event, node disconnect,
      plugin crash, node reconnect
- [ ] Full §42 correctness-property suite green

**Exit criteria:** the §48 quality-bar list is satisfied for the v1
subset, with deferred items explicitly marked.

------------------------------------------------------------------------

## 5. Dependency Order

```text
M0 -> M1 -> M2 -> M3 -> M4 -> M5 -> M6 -> M7 -> M8 -> M9 -> M10 -> M11
                                     \-> M9 (handoff needs node + runtime)
M12 runs continuously from M1 (CI, docs) and closes at the end
```

M11 can be done any time after M2. M10 can start once M7 exposes the
Control API.

------------------------------------------------------------------------

## 6. Notes

- O1–O7 are resolved; see §3. Their blocking milestones are unblocked.
- Any task that cannot be completed must stay unchecked with a note, not
  be silently dropped.
- The design document remains the source of truth for semantics; this plan
  only sequences the work and records the v1 cut line.
