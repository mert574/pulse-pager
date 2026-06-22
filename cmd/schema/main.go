// Command schema applies the database schema in internal/store/schema.sql to the
// database in PULSE_POSTGRES_DSN. It drops and recreates the known tables, so it
// resets the schema each run. This is the early-dev stand-in for migrations.
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

	if err := store.ApplySchema(ctx, pool); err != nil {
		fmt.Fprintln(os.Stderr, "schema:", err)
		os.Exit(1)
	}
	fmt.Println("schema applied")
}
