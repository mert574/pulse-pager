# ADR-0002: PostgreSQL via pgx + golang-migrate

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-000 section 6.3, RFC-001 sections 7.1 and 8.1, ADR-0001, ADR-0007

## Context
The SaaS replaces the v1 SQLite store with PostgreSQL. We need a Go driver fast enough for the check_results firehose (~10k inserts/sec) that also exposes the Postgres features the data model leans on (binary protocol, COPY, JSONB, arrays, LISTEN/NOTIFY), and a migration tool that can manage range partitions and RLS policy DDL forward-only across five services without them racing to migrate.

## Options considered
- pgx v5 + pgxpool, native interface - fastest Go Postgres driver, native binary protocol and COPY for bulk writes, pgxpool per-acquire hooks are where the org-reset discipline lives.
- database/sql + lib/pq - lib/pq is in maintenance mode and slower, and the generic interface hides pgx features on the hot write path.
- gorm (ORM) - hides the SQL that the whole isolation story depends on being explicit and auditable; weak partitioned-insert and COPY story.
- For migrations: golang-migrate as a library vs the v1 hand-rolled embedded runner.

## Decision
Use github.com/jackc/pgx/v5 with pgxpool through the pgx native interface, not through database/sql, everywhere including the hot paths. Run schema changes with golang-migrate/migrate used as a library, reading forward-only .sql files embedded with //go:embed from internal/store/migrations and tracking applied versions in schema_migrations. Migrations run as a Kubernetes pre-deploy job connecting as the separate BYPASSRLS migration role; the five services then connect as the non-superuser, non-BYPASSRLS service role.

## Consequences
We get COPY and the native batch API on the inserts that matter, and pgxpool acquire hooks give one place to enforce the app.current_org reset (ADR-0001). The SQL stays raw and reviewed, which the cross-tenant test suite depends on. We give up ORM convenience and accept hand-written SQL. golang-migrate handles partition and RLS DDL forward-only with no down files; the v1 store.Migrate contract is preserved in spirit but now lives in the migration job, not at service start, so services do not race. The forward-only Job start time gives a clean point-in-time-recovery anchor for a bad migration. Revisit the driver only if a future need outgrows pgx, and the migration tool if partition automation needs more than embedded SQL gives.
