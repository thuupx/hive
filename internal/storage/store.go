// Package storage persists coordinator-owned domain state.
//
// The v1 store is libSQL, an SQLite fork. The coordinator is the single
// writer for its database: write transactions are serialized in-process
// and issued as BEGIN IMMEDIATE, so a session's event sequence stays
// strictly monotonic without relying on distributed coordination.
//
// This package is persistence only. Domain rules, state machines, and
// validation belong to the domain layer.
package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	_ "github.com/tursodatabase/go-libsql"
)

// DriverName is the database/sql driver registered by go-libsql.
const DriverName = "libsql"

// connectionPragmas are applied to every new connection.
//
// These are connection-scoped, so they cannot be set once on the pool.
// journal_mode is deliberately absent: it is a persistent database property
// and changing it takes a lock, so it is applied once per database instead.
var connectionPragmas = []string{
	"PRAGMA busy_timeout=5000",
	"PRAGMA foreign_keys=ON",
	"PRAGMA synchronous=NORMAL",
}

// enableWAL switches the database to write-ahead logging. It is a persistent
// database property, so it is applied once when the store opens.
const enableWAL = "PRAGMA journal_mode=WAL"

// Execer is the subset of *sql.DB, *sql.Conn, and *sql.Tx used by storage
// operations. Taking it explicitly lets a caller compose several writes
// into one transaction.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// scanner is satisfied by *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// Store is a handle to a Hive database.
type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
}

// Open opens or creates the database at path, applies migrations, and
// returns a ready store.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path
	base, err := openConnector(dsn)
	if err != nil {
		return nil, err
	}

	pool := sql.OpenDB(&pragmaConnector{base: base, pragmas: connectionPragmas})
	pool.SetMaxOpenConns(8)
	pool.SetConnMaxIdleTime(5 * time.Minute)

	s := &Store{db: pool}
	if err := s.enableWAL(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := s.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// enableWAL switches the database to write-ahead logging.
func (s *Store) enableWAL(ctx context.Context) error {
	var mode string
	if err := s.db.QueryRowContext(ctx, enableWAL).Scan(&mode); err != nil {
		return fmt.Errorf("storage: enable WAL: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("storage: journal mode is %q, want wal", mode)
	}
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying handle for read-only queries. Writes must go
// through WriteTx so that they are serialized.
func (s *Store) DB() *sql.DB { return s.db }

// WriteTx runs fn inside a serialized write transaction.
//
// The transaction is issued as BEGIN IMMEDIATE so the write lock is held
// before any read, which keeps read-then-write sequences such as event
// sequence assignment correct. Serializing in-process avoids SQLITE_BUSY
// contention between concurrent writers.
func (s *Store) WriteTx(ctx context.Context, fn func(tx Execer) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("storage: acquire connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("storage: begin: %w", err)
	}

	if err := fn(conn); err != nil {
		if _, rbErr := conn.ExecContext(ctx, "ROLLBACK"); rbErr != nil {
			return fmt.Errorf("%w (rollback also failed: %v)", err, rbErr)
		}
		return err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return fmt.Errorf("storage: commit: %w", err)
	}
	return nil
}

// now returns the current time in UTC. Timestamps are stored as UTC unix
// nanoseconds so ordering is timezone-independent.
func now() time.Time { return time.Now().UTC() }

func unixNano(t time.Time) int64 { return t.UTC().UnixNano() }

func fromUnixNano(n int64) time.Time { return time.Unix(0, n).UTC() }

// nullableTime stores a zero time as SQL NULL.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return unixNano(t)
}

// timeFromNull reads a nullable timestamp column.
func timeFromNull(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return fromUnixNano(n.Int64)
}

// connectorOpener is the exported method set of the libSQL driver that
// yields a reusable connector, letting Hive wrap it with per-connection
// setup.
type connectorOpener interface {
	OpenConnector(name string) (driver.Connector, error)
}

func openConnector(dsn string) (driver.Connector, error) {
	probe, err := sql.Open(DriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open driver: %w", err)
	}
	defer probe.Close()

	opener, ok := probe.Driver().(connectorOpener)
	if !ok {
		return nil, errors.New("storage: libsql driver does not expose OpenConnector")
	}
	base, err := opener.OpenConnector(dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: create connector: %w", err)
	}
	return base, nil
}

// pragmaConnector applies the connection pragmas on every new connection.
type pragmaConnector struct {
	base    driver.Connector
	pragmas []string
}

func (c *pragmaConnector) Driver() driver.Driver { return c.base.Driver() }

func (c *pragmaConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	queryer, ok := conn.(driver.QueryerContext)
	if !ok {
		conn.Close()
		return nil, errors.New("storage: libsql connection does not support QueryContext")
	}
	for _, p := range c.pragmas {
		// Pragmas such as journal_mode return a row, so they are executed
		// as queries and drained rather than through Exec.
		rows, err := queryer.QueryContext(ctx, p, nil)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("storage: apply %q: %w", p, err)
		}
		dest := make([]driver.Value, len(rows.Columns()))
		for {
			if err := rows.Next(dest); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				rows.Close()
				conn.Close()
				return nil, fmt.Errorf("storage: apply %q: %w", p, err)
			}
		}
		rows.Close()
	}
	return conn, nil
}
