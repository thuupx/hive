package storage

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var coordinatorMigrations embed.FS

//go:embed nodemigrations/*.sql
var nodeMigrations embed.FS

// migration is a single forward-only schema change.
type migration struct {
	version int
	name    string
	body    string
}

// Migrate applies every pending coordinator migration in version order.
//
// Migrations are forward-only: there are no down migrations. Applying is
// idempotent, so Migrate may run on every start.
func (s *Store) Migrate(ctx context.Context) error {
	return s.migrate(ctx, coordinatorMigrations, "migrations")
}

// migrateNode applies every pending node migration in version order.
//
// A node database is local execution state and an event buffer. It is not an
// independent authoritative copy of sessions, permissions, or handoff state.
func (s *Store) migrateNode(ctx context.Context) error {
	return s.migrate(ctx, nodeMigrations, "nodemigrations")
}

func (s *Store) migrate(ctx context.Context, fsys fs.FS, dir string) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT    NOT NULL,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("storage: create schema_migrations: %w", err)
	}

	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}

	migrations, err := loadMigrationsFrom(fsys, dir)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// MigrationVersion returns the highest applied migration version.
func (s *Store) MigrationVersion(ctx context.Context) (int, error) {
	var v *int
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("storage: read migration version: %w", err)
	}
	if v == nil {
		return 0, nil
	}
	return *v, nil
}

// Tables returns the names of the user tables, sorted.
func (s *Store) Tables(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("storage: list tables: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("storage: scan table name: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list tables: %w", err)
	}
	return out, nil
}

func (s *Store) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("storage: read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// applyMigration runs one migration and records it in a single write
// transaction, so a failed migration leaves no partial version record.
func (s *Store) applyMigration(ctx context.Context, m migration) error {
	statements, err := splitStatements(m.body)
	if err != nil {
		return fmt.Errorf("storage: migration %04d_%s: %w", m.version, m.name, err)
	}
	return s.WriteTx(ctx, func(tx Execer) error {
		for i, stmt := range statements {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("storage: migration %04d_%s statement %d: %w", m.version, m.name, i+1, err)
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, unixNano(now()))
		return err
	})
}

func loadMigrations() ([]migration, error) {
	return loadMigrationsFrom(coordinatorMigrations, "migrations")
}

func loadMigrationsFrom(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("storage: read %s: %w", dir, err)
	}

	var out []migration
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".sql" {
			continue
		}
		version, name, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("storage: read %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: name, body: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("storage: %s migrations must be numbered contiguously from 1; got %d at position %d", dir, m.version, i+1)
		}
	}
	return out, nil
}

// parseMigrationName splits "0001_init.sql" into version 1 and name "init".
func parseMigrationName(fileName string) (int, string, error) {
	base := strings.TrimSuffix(fileName, ".sql")
	prefix, name, ok := strings.Cut(base, "_")
	if !ok {
		return 0, "", fmt.Errorf("storage: migration %q must be named <version>_<name>.sql", fileName)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil || version < 1 {
		return 0, "", fmt.Errorf("storage: migration %q has an invalid version prefix", fileName)
	}
	if name == "" {
		return 0, "", fmt.Errorf("storage: migration %q has an empty name", fileName)
	}
	return version, name, nil
}
