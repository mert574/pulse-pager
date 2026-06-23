// Command migrate applies pending database migrations (internal/store/migrations)
// to the database in PULSE_POSTGRES_DSN using goose. It is the normal way to change
// the schema of a real database: forward-only, never drops data, runs only the
// migrations not yet applied (tracked in goose_db_version). Create a new migration
// with `make migrate-create name=...`.
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
		fmt.Fprintln(os.Stderr, "migrate: PULSE_POSTGRES_DSN is required")
		os.Exit(1)
	}
	if err := store.MigrateUp(context.Background(), dsn); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	fmt.Println("migrations applied")
}
