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
- [ ] `hive` role selection and spawning the node child process
- [ ] Node-local database for the event buffer
- [x] Tests: reconnect creates no duplicate execution, lease expiry reports
      without replacing, stale generation rejected, sync requests only
      missing events

**Exit criteria:** §42 "Reconnect safety" and "Execution fencing" have
passing tests; killing the node child does not kill the coordinator.

Status: the node link is met; the daemon wiring is not. `make ci` is green
with 176 passing tests; the suite is clean under `-race`.

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
- `WatchLeases` reports every expired execution on every tick rather than
  deduplicating internally, because the domain transition it drives is already
  idempotent. The callback must not start work.
- Reconnect reconciliation is idempotent by construction: it only reports what
  the coordinator is missing, and replaying a buffered event is deduplicated by
  `event_id` in the store.
- The node's executor is an interface. Tests drive it with a fake, so the link
  is verified independently of any particular agent runtime.

Deliberately not in M6:

- `hive` does not yet select a role or spawn a node child process, so the
  one-command UX from §44 is not delivered. The node link works against a
  listening coordinator.
- The node event buffer is in memory, not in the node database. A node restart
  therefore loses buffered events that were never uploaded. The durable buffer
  belongs with the daemon lifecycle, together with role selection.
- The node's executor is not yet wired to the plugin supervisor, so a node does
  not yet run an ACP agent end to end. That composition is the next step.

### M7 — Control API and gateway

- [ ] Control API domains: `session.*`, `agent.*`, `node.*`,
      `workspace.*`, `command.*`
- [ ] Authentication: owner principal model, `allowed_users` (§18.1)
- [ ] Authorization evaluated before accepting a mutating operation;
      deny by default
- [ ] An unverified client-supplied principal string is never treated as
      identity
- [ ] Transport-neutral normalization: `IncomingMessage` /
      `IncomingCommand` / `IncomingInteraction` → Hive operation
- [ ] `source_id` → `command_id` dedupe at the authoritative domain
      boundary
- [ ] Unix socket endpoint for the local CLI/TUI
- [ ] Tests: session isolation, authentication/authorization, duplicate
      inbound delivery yields one command, new user message yields a new
      command

**Exit criteria:** §42 "Transport isolation", "Authentication and
authorization", and "Transport command correctness" (normalization half)
have passing tests.

### M8 — Slack transport plugin

- [ ] `hive-plugin-slack` as a separate process; depends on
      `protocol/hive/v1` only
- [ ] Inbound via Socket Mode (O6)
- [ ] `source_id` = Slack event/message ID; delivery dedupe
- [ ] Command grammar: `@Hive /new_chat` → `session.create`, plus
      `/agents`, `/status`, `/cancel`, `/handoff`
- [ ] Ordinary message → `session.prompt`; a recognized command is never
      silently forwarded as a prompt
- [ ] Interactive controls: `[Allow]` / `[Deny]` → `permission.respond`
- [ ] Outbound rendering to Block Kit from Hive events
- [ ] Durable transport binding + `event_cursor`; resume after plugin
      restart
- [ ] Optional acknowledgement reaction per §21.2 (O7): capability
      metadata, best-effort, failure never fails the operation
- [ ] Tests — the §41 Phase 3 minimum set plus acknowledgement:
      - [ ] ordinary message → `session.prompt`
      - [ ] `/new_chat` → `session.create`
      - [ ] `@Hive /new_chat` → `session.create`
      - [ ] unknown command → transport-level command error
      - [ ] duplicate inbound event → same `command_id`
      - [ ] new repeated user message → new `command_id`
      - [ ] button interaction → correct Hive operation
      - [ ] acknowledgement enabled → reaction attempted
      - [ ] acknowledgement unsupported → message still processes
      - [ ] acknowledgement failure → message still processes

**Exit criteria:** §42 "Transport command correctness" and "Message
acknowledgement correctness" have passing tests.

### M9 — Handoff and snapshots

- [ ] Snapshot creation with `schema_version`; snapshot is not a
      replacement for the event log (§30)
- [ ] `HandoffContext`: summary, recent events, workspace ref, artifacts
      (§16); summary is optional
- [ ] Handoff lifecycle: `requested`, `context_building`, `target_starting`,
      then `active` / `failed` / `cancelled` (§15.1)
- [ ] Handoff record + target AgentRun assignment idempotent by
      `command_id`
- [ ] Source and target `runtime_session_id` are always distinct fields
- [ ] `default_interactive_run_id` updates atomically on handoff
      **activation**, not on record creation (D8)
- [ ] Handoff is not reported successful unless the target execution and
      required context are durably established
- [ ] Tests: crash mid-handoff leaves an inspectable incomplete state; no
      false success; handoff correctness never reinterprets the source
      runtime session ID

**Exit criteria:** §42 "Handoff correctness" and "Handoff recovery" have
passing tests.

### M10 — CLI/TUI

- [ ] CLI client over the Unix socket, using the Hive Protocol
- [ ] TUI management views: Sessions, Agents, Nodes, Plugins, Events,
      Config, Logs (Security can be minimal in v1)
- [ ] Session detail: workspace, current agent, node, run list,
      `[Handoff]` `[Cancel]` `[Open]`
- [ ] No orchestration logic in the CLI/TUI — it is a client of Hive APIs
      (§32)
- [ ] Tests: CLI performs handoff through the protocol only

**Exit criteria:** a full flow (create session, prompt, handoff, permission
response) is drivable from the TUI with no core logic in the client.

### M11 — Workspace

- [ ] `Workspace` + `WorkspaceLocation` (`node_id`, `path`)
- [ ] Shared-location warning when multiple active AgentRuns reference the
      same location; never a lock (§23, §24)
- [ ] Tests: warning is emitted, no lock or serialization is introduced

**Exit criteria:** §42 "Agent isolation" is unaffected by concurrent runs
sharing a workspace.

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
