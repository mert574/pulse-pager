package store

import (
	"context"
	_ "embed"
)

//go:embed schema.sql
var schemaSQL string

// ApplySchema applies the whole schema to the database, dropping and recreating
// the known tables. It is the early-development stand-in for migrations: re-run
// it to reset the schema. It uses the simple query protocol so the multi-statement
// script (including the DO block) runs in one round trip. Run it with a
// privileged connection (it creates a role and policies).
func ApplySchema(ctx context.Context, p *Pool) error {
	conn, err := p.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	_, err = conn.Conn().PgConn().Exec(ctx, schemaSQL).ReadAll()
	return err
}
