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

### Tests live in `tests/`

All tests live in `tests/` as a single external test package
(`package tests`), not next to the code they exercise. Files are named
`<area>_test.go`, with the protocol package prefixed `protocol_v1_`.

Consequences to respect when writing tests:

- Only exported identifiers are reachable. Do not plan on white-box
  testing of unexported state.
- Struct literals of another package's types must use keyed fields, or
  `go vet` fails.
- A test that inspects package source rather than importing it (for
  example the protocol dependency-boundary test) must address the
  directory by relative path.
- `go test ./internal/...` runs no tests. Use `go test ./tests`.

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
