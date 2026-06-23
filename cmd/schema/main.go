// Command schema bootstraps the database schema (the baseline internal/store/schema.sql
// plus migrations) on an EMPTY database in PULSE_POSTGRES_DSN. It is for standing up a
// brand-new database only. It refuses to run against an already-initialized database,
// because the baseline drops and recreates the tables and would destroy data: use
// `make migrate` to change an existing schema. To deliberately wipe and rebuild a dev
// database, set PULSE_FORCE_RESET=true (DESTROYS ALL DATA).
package main

import (
	"context"
	"fmt"
	"os"

	"pulse/internal/store"
)

func main() {
	dsn := os.Getenv("PULSE_POSTGRES_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "schema: PULSE_POSTGRES_DSN is required")
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err := store.Open(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "schema: connect:", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Guard: the baseline is destructive (it drops the known tables). Refuse on a
	// database that already has them unless an operator explicitly opts in, so a
	// stray `make schema` can never silently wipe a populated database.
	var initialized bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.organizations') IS NOT NULL`).Scan(&initialized); err != nil {
		fmt.Fprintln(os.Stderr, "schema: probe:", err)
		os.Exit(1)
	}
	if initialized && os.Getenv("PULSE_FORCE_RESET") != "true" {
		fmt.Fprintln(os.Stderr, "schema: database is already initialized; refusing to drop and recreate it.")
		fmt.Fprintln(os.Stderr, "        Use `make migrate` to apply schema changes. To wipe and rebuild a dev")
		fmt.Fprintln(os.Stderr, "        database (DESTROYS ALL DATA), set PULSE_FORCE_RESET=true.")
		os.Exit(1)
	}

	if err := store.ApplySchema(ctx, pool); err != nil {
		fmt.Fprintln(os.Stderr, "schema:", err)
		os.Exit(1)
	}
	fmt.Println("schema applied")
}
