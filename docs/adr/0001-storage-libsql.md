# ADR 0001 — Storage: libSQL via go-libsql

- Status: accepted
- Date: 2026-09-23
- Decision: D3 — see "Decisions already made" in `AGENTS.md`

## Context

Hive needs an embedded transactional store for coordinator-owned domain
state: sessions, agent runs, commands, events, snapshots, handoffs,
permission requests, bindings, plugins, and workspaces.

The v1 deployment is a single machine. There is no external Redis, Kafka,
NATS, Postgres, or message queue, and no distributed consensus.

The architecture document selects libSQL and directs the implementation to
use the maintained `go-libsql` rather than the deprecated older Go client.

The alternative considered was a pure-Go SQLite driver, which removes the
CGO toolchain requirement and makes cross-compilation trivial.

## Decision

Use libSQL through `github.com/tursodatabase/go-libsql`, with CGO enabled.

## Consequences

Accepted costs:

- **CGO and a native library.** Builds require a C toolchain, and the
  released native library is available only for `linux amd64`,
  `linux arm64`, `darwin amd64`, and `darwin arm64`. **Windows is not
  supported by the driver**, so Windows is not a v1 release target.
- **Release automation is heavier.** Cross-compilation with CGO is
  impractical, so releases build on native runners per OS and
  architecture rather than cross-compiling from one host.
- **The module has no semantic version tags.** `go-libsql` is consumed as
  a pseudo-version. Upgrades are explicit and must be reviewed, not
  floated.
- **Multi-statement `Exec` is not relied upon.** The driver executes one
  statement per call, so migrations are split explicitly in
  `internal/storage/sqlsplit.go`. This keeps failures attributable to one
  statement instead of depending on undocumented behaviour.
- **Pragmas are per connection.** `database/sql` pools connections and
  SQLite pragmas are connection-scoped, so the driver connector is wrapped
  to apply `journal_mode=WAL`, `synchronous=NORMAL`, `busy_timeout`, and
  `foreign_keys=ON` on every new connection.
- **`BEGIN IMMEDIATE` is issued manually.** The driver's `BeginTx` issues
  a deferred `BEGIN` and supports no other isolation level, so write
  transactions are started explicitly and serialized in-process.

Not used in v1, and deliberately not a justification for the choice:

- libSQL embedded replicas
- remote replication to a libSQL server
- Turso-specific features

Coordinator failover is not part of v1, so replication features provide no
current value. They are accepted as future upside only.

## Reversibility

The storage package is the only place that knows about libSQL. Repositories
use `database/sql` interfaces and avoid libSQL-specific SQL, so replacing
the driver with a pure-Go SQLite implementation is a contained change:
swap the connector, keep the schema and the query layer. Migrations and SQL
dialect are the parts that would need review, not the domain model.
