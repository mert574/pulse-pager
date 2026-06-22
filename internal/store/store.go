// Package store is the Postgres data-access layer. It owns the pgxpool, runs
// migrations, and provides the org-scoping helper that, together with Postgres
// RLS, gives the two-layer tenant isolation from RFC-001 section 6.1: the app
// scopes every tenant query by org_id, and RLS makes a missed filter fail safe.
//
// This is the barebones: the connection, the migration runner, the WithOrg
// helper, and one example tenant table to prove the pattern. The real schema
// (all entities from PRD-001..007) is a later work package.
package store

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// secretCipher encrypts and decrypts secret column values (e.g. secret monitor
// headers). It is the subset of crypto.Cipher the store needs. May be nil, in which
// case secret values are stored as-is (dev/test without a key); production wires a
// real cipher via SetCipher.
type secretCipher interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(encoded string) (string, error)
}

// Pool is a Postgres connection pool.
type Pool struct {
	*pgxpool.Pool
	cipher secretCipher // nil = no encryption of secret columns
}

// Open dials Postgres and verifies the connection with a ping.
func Open(ctx context.Context, dsn string) (*Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Pool{Pool: pool}, nil
}

// SetCipher wires the secret-field cipher used to encrypt secret columns at rest
// (e.g. secret monitor headers, RFC-013). Called once at startup after LoadKey.
func (p *Pool) SetCipher(c secretCipher) { p.cipher = c }

// WithOrg runs fn inside a transaction with the tenant session variable set, so
// RLS policies key off the right org. Every tenant query must go through this
// (or an equivalent that sets app.current_org). set_config(..., true) scopes the
// setting to the transaction, like SET LOCAL but parameterizable.
func (p *Pool) WithOrg(ctx context.Context, orgID int64, fn func(tx pgx.Tx) error) (err error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	if _, err = tx.Exec(ctx, "SELECT set_config('app.current_org', $1, true)", strconv.FormatInt(orgID, 10)); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// IsUniqueViolation reports whether err is a Postgres unique-constraint violation
// (SQLSTATE 23505). Callers turn a clash into a friendly 409 (e.g. a duplicate
// pending invitation, I7).
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// IsLastOwnerViolation reports whether err is the at-least-one-owner trigger firing
// (the trg_memberships_last_owner backstop raises a check_violation, SQLSTATE
// 23514). The service guards this first; this catches a race that slips past.
func IsLastOwnerViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}
