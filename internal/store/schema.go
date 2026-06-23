package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql "pgx" driver for goose
	"github.com/pressly/goose/v3"
)

//go:embed schema.sql
var schemaSQL string

//go:embed migrations/*.sql
var migrationsFS embed.FS

const migrationsDir = "migrations"

// ApplySchema builds a database from scratch: the frozen baseline (schema.sql) plus
// every incremental migration on top, so a fresh or test database (testcontainers)
// matches a migrated production database. schema.sql is the BASELINE and is frozen:
// all schema changes go in migrations/ (goose), never by editing schema.sql. It uses
// the simple query protocol so the multi-statement baseline (DO block, functions)
// runs in one round trip. Run it with a privileged connection (it creates a role and
// policies). This path does not record goose versions (the DB is brand new); the
// versioned runner used against real databases is MigrateUp.
func ApplySchema(ctx context.Context, p *Pool) error {
	conn, err := p.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Conn().PgConn().Exec(ctx, schemaSQL).ReadAll(); err != nil {
		return fmt.Errorf("apply baseline: %w", err)
	}
	ups, err := migrationUps()
	if err != nil {
		return err
	}
	for _, up := range ups {
		if strings.TrimSpace(up) == "" {
			continue
		}
		if _, err := conn.Conn().PgConn().Exec(ctx, up).ReadAll(); err != nil {
			return fmt.Errorf("apply migration: %w", err)
		}
	}
	return nil
}

// MigrateUp applies pending migrations to the database at dsn using goose, recording
// applied versions in goose_db_version. This is the production/dev path (`make
// migrate`): it never drops data and runs only migrations not yet applied. The
// baseline (schema.sql) is assumed already present (a real DB was bootstrapped once
// with `make schema` on an empty database); goose layers the migrations/ changes on
// top.
func MigrateUp(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.UpContext(ctx, db, migrationsDir)
}

// migrationUps returns the Up SQL of each migration file in order, for the exec-only
// path in ApplySchema (a fresh DB needs the changes but not the version bookkeeping).
func migrationUps() ([]string, error) {
	entries, err := fs.ReadDir(migrationsFS, migrationsDir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		b, err := migrationsFS.ReadFile(migrationsDir + "/" + n)
		if err != nil {
			return nil, err
		}
		out = append(out, gooseUpSection(string(b)))
	}
	return out, nil
}

// gooseUpSection extracts the Up SQL from a goose migration: the text between
// "-- +goose Up" and "-- +goose Down", with the "-- +goose" directive lines removed.
func gooseUpSection(content string) string {
	var b strings.Builder
	inUp := false
	for _, ln := range strings.Split(content, "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(t, "-- +goose Up"):
			inUp = true
		case strings.HasPrefix(t, "-- +goose Down"):
			return b.String()
		case !inUp || strings.HasPrefix(t, "-- +goose"):
			// skip directive lines and anything before Up
		default:
			b.WriteString(ln)
			b.WriteString("\n")
		}
	}
	return b.String()
}
