# ADR-0001: Shared-database row-level multi-tenancy with Postgres RLS

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-000 section 6.1, RFC-001 sections 5.3 and 6, ADR-0002, ADR-0013

## Context
Pulse is a multi-tenant SaaS targeting roughly 50k orgs at SMB scale. The hard invariant is that a user or key from org A can never read or affect org B's data under any endpoint. We need an isolation model that scales to that org count, keeps one migration path, and fails safe if a single query is written without an org filter.

## Options considered
- Shared DB, every tenant row carries org_id, plus Postgres RLS - one schema and one pool, org_id is just an indexed column, RLS is the backstop. Cross-org analytics joins stay possible.
- Schema-per-tenant - real isolation but 50k copies of every table and index, migrations run across 50k schemas, catalog bloat.
- Database-per-tenant - strongest isolation but operationally absurd and over-priced at SMB scale.

## Decision
Single shared PostgreSQL database with mandatory org_id row scoping at the repository layer, backed by Postgres Row-Level Security on every tenant table as defense in depth. Two layers: every tenant query goes through a repository method that requires an org-scoped context, and RLS policies key off a transaction-local session variable (set_config('app.current_org', ..., true) per transaction). The service role is non-superuser and non-BYPASSRLS, and tables use FORCE ROW LEVEL SECURITY so the policy applies even though that role owns the tables. A separate BYPASSRLS migration role exists only for migrations and backfills and is never used by a running service.

## Consequences
A missed WHERE org_id filter returns only the current org's rows instead of leaking, so a single application bug cannot cross tenants. We pay a small per-request cost to set the session variable inside withOrg and to keep the SET transaction-local (a SET without LOCAL would leak across pooled connections). A binding cross-tenant test suite must pass before every release: for each tenant-scoped repository method it asserts org A cannot read, list, update, or delete org B's rows and that RLS blocks a deliberately org-unfiltered query. Physical isolation (db-per-tenant) stays reserved for a future enterprise data-residency tier as an exception, not the model. Revisit only if a residency or contractual requirement forces physical separation for a specific large customer.
