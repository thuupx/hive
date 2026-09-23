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
