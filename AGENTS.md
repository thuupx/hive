# AGENTS.md

Project conventions for the Hive repository.

## Source of truth

See `/docs` for the design.


## Commands

```sh
make build   # go build -o bin/hive ./cmd/hive
make test    # go test ./...
make vet     # go vet ./...
make fmt     # go fmt ./...
make tidy    # go mod tidy
make run     # build, then run the daemon
make ci      # vet + test + build
make dist    # package this host's binaries as a release tarball
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

### Releases

- A release is a tag: pushing `v*` runs `.github/workflows/release.yml`. The
  process, and what is deliberately not done yet, is in `docs/releasing.md`.
- CGO cannot be cross-compiled, so each platform is built on a native runner.
  The release matrix mirrors `ci.yml`; keep the two in step.
- `make dist` packages exactly what a runner uploads. Its `VERSION` is stamped
  in with `-ldflags "-X main.Version=..."`, which is why `Version` in
  `cmd/hive/main.go` is a variable rather than a constant.
- A tarball holds `hive` and both plugins. The daemon resolves plugins from its
  own directory, so a tarball with `hive` alone is a gateway with no agents.
- `checksums.txt` is written once, by the release job, after every artifact is
  downloaded. Do not compute it per runner: one file assembled from several
  sources is one that can disagree with itself.

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

### Process names

- Every role runs the same binary, so an operating system reports them all by
  the executable's name. A role-named alias next to the binary is what tells
  them apart: `hive-coordinator`, `hive-node`, `hive-transport-<name>`,
  `hive-agent-<name>`. `cmd/hive/procname.go` owns this.
- The alias is a hard link, so it reads as the role from the executable path,
  the command name, and the command line alike; a symbolic link does not,
  because a process monitor resolves it back to the real binary. A hard link
  needs no privilege on any platform, and a platform that refuses it is not an
  error: the process keeps the real binary's name.
- A process name is fixed when the process starts, so the supervisor has to
  start the alias: the LaunchAgent and the systemd unit run `hive-coordinator`
  (or `hive-node` for a standalone node). `hive service restart` rewrites the
  definition so a name or role change takes effect without touching secrets.

### Workspaces

- A run must have a working directory, and it is never the process's own: a
  service starts in the filesystem root, so falling back to it hands the agent
  every file the user can reach. An empty `workspace_dir` means
  `~/.hive/workspace`, which Hive owns and creates. `workspace.Directory` owns
  the rule and refuses when there is nothing to resolve.
- `hive init` writes the directory it was run in, and refuses the root and the
  home directory, because neither is a project.

### Event ownership

- The **node** owns the event buffer and the durable upload, so the node assigns
  the stable event id that survives a replay. The id carries a per-process
  random prefix: a bare counter would restart at 1 after a restart and a new
  event could be deduplicated as a duplicate of an old one.
- An event is buffered durably before the upload is attempted. A failed upload
  is not an error for the agent.
- Permission requests are never buffered: a request that cannot be relayed must
  fail closed.

### CLI

- The standard flag package stops parsing at the first positional argument, so a
  subcommand must call `parseArgsAndFlags`, not `flag.FlagSet.Parse`. A CLI must
  not depend on the caller remembering flag order.

### Errors

- Storage and domain errors are Go errors, not protocol errors. Translate them
  at the API boundary with `internal/apierr.From`, which lives in one place so
  the mapping cannot drift between the Control API, the node link, and the
  plugin link.
- A record a caller may not reach is reported as not-found, not unauthorized, so
  a caller cannot learn that it exists.

### Concurrency

- Read-modify-write must happen inside one write transaction. An AgentRun, a
  Session, and a Handoff are each written by more than one writer, so persisting
  a stale in-memory copy silently overwrites a newer one. Use
  `Store.UpdateAgentRunWith`, `Store.UpdateSessionWith`, or
  `Store.UpdateHandoffWith` rather than loading, mutating, and calling the plain
  `Update...` method. This bug has been introduced three times; the helper exists
  to make it hard to write.
- The execution surface names the agent. A node runs several agents, so
  dispatching to a fixed plugin would silently run the wrong one.

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

The module is `github.com/thuupx/hive`, which is where the repository lives. The
two have to agree for `go install github.com/thuupx/hive/cmd/hive@v1.0.0` to
resolve; nothing else depends on the path.

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
