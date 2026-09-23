# AGENTS.md

Project conventions for the Hive repository.

## Source of truth

- `hive-solution-design-v6.md` — architecture and semantics.
- `hive-v1-implementation-plan.md` — v1 cut line, resolved decisions
  (D1–D9, O1–O7), and milestone tracking.

When semantics are unclear, the design document wins. When sequencing is
unclear, the implementation plan wins.

## Commands

```sh
make build   # go build -o bin/hive ./cmd/hive
make test    # go test ./...
make vet     # go vet ./...
make fmt     # go fmt ./...
make tidy    # go mod tidy
make run     # build, then run the daemon
make ci      # vet + test + build
```

Go is at `/usr/local/go/bin/go` and may not be on `PATH` in a
non-interactive shell.

## Conventions

### Tests

Follow standard Go layout: `*_test.go` files live in the same directory as
the code they test.

- `package foo` for internal (white-box) tests that need unexported
  identifiers.
- `package foo_test` for external (black-box) tests that use only the
  exported API.

Both may coexist in one directory, and that is the normal way to mix API
tests with tests of internal helpers. `internal/storage/sqlsplit_test.go`
is an example of the internal form.

- Test helpers are unexported functions inside the test files.
- Use `t.Helper()`, `t.Cleanup()`, and table-driven subtests via `t.Run`.
- `testdata/` holds fixtures and is ignored by the go tool.
- Do not create a top-level `tests/` directory for unit tests: it makes
  unexported code untestable and breaks per-package test runs. Reserve a
  separate directory for integration tests, and give those files a
  `//go:build integration` tag so `go test ./...` stays fast.

### Events

- Events are durable before they are published. The outbox row is written in
  the same transaction as the command that caused the event (§7.1.2), and
  `eventbus.Publisher` moves published events from the outbox to the bus
  afterwards.
- `Bus.Publish` must never block. A subscriber with a full bounded queue
  drops events and is reported lagging; it recovers from its durable cursor.
- Replay decisions use `Store.PrunedThrough`, never `Store.EarliestSequence`:
  a fully pruned stream has no events left and would otherwise look empty.

### Composition

- `internal/daemon` is the composition root for the coordinator and the node.
  Keep wiring there rather than in `main`, so the whole stack is testable in
  process.
- `cmd/hive serve` resolves the role from `cluster.role` (`auto` means "node if
  a coordinator URL is configured, otherwise coordinator"). A coordinator starts
  a node child process by default, which is the single-machine installation.
- Plugin binaries ship next to the `hive` binary. `HIVE_PLUGIN_DIR` overrides
  the lookup directory, which is what makes `go run` and tests workable.

### Event ownership

- The **node** owns the event buffer and the durable upload, so the node assigns
  the stable event id that survives a replay. The id carries a per-process
  random prefix: a bare counter would restart at 1 after a restart and a new
  event could be deduplicated as a duplicate of an old one.
- An event is buffered durably before the upload is attempted. A failed upload
  is not an error for the agent.
- Permission requests are never buffered: a request that cannot be relayed must
  fail closed.

### Errors

- Storage and domain errors are Go errors, not protocol errors. Translate them
  at the API boundary with `internal/apierr.From`, which lives in one place so
  the mapping cannot drift between the Control API, the node link, and the
  plugin link.
- A record a caller may not reach is reported as not-found, not unauthorized, so
  a caller cannot learn that it exists.

### Concurrency

- Read-modify-write must happen inside one write transaction. An AgentRun's
  state and execution generation are written by the node, the lease sweep, and
  the control plane, so persisting a stale in-memory copy silently overwrites a
  newer one. Use `Store.UpdateAgentRunWith` rather than loading, mutating, and
  calling `UpdateAgentRun`.

### Transports

- A transport owns presentation; Hive owns semantics. A transport resolves its
  own aliases (`/new_chat`) to Hive methods (`session.create`) before handing a
  normalized envelope over. Hive core must never contain a platform command
  registry.
- The transport envelope and the core event type names live in
  `protocol/hive/v1`, because a transport plugin builds them and a plugin does
  not link the core. `internal/event` aliases the protocol event constants.
- A transport speaks only for its own transport and may assert only a principal
  of that transport. Both are enforced in the coordinator before routing.
- Transport options reach the plugin as opaque key/value pairs, so Hive
  configuration holds no vendor-specific fields.

### Layering

Domain packages own the entity types; storage is an adapter over them.

- `internal/agent`, `internal/command`, `internal/event`, and
  `internal/session` define entities, states, and transition rules. They
  must not import `internal/storage`.
- `internal/storage` persists those entities and may import them.
- State transitions are validated by the domain. Storage must never write a
  state string the domain does not define.
- Prompt routing takes a lookup function rather than a repository, so it
  stays pure.

### Storage

- Migrations are embedded SQL files in `internal/storage/migrations/`,
  named `<version>_<name>.sql`, numbered contiguously from 1, and
  forward-only. There are no down migrations.
- Every write goes through `Store.WriteTx`. It serializes writers
  in-process and issues `BEGIN IMMEDIATE`, which is what keeps a session's
  event sequence strictly monotonic.
- The driver executes one statement per call, so multi-statement SQL is
  split by `internal/storage/sqlsplit.go`.
- `journal_mode=WAL` is applied once per database, not per connection.
  Other pragmas are per connection and applied by the connector wrapper.

### Protocol package purity

`protocol/hive/v1` is the public protocol surface. It must:

- import no `internal/` package, and
- depend on the standard library only.

Both rules are enforced by tests in `tests/protocol_v1_deps_test.go`.
Plugins depend on this package without linking the core.

### Module path

The module is `github.com/thupham/hive`. Change it everywhere before the
first push if the repository will live elsewhere.

## Decisions already made

Do not relitigate these without reason; see
`hive-v1-implementation-plan.md` §2–§3.

- Single-machine v1: `hive` spawns the node as a child process and speaks
  the real node protocol over loopback TLS.
- One production transport in v1: Slack. CLI/TUI is a protocol client,
  not a transport plugin.
- Storage: libSQL via `go-libsql`, CGO accepted.
- Coordinator failover is not in v1 and is not a v1 guarantee.
- ACP protocol version 1, negotiated in `initialize`.
