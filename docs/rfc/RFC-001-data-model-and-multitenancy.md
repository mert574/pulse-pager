# RFC-001 - Data Model and Multi-Tenancy

Status: DRAFT for review
Author: Principal Architecture (data/backend)
Parent: `docs/rfc/RFC-000-architecture-overview.md` (sections 6, 5, 14; ADR-0001/0002/0007)
Product source of truth: `docs/prd/PRD-001..007`
Supersedes for the storage layer: the v1 single-binary architecture's storage section (the SQLite DDL is carried forward in spirit; the runtime is replaced)

House style note: no em-dashes anywhere in this document.

---

## 1. Overview and scope

This RFC is the schema and tenancy foundation. Almost every other RFC reads or writes data described here, so it is concrete and complete: real DDL, real indexes, real constraints, real RLS policy SQL.

### 1.1 What this RFC owns (from RFC-000 section 13)

| Owned contract | Where in this RFC |
|----------------|-------------------|
| Full PostgreSQL schema for every entity in PRD-001..007, with `org_id` on every org-owned table | Section 4 |
| The shared-DB + repository org scoping + Row-Level Security tenancy implementation | Section 5 |
| The cross-tenant isolation test suite that must pass before any release | Section 5.4 |
| `check_results` time-range partitioning, retention as partition drop, hourly rollups | Section 6 |
| The id strategy (internal PK + external `mon_`/`inc_` ids) | Section 3 |
| The Postgres `Store` replacing the SQLite store, on pgx/pgxpool | Section 7 |
| Forward-only migrations, partition and RLS migration mechanics | Section 8 |
| Backup, PITR, DR posture and how the 14-day deletion grace interacts with backups | Section 9 |
| Read replicas and replication-lag handling | Section 10 |

### 1.2 What this RFC does not own

- Event schemas and idempotency-key field layout: RFC-002. This RFC only fixes the columns that those keys land in (the `(monitor_id, region, checked_at)` unique constraint, the partial unique open-incident index, the notify dedup backstop table).
- The entitlement enforcement logic and cache: RFC-009. This RFC owns the `plans`, `subscriptions`, and `entitlements` tables only.
- Region health detection and failover: RFC-008. This RFC owns the `regions` catalog and `region_health` tables only.
- AuthN/AuthZ flows: RFC-003. This RFC owns `users`, `user_identities`, `memberships`, `api_keys`, `refresh_tokens`, `idempotency_keys`.

### 1.3 Conventions

- All timestamps are `TIMESTAMPTZ`, stored UTC. The v1 "TEXT RFC3339" choice does not carry forward; Postgres has a real timestamp type and we use it. RFC3339 stays the wire format (RFC-000 section 0), produced at serialization, not storage.
- Every org-owned table has `org_id BIGINT NOT NULL` as its first real column after the PK, an index that leads with `org_id`, and an RLS policy. The one exception is `users` (global) and the global catalog tables (`plans`, `regions`, `goose_db_version`).
- Encrypted columns are flagged in the DDL with a comment `-- ENCRYPTED (crypto AES-256-GCM)`. They store `base64(nonce||ciphertext||tag)` exactly as `internal/crypto` produces today (RFC-000 section 10, `internal/crypto` reused unchanged).
- Hashed columns are flagged `-- HASHED`. A hash is one-way and is never decrypted.

---

## 2. Entity map

The entities, their owner, and their tenancy class. Global rows are not org-scoped and carry no `org_id`; everything else is org-scoped and falls under RLS (section 5).

| Table | Tenancy | Source PRD | External id prefix |
|-------|---------|-----------|--------------------|
| `users` | global | PRD-001 | `usr_` |
| `user_identities` | global (FK to user) | PRD-001 | none (internal) |
| `refresh_tokens` | global (FK to user) | PRD-001/003 | none |
| `organizations` | tenant root | PRD-001 | `org_` |
| `memberships` | org | PRD-001 | none |
| `seats` | org | PRD-001 | none |
| `invitations` | org | PRD-001 | `inv_` |
| `api_keys` | org | PRD-005 | `key_` (prefix is its own thing) |
| `plans` | global catalog | PRD-006 | code (`tier1`/`tier2`/`tier3`/`tierCustom`) |
| `subscriptions` | org | PRD-006 | none |
| `entitlements` | org | PRD-006 | none |
| `monitors` | org | PRD-002 | `mon_` |
| `monitor_headers` | org (FK to monitor) | PRD-002 | none |
| `channels` | org | PRD-003 | `chn_` |
| `monitor_channels` | org (join) | PRD-002/003 | none |
| `check_results` (partitioned) | org | PRD-002 | `res_` |
| `incidents` | org | PRD-002 | `inc_` |
| `check_rollups` | org | PRD-002/004 | none |
| `status_pages` | org | PRD-004 | `sp_` |
| `status_page_monitors` | org (join) | PRD-004 | none |
| `regions` | global catalog | PRD-007 | region code (`us-east`) |
| `region_health` | global | PRD-007 | none |
| `audit_events` | org | PRD-001 | none |
| `outbound_webhooks` | org | PRD-005 | `wh_` |
| `idempotency_keys` | org | PRD-005 | none |
| `notify_dedup` | org | RFC-000 5.3 backstop | none |
| `goose_db_version` | global | goose (RFC-001) | none |

---

## 3. ID strategy (decision)

### 3.1 Decision

- **Internal primary keys: `BIGINT GENERATED ALWAYS AS IDENTITY`** (Postgres 10+ identity columns, the modern form of `BIGSERIAL`). One per row, monotonic per table.
- **External-facing ids: a prefixed string that encodes the bigint**, shaped `mon_<encoded>`, `inc_<encoded>`, `chn_<encoded>`, and so on, matching the PRD payloads (`mon_123`, `inc_456` in PRD-003:137-138). The encoding is done at the API serialization boundary (RFC-012 owns the codec), not stored. The database row stays a clean `BIGINT`.
- **`check_results` keeps a `BIGINT` identity** but its real identity for dedup is the composite unique key `(org_id, monitor_id, region, checked_at)`, carried forward and extended from v1's intent (section 4, section 6).

### 3.2 Reasoning

| Factor | Why BIGINT identity wins |
|--------|--------------------------|
| Index locality | Monotonic bigints append to the right of the B-tree. On `check_results` (10k inserts/sec, RFC-000 6.2) this matters a lot: random UUID v4 PKs scatter writes across the index and cause page splits and write amplification. Append-mostly bigints keep insert cost low and the index dense. |
| Storage / firehose | A bigint PK is 8 bytes; a UUID is 16, and it repeats in every secondary index and every `check_results` partition. Across 500k monitors times retention, the UUID overhead on the firehose table and its indexes is large for no isolation benefit. |
| Event keys (RFC-000 5.2) | Kafka partition keys are `monitor_id` and `org_id`. A bigint is a compact, stable key. The composite result dedup key `(monitor_id, region, checked_at)` is the real idempotency key (RFC-000 5.3), not the surrogate id, so the surrogate does not need to be globally unique or random. |
| URL safety / enumeration | URLs and events never carry the raw bigint. They carry the prefixed external id. Enumeration resistance is provided by authz + RLS (org A cannot read org B's `mon_2` even if it guesses the number), not by id opacity. Where a context truly needs unguessable ids (the public status-page URL, the invitation accept link, the webhook), that surface uses a separate random token column (`status_pages.public_token`, `invitations.token`, not the PK). |
| Joins / analytics | Cross-table joins on bigint FKs are cheaper than on UUID, and our own internal analytics joins across tenant tables stay fast. |

### 3.3 Rejected alternatives

| Alternative | Why rejected |
|-------------|--------------|
| UUID v4 as PK everywhere | Index fragmentation and write amplification on `check_results` is the disqualifier at 10k inserts/sec. The "ids are unguessable" win is not needed because RLS + authz already make a guessed id useless across tenants, and the surfaces that need unguessable values get a dedicated random token. |
| ULID / UUID v7 (time-ordered) as PK | Better locality than UUID v4 (time-prefixed, so append-friendly), and genuinely tempting for `check_results`. Still 16 bytes vs 8 in every index and partition, and still larger event keys. We keep ULID in our pocket: if a future need for client-generated, collision-free ids across regions appears (for example workers minting result ids offline during a home-region partition), `check_results.id` can move to ULID without touching the dedup key. For v1 the workers get `checked_at` + region from the job, so the composite key already dedups and a surrogate ULID buys nothing. |
| Expose the raw bigint in URLs/events | Rejected. It leaks row counts and growth rate (a competitor can read `mon_5012` and infer total monitor volume), and it couples the public id to the storage surrogate so we could never re-key. The prefixed external id decouples them and matches the PRD payload shape. |

### 3.4 The external id codec (contract, owned by RFC-012)

The codec is a pure function `external(prefix, bigint) -> string` and its inverse `internal(string) -> (prefix, bigint)`. v1 uses the trivial decimal encoding to match the PRD examples literally (`mon_123` is `mon_` + `"123"`). If product later wants opacity in URLs, the codec swaps to a keyed encoding (for example sqids/hashids over the bigint) with zero schema change, because nothing is stored. RFC-001 only fixes that the stored id is the bigint and the prefix-to-table mapping in section 2.

---

## 4. Full PostgreSQL DDL

The DDL below is the schema as of migration `0001_init` through the partition and RLS migrations. It is grouped by domain for reading. Read the tenancy section (5) and partitioning section (6) alongside; the RLS policies and the `check_results` partition mechanics are pulled into those sections to keep them together, but they are part of the same migration set.

Ordering note: the blocks below are presented by domain, not in strict apply order, so a few forward foreign-key references appear (for example `organizations.plan_id` references `plans`, and `memberships.seat_id` references `seats`, both created in later blocks). The actual migration file orders its statements by dependency: catalog tables first (`plans`, `regions`), then `users`, then `organizations`, then `seats`, then the tables that reference them. Postgres needs the referenced table to exist before the FK is declared, so the file order is dependency-sorted even though this document reads by domain.

### 4.1 Identity and tenancy (PRD-001)

```sql
-- Global. The one cross-org row. No org_id, no RLS.
CREATE TABLE users (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  primary_email TEXT        NOT NULL,                 -- verified identity anchor
  display_name  TEXT        NOT NULL DEFAULT '',
  avatar_url    TEXT        NOT NULL DEFAULT '',
  status        TEXT        NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active','deletion-pending','deleted')),
  deletion_pending_at TIMESTAMPTZ,                    -- set when status -> deletion-pending
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uniq_users_primary_email ON users (lower(primary_email))
  WHERE status <> 'deleted';

-- Global. A user has 1..2 identities (google/github). Account linking on verified email.
CREATE TABLE user_identities (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id          BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider         TEXT   NOT NULL CHECK (provider IN ('google','github')),
  provider_user_id TEXT   NOT NULL,                   -- provider's stable subject id
  provider_email   TEXT   NOT NULL,
  email_verified   BOOLEAN NOT NULL DEFAULT false,    -- must be true to sign in / auto-link
  linked_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- I4: one identity per (user, provider). I5: a provider account maps to one user.
CREATE UNIQUE INDEX uniq_identity_user_provider ON user_identities (user_id, provider);
CREATE UNIQUE INDEX uniq_identity_provider_subject
  ON user_identities (provider, provider_user_id);

-- Global (keyed by user, but also carries org for the active-org claim convenience).
-- Opaque refresh tokens, hashed. Supports multi-device and revoke-all. RFC-003 owns lifetimes.
CREATE TABLE refresh_tokens (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash   TEXT   NOT NULL,                       -- HASHED (sha-256 of opaque token)
  family_id    BIGINT NOT NULL,                        -- one login chain; rotation keeps the family (RFC-003 4.1)
  device_label TEXT   NOT NULL DEFAULT '',
  issued_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at   TIMESTAMPTZ NOT NULL,
  replaced_by  BIGINT REFERENCES refresh_tokens(id),   -- set on rotation; if a replaced token is presented again, revoke the family (RFC-003 4.1)
  revoked_at   TIMESTAMPTZ                            -- non-null => revoked (logout / role change)
);
CREATE UNIQUE INDEX uniq_refresh_token_hash ON refresh_tokens (token_hash);
CREATE INDEX idx_refresh_user ON refresh_tokens (user_id) WHERE revoked_at IS NULL;
CREATE INDEX idx_refresh_expires ON refresh_tokens (expires_at);

-- Tenant root.
CREATE TABLE organizations (
  id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name                TEXT   NOT NULL,
  slug                TEXT   NOT NULL,                 -- unique globally, shapes {slug}.pulsepager.com
  kind                TEXT   NOT NULL CHECK (kind IN ('personal','team')),
  plan_id             BIGINT NOT NULL REFERENCES plans(id),
  status              TEXT   NOT NULL DEFAULT 'active'
                       CHECK (status IN ('active','deletion-pending','deleted')),
  deletion_pending_at TIMESTAMPTZ,                     -- 14-day grace window start (section 9.4)
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uniq_org_slug ON organizations (lower(slug))
  WHERE status <> 'deleted';

-- Note: organizations carries id = org_id for itself. RLS on this table keys off
-- id (section 5.2) rather than a separate org_id column.

CREATE TABLE memberships (
  id        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id    BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  user_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role      TEXT   NOT NULL CHECK (role IN ('owner','admin','member','viewer')),
  source    TEXT   NOT NULL CHECK (source IN ('signup','invitation')),
  seat_id   BIGINT REFERENCES seats(id) ON DELETE SET NULL,
  joined_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- I3: a user has at most one membership per org.
CREATE UNIQUE INDEX uniq_membership_user_org ON memberships (org_id, user_id);
CREATE INDEX idx_membership_user ON memberships (user_id);
-- I1 (>=1 owner) is enforced in the repository transaction, not a DB constraint:
-- a partial check cannot express "at least one row with role=owner per org" cheaply.
-- The repository refuses the last-owner removal/role-change (section 7.5).
CREATE INDEX idx_membership_owner ON memberships (org_id) WHERE role = 'owner';

CREATE TABLE seats (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id      BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  occupancy   TEXT   NOT NULL DEFAULT 'free'
               CHECK (occupancy IN ('accepted-member','reserved-invite','free')),
  -- occupied_by is a discriminated reference: kind says which table the id points at.
  occupied_kind TEXT CHECK (occupied_kind IN ('membership','invitation')),
  occupied_id   BIGINT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK ((occupancy = 'free') = (occupied_kind IS NULL))
);
CREATE INDEX idx_seats_org ON seats (org_id);

CREATE TABLE invitations (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id           BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  invited_email    TEXT   NOT NULL,
  target_role      TEXT   NOT NULL CHECK (target_role IN ('owner','admin','member','viewer')),
  state            TEXT   NOT NULL DEFAULT 'pending'
                    CHECK (state IN ('pending','accepted','revoked','expired')),
  token_hash       TEXT   NOT NULL,                    -- HASHED; the raw token is in the email link only
  invited_by       BIGINT NOT NULL REFERENCES users(id),
  reserved_seat_id BIGINT REFERENCES seats(id) ON DELETE SET NULL,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at       TIMESTAMPTZ NOT NULL,               -- created_at + 7 days
  accepted_at      TIMESTAMPTZ
);
-- I7: at most one pending invite per (org, email).
CREATE UNIQUE INDEX uniq_invite_pending
  ON invitations (org_id, lower(invited_email)) WHERE state = 'pending';
CREATE UNIQUE INDEX uniq_invite_token ON invitations (token_hash);
CREATE INDEX idx_invite_org ON invitations (org_id);
CREATE INDEX idx_invite_expiry ON invitations (expires_at) WHERE state = 'pending';
```

### 4.2 Billing and entitlements (PRD-006, RFC-009 owns logic)

```sql
-- Global catalog. The four tiers. No org_id, no RLS.
CREATE TABLE plans (
  id                       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  code                     TEXT NOT NULL UNIQUE
                            CHECK (code IN ('tier1','tier2','tier3','tierCustom')),
  display_name             TEXT NOT NULL,
  monitors_cap             INT  NOT NULL,             -- 10 / 25 / 50 / 1000
  min_interval_seconds     INT  NOT NULL,             -- 900 / 300 / 60 / 30
  hard_floor_seconds       INT  NOT NULL DEFAULT 30,  -- 30 across all tiers
  regions_allowed_count    INT  NOT NULL,             -- 1 / 2 / 4 / 6
  premium_regions_allowed  BOOLEAN NOT NULL,          -- false / false / false / true
  seats_included           INT  NOT NULL,             -- 1 / 3 / 10 / 25
  per_seat_addon_enabled   BOOLEAN NOT NULL,
  retention_days           INT  NOT NULL,             -- 7 / 30 / 90 / 180
  status_pages_cap         INT  NOT NULL,             -- 1 / 1 / 3 / 10
  custom_domain_allowed    BOOLEAN NOT NULL,          -- N / N / Y / Y
  api_access               TEXT NOT NULL
                            CHECK (api_access IN ('read','full')), -- Free is read-only
  api_rate_per_min         INT  NOT NULL,             -- 30 / 120 / 300 / 600
  outbound_webhooks_enabled BOOLEAN NOT NULL,
  audit_log_retention_days INT,                       -- NULL / NULL / 30 / 365
  failure_snapshot_enabled BOOLEAN NOT NULL DEFAULT true, -- store last failed response (PRD-002 3.8); on for all tiers now
  created_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Org -> plan, seat count, billing state. PRD-006 section 2.3.
CREATE TABLE subscriptions (
  id                     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id                 BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  plan_id                BIGINT NOT NULL REFERENCES plans(id),
  status                 TEXT   NOT NULL DEFAULT 'active'
                          CHECK (status IN ('active','trialing','past_due','canceled')),
  seats_purchased        INT    NOT NULL DEFAULT 0,   -- paid seats beyond plan-included
  billing_cycle          TEXT   CHECK (billing_cycle IN ('monthly','annual')), -- Phase 2
  provider_customer_id   TEXT,                          -- (provider-agnostic, see RFC-018)
  provider_subscription_id TEXT,                        -- (provider-agnostic, see RFC-018)
  trial_end              TIMESTAMPTZ,                    -- trial end; length is provider-driven, plan_prices.trial_days
  current_period_end     TIMESTAMPTZ,                    -- Phase 2
  created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uniq_subscription_org ON subscriptions (org_id);  -- one active sub per org
CREATE INDEX idx_subscription_provider ON subscriptions (provider_subscription_id)
  WHERE provider_subscription_id IS NOT NULL;

-- Concrete per-org allowances. Normally derived from plan, but stored so an
-- override (a custom enterprise deal, a comped limit) does not need a fake plan.
-- RFC-009 owns how this is populated and cached; RFC-001 owns the columns.
CREATE TABLE entitlements (
  org_id                  BIGINT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
  monitors_cap            INT  NOT NULL,
  min_interval_seconds    INT  NOT NULL,              -- the floor (plan floor or override)
  hard_floor_seconds      INT  NOT NULL DEFAULT 30,
  seats_included          INT  NOT NULL,
  seats_purchased         INT  NOT NULL DEFAULT 0,
  regions_allowed         TEXT[] NOT NULL,            -- set of region codes the org may use
  regions_per_monitor_cap INT  NOT NULL,
  retention_days          INT  NOT NULL,
  status_pages_cap        INT  NOT NULL,
  custom_domain           BOOLEAN NOT NULL,
  api_access              TEXT NOT NULL CHECK (api_access IN ('none','read','full')),
  api_rate_per_min        INT  NOT NULL,
  outbound_webhooks       BOOLEAN NOT NULL,
  audit_log_retention_days INT,
  failure_snapshot        BOOLEAN NOT NULL DEFAULT true, -- store last failed response (PRD-002 3.8)
  updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

`plans` is seeded by a data migration with the exact tier matrix from PRD-006 section 3:

| code | monitors_cap | min_interval_seconds | regions_allowed_count | premium | seats_included | retention_days | status_pages_cap | custom_domain | api_access | api_rate_per_min | audit_log_retention_days |
|------|-------------:|---------------------:|----------------------:|---------|---------------:|---------------:|-----------------:|---------------|-----------|-----------------:|-------------------------:|
| tier1 | 10 | 900 | 1 | N | 1 | 7 | 1 | N | none | 0 | NULL |
| tier2 | 25 | 300 | 1 | N | 3 | 30 | 3 | N | read | 120 | NULL |
| tier3 | 50 | 60 | 4 | N | 10 | 90 | 10 | Y | full | 300 | 30 |
| tierCustom | 1000 | 30 | 4 | Y | 1000000 | 180 | 1000 | Y | full | 600 | 365 |

### 4.3 Monitoring engine (PRD-002) - carries forward the v1 columns

```sql
CREATE TABLE monitors (
  id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id                BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  type                  TEXT    NOT NULL DEFAULT 'http' CHECK (type = 'http'), -- only http in v1
  name                  TEXT    NOT NULL,
  url                   TEXT    NOT NULL,
  method                TEXT    NOT NULL DEFAULT 'GET'
                         CHECK (method IN ('GET','POST','PUT','PATCH','DELETE','HEAD')),
  body                  TEXT    NOT NULL DEFAULT '',
  expected_status_codes TEXT    NOT NULL DEFAULT '200',  -- raw spec "200,204" or "2xx" (v1)
  timeout_seconds       INT     NOT NULL DEFAULT 10 CHECK (timeout_seconds BETWEEN 1 AND 60),
  interval_seconds      INT     NOT NULL DEFAULT 60,
  enabled               BOOLEAN NOT NULL DEFAULT true,
  max_latency_ms        INT,                              -- NULL = no assertion (v1)
  body_contains         TEXT,                             -- NULL = no assertion (v1)
  failure_threshold     INT     NOT NULL DEFAULT 1 CHECK (failure_threshold >= 1),
  -- multi-region (new in this SaaS, PRD-002/007)
  regions               TEXT[]  NOT NULL,                 -- non-empty list of region codes
  down_policy           TEXT    NOT NULL DEFAULT 'quorum'
                         CHECK (down_policy IN ('any','quorum','all')),
  -- alert state carried forward from v1 (survives restart, RFC-000 8)
  consecutive_fails     INT     NOT NULL DEFAULT 0,
  first_fail_at         TIMESTAMPTZ,                       -- first fail of current run, NULL when count=0
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (cardinality(regions) >= 1)
);
CREATE INDEX idx_monitors_org ON monitors (org_id);
-- scheduler boot: load enabled monitors for an org (and globally on rebuild)
CREATE INDEX idx_monitors_enabled ON monitors (org_id) WHERE enabled = true;

-- headers as rows so a secret header can be encrypted per value (v1 design carried forward)
CREATE TABLE monitor_headers (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id     BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  monitor_id BIGINT  NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  key        TEXT    NOT NULL,
  value      TEXT    NOT NULL,    -- ENCRYPTED (crypto AES-256-GCM) when is_secret, plaintext otherwise
  is_secret  BOOLEAN NOT NULL DEFAULT false
);
CREATE INDEX idx_headers_monitor ON monitor_headers (monitor_id);
CREATE INDEX idx_headers_org ON monitor_headers (org_id);

-- Last failed check's response, per monitor (PRD-002 3.8). One row per monitor,
-- overwritten on each new failure, so it does not grow with check volume and is
-- kept off the high-volume check_results table. Captured only on response-level
-- failures (status_mismatch / latency_exceeded / body_assertion_failed). Operational
-- data, not secret: stored plaintext, org-scoped, never on public status pages.
CREATE TABLE monitor_last_failure (
  monitor_id  BIGINT PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE,
  org_id      BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  checked_at  TIMESTAMPTZ NOT NULL,
  status_code INT,
  headers     JSONB NOT NULL DEFAULT '{}'::jsonb,  -- response headers as captured
  body        TEXT  NOT NULL DEFAULT '',           -- response body up to the 64 KB cap
  truncated   BOOLEAN NOT NULL DEFAULT false,      -- body exceeded the cap
  captured_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_last_failure_org ON monitor_last_failure (org_id);

CREATE TABLE channels (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id     BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  name       TEXT    NOT NULL,
  type       TEXT    NOT NULL CHECK (type IN ('slack','discord','webhook','smtp')),
  config     JSONB   NOT NULL,   -- type-specific; secret fields inside are ENCRYPTED per value
  enabled    BOOLEAN NOT NULL DEFAULT true,
  created_by BIGINT  REFERENCES users(id) ON DELETE SET NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_channels_org ON channels (org_id);

-- Secret fields inside channels.config (encrypted per value before the JSONB is written):
--   slack:   webhook_url
--   discord: webhook_url
--   webhook: url, and each custom_headers[].value
--   smtp:    password
-- Non-secret keys (smtp host/port/from/to/tls/username) stay plaintext in the JSONB.

CREATE TABLE monitor_channels (
  org_id     BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  monitor_id BIGINT NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  channel_id BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  PRIMARY KEY (monitor_id, channel_id)
);
CREATE INDEX idx_mc_channel ON monitor_channels (channel_id);
CREATE INDEX idx_mc_org ON monitor_channels (org_id);
```

`check_results` DDL is in section 6.1. Current state: it is a plain unpartitioned table, not partitioned.

```sql
CREATE TABLE incidents (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id          BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  monitor_id      BIGINT  NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  started_at      TIMESTAMPTZ NOT NULL,                -- first failing check of the run
  ended_at        TIMESTAMPTZ,                          -- NULL while open
  cause_reason    TEXT    NOT NULL,                     -- the failure_reason that opened it
  close_reason    TEXT    CHECK (close_reason IN ('recovered','disabled','manual')), -- NULL while open
  closed_by       BIGINT  REFERENCES users(id) ON DELETE SET NULL, -- set when close_reason='manual'
  first_result_id BIGINT REFERENCES check_results(id) ON DELETE SET NULL, -- link to the failing check_result
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_incidents_monitor_time ON incidents (org_id, monitor_id, started_at DESC);
-- the load-bearing invariant: at most one OPEN incident per monitor, now per (org, monitor)
CREATE UNIQUE INDEX uniq_open_incident
  ON incidents (org_id, monitor_id) WHERE ended_at IS NULL;
-- global per-org incident list, open-first
CREATE INDEX idx_incidents_open ON incidents (org_id, ended_at, started_at DESC);
```

Note on `incidents.first_result_id`: it is a real foreign key, `REFERENCES check_results(id) ON DELETE SET NULL`. If the linked raw result is ever removed, the column goes to NULL rather than blocking the delete. The incident itself holds `started_at` and `cause_reason` so nothing user-visible is lost. (The original plan kept this as a soft reference because `check_results` was meant to be partitioned and aged out by partition drop; that partitioning is not built yet, see section 6, so the FK is in place.)

### 4.4 Status pages (PRD-004)

```sql
CREATE TABLE status_pages (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id        BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  name          TEXT    NOT NULL,
  slug          TEXT    NOT NULL,                      -- unique within org; /{slug} path
  state         TEXT    NOT NULL DEFAULT 'draft' CHECK (state IN ('draft','published')),
  custom_domain TEXT,                                  -- NULL unless plan allows + configured
  public_token  TEXT    NOT NULL,                      -- unguessable token for any non-slug public ref
  branding      JSONB   NOT NULL DEFAULT '{}',         -- logo, theme, accent (non-secret)
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uniq_status_page_slug ON status_pages (org_id, lower(slug));
CREATE UNIQUE INDEX uniq_status_page_domain ON status_pages (lower(custom_domain))
  WHERE custom_domain IS NOT NULL;
CREATE INDEX idx_status_pages_org ON status_pages (org_id);

CREATE TABLE status_page_monitors (
  org_id         BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  status_page_id BIGINT NOT NULL REFERENCES status_pages(id) ON DELETE CASCADE,
  monitor_id     BIGINT NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  display_name   TEXT,                                 -- override; NULL = use monitor.name
  sort_order     INT    NOT NULL DEFAULT 0,
  PRIMARY KEY (status_page_id, monitor_id)
);
CREATE INDEX idx_spm_org ON status_page_monitors (org_id);
CREATE INDEX idx_spm_page_order ON status_page_monitors (status_page_id, sort_order);
```

### 4.5 Regions (PRD-007, RFC-008 owns health logic)

```sql
-- Global catalog. No org_id, no RLS (read by everyone, written by ops/migrations).
CREATE TABLE regions (
  code           TEXT PRIMARY KEY,                     -- 'us-east','eu-west',... stable, never reused
  display_name   TEXT NOT NULL,                        -- 'US East (Virginia)'
  geography      TEXT NOT NULL,                        -- 'North America','Europe','Asia-Pacific'
  cost_class     TEXT NOT NULL CHECK (cost_class IN ('standard','premium')),
  is_premium     BOOLEAN NOT NULL DEFAULT false,
  is_home        BOOLEAN NOT NULL DEFAULT false,       -- one home region (control plane)
  lifecycle      TEXT NOT NULL DEFAULT 'available'
                  CHECK (lifecycle IN ('available','deprecated','retired')),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Global. Current liveness per region, fed by worker heartbeats (RFC-008, region.health topic).
CREATE TABLE region_health (
  code            TEXT PRIMARY KEY REFERENCES regions(code),
  status          TEXT NOT NULL DEFAULT 'healthy' CHECK (status IN ('healthy','degraded','unhealthy')), -- three-value: degraded is the failover early-warning state (RFC-008 5.2)
  last_heartbeat  TIMESTAMPTZ,
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

A monitor's `regions TEXT[]` references region codes by value, not by FK (Postgres cannot FK an array element). The repository validates each code against `regions` (and against the org's `entitlements.regions_allowed`) on write; the scheduler re-checks on dispatch (RFC-000 section 12). Region codes are stable and never reused, so a value reference is safe.

### 4.6 API keys, webhooks, audit, idempotency (PRD-005, PRD-001)

```sql
CREATE TABLE api_keys (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id       BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  name         TEXT    NOT NULL,
  role         TEXT    NOT NULL CHECK (role IN ('member','admin')), -- never owner (PRD-005)
  key_hash     TEXT    NOT NULL,                       -- HASHED (SHA-256 of presented secret; RFC-003 5.2 explains why not bcrypt/argon2)
  prefix       TEXT    NOT NULL,                       -- e.g. 'pulse_sk_ab12...' non-secret
  last_used_at TIMESTAMPTZ,
  created_by   BIGINT  REFERENCES users(id) ON DELETE SET NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at   TIMESTAMPTZ                             -- non-null => revoked, fails all requests
);
CREATE UNIQUE INDEX uniq_api_key_hash ON api_keys (key_hash);
CREATE INDEX idx_api_keys_org ON api_keys (org_id);
CREATE INDEX idx_api_keys_prefix ON api_keys (prefix);  -- prefix lookup narrows the hash compare

CREATE TABLE outbound_webhooks (
  id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id               BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  endpoint_url         TEXT    NOT NULL,
  signing_secret       TEXT    NOT NULL,               -- ENCRYPTED (crypto AES-256-GCM)
  event_types          TEXT[]  NOT NULL,               -- monitor.down, incident.opened, ...
  enabled              BOOLEAN NOT NULL DEFAULT true,
  last_delivery_at     TIMESTAMPTZ,
  last_delivery_status TEXT    CHECK (last_delivery_status IN ('success','failed')),
  last_delivery_error  TEXT,
  created_by           BIGINT  REFERENCES users(id) ON DELETE SET NULL,
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_webhooks_org ON outbound_webhooks (org_id);

-- Append-only audit trail. Per-org, visible to owner/admin (PRD-001 section 9).
CREATE TABLE audit_events (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id     BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  actor_type TEXT    NOT NULL CHECK (actor_type IN ('human','api_key','system')),
  actor_id   BIGINT,                                   -- user_id or api_key_id; NULL when system
  action     TEXT    NOT NULL,                         -- 'member.invited','org.deleted',...
  target     TEXT,                                     -- resource ref or email
  changes    JSONB,                                    -- old/new fields for edits
  ip_address INET,
  user_agent TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_org_time ON audit_events (org_id, created_at DESC);
CREATE INDEX idx_audit_org_action ON audit_events (org_id, action, created_at DESC);
-- The actor_type split answers RFC-000 open-question 2 (human vs api-key vs system actor).

-- HTTP write dedup (PRD-005 section 3.7). 24h response replay on retried Idempotency-Key.
CREATE TABLE idempotency_keys (
  org_id          BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  key             TEXT    NOT NULL,                    -- client-supplied Idempotency-Key
  request_hash    TEXT    NOT NULL,                    -- reject same key + different body (409)
  response_status INT     NOT NULL,
  response_body   JSONB   NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at      TIMESTAMPTZ NOT NULL,                -- created_at + 24h
  PRIMARY KEY (org_id, key)
);
CREATE INDEX idx_idem_expires ON idempotency_keys (expires_at);

-- Notifier dedup backstop (RFC-000 5.3). Redis is the fast path; this is the durable fallback.
CREATE TABLE notify_dedup (
  org_id     BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  dedup_id   TEXT   NOT NULL,                          -- hash(incident_id, event_type)
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, dedup_id)
);
CREATE INDEX idx_notify_dedup_age ON notify_dedup (created_at);

-- Migration tracking (section 8). goose owns this table and its shape; it is
-- created and maintained by goose, not by our baseline.
CREATE TABLE goose_db_version (
  id         SERIAL  PRIMARY KEY,
  version_id BIGINT  NOT NULL,
  is_applied BOOLEAN NOT NULL,
  tstamp     TIMESTAMP DEFAULT now()
);
```

---

## 5. Tenancy isolation implementation

This is RFC-000 ADR-0001 made concrete. Three layers, each independently sufficient to keep the hard invariant, so a bug in one is caught by the next.

The hard invariant (PRD-001 I8, RFC-000 6.1): a user or key from org A can never read or affect org B's data, under any endpoint, ever.

### 5.1 Layer 1: repository org scoping

No handler builds raw SQL. All tenant access goes through `internal/store` repository methods, and every such method takes an org-scoped context. The context carries the authenticated `org_id` resolved by `internal/authz` from the request (RFC-003). Every query the repository issues for a tenant table includes `WHERE org_id = $orgID`, and every insert sets `org_id` from the context, never from the request body.

This is the primary correctness mechanism. Layers 2 and 3 exist so that a single forgotten `WHERE org_id` does not leak.

### 5.2 Layer 2: Postgres Row-Level Security

RLS is enabled and forced on every tenant table. Each tenant connection sets a transaction-local session variable `app.current_org` to the caller's org id, and the policies key off it.

```sql
-- Applied for every org-owned table. Shown for monitors; the same block is
-- generated for every table in section 2 that has an org_id (and for organizations,
-- keyed off id instead of org_id).

ALTER TABLE monitors ENABLE ROW LEVEL SECURITY;
ALTER TABLE monitors FORCE ROW LEVEL SECURITY;   -- applies even to the table owner role

CREATE POLICY org_isolation ON monitors
  USING      (org_id = current_setting('app.current_org', true)::BIGINT)   -- read/update/delete visibility
  WITH CHECK (org_id = current_setting('app.current_org', true)::BIGINT);  -- insert/update target rows

-- organizations keys off its own id (it has no org_id column):
ALTER TABLE organizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE organizations FORCE ROW LEVEL SECURITY;
CREATE POLICY org_self_isolation ON organizations
  USING      (id = current_setting('app.current_org', true)::BIGINT)
  WITH CHECK (id = current_setting('app.current_org', true)::BIGINT);
```

Notes that make this safe:

- `current_setting('app.current_org', true)` uses the two-argument `missing_ok` form, so when the variable is unset it returns NULL instead of raising. `org_id = NULL` is never true, so a tenant query that runs without an org set matches no rows rather than returning all rows. The fail-closed behavior comes from the no-match, not from a raised error. The repository always sets the org (5.3) before any tenant query.
- The application role that the services connect as is a non-superuser, non-`BYPASSRLS` role. `FORCE ROW LEVEL SECURITY` ensures the policy applies even though that role owns the tables. Migrations run as a separate role that is allowed to bypass RLS (it needs to touch all rows during a backfill), and that role is never used by a running service.
- Global tables (`users`, `plans`, `regions`, `region_health`, `goose_db_version`) have no RLS. `user_identities` and `refresh_tokens` are keyed by `user_id`, not `org_id`, and are reached only through the auth path which scopes by the authenticated user; they are not under org RLS by design (a user spans orgs).

### 5.3 How the repository sets the session variable

Each unit of tenant work runs in a transaction that first sets the org, using `set_config` with the `is_local = true` flag so the setting is scoped to the transaction and cannot leak to the next checkout of a pooled connection.

```go
// Pool.WithOrg runs fn inside a transaction that has app.current_org set to the
// passed org for the life of that transaction only. The org is passed explicitly
// as an argument, not read from the context. Every tenant repository call goes
// through this.
func (p *Pool) WithOrg(ctx context.Context, orgID int64, fn func(tx pgx.Tx) error) error {
    return pgx.BeginTxFunc(ctx, p.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
        // is_local = true: scoped to THIS transaction, reset on commit/rollback.
        if _, err := tx.Exec(ctx,
            "SELECT set_config('app.current_org', $1, true)",
            strconv.FormatInt(orgID, 10)); err != nil {
            return fmt.Errorf("set org scope: %w", err)
        }
        return fn(tx)
    })
}
```

Why `is_local = true` and a transaction per unit of work: pgxpool reuses connections. A `SET` without `LOCAL` would persist on the physical connection and the next request that checks out that connection would inherit the previous org. `set_config(..., true)` ties the value to the transaction, so a commit or rollback clears it and a leaked org is impossible. This is the single most important detail in the whole tenancy implementation, so it is centralized in `WithOrg` and never written ad hoc.

### 5.4 Cross-tenant isolation test suite (binding, must pass before release)

This suite is the release check RFC-000 6.1 requires. It runs against a real Postgres (testcontainers) with RLS enabled and the application role, not a superuser. It seeds two orgs, A and B, each with a full set of rows, then asserts isolation from both the repository angle and the RLS-backstop angle.

| # | Class | Test | Pass condition |
|---|-------|------|----------------|
| T1 | Repository read | For every tenant repository `Get*`/`List*` method, call it with org A's context against an id that belongs to org B | returns not-found / empty, never B's row |
| T2 | Repository write | For every `Update*`/`Delete*`/`SetEnabled` method, call with org A's context targeting org B's id | affects zero rows, returns not-found |
| T3 | Repository insert | Insert with org A's context but a request body claiming `org_id = B` | the stored row has `org_id = A` (org comes from context, body is ignored) |
| T4 | RLS backstop, read | Open a tenant transaction for org A, set `app.current_org = A`, then run a deliberately org-unfiltered `SELECT * FROM monitors` (simulating a repository bug that forgot the WHERE) | returns only A's rows; B's are invisible |
| T5 | RLS backstop, write | Same transaction, run `UPDATE monitors SET name='x'` with no WHERE | updates only A's rows; B's are untouched |
| T6 | RLS backstop, cross-org insert | With `app.current_org = A`, attempt `INSERT ... (org_id) VALUES (B...)` | rejected by the policy `WITH CHECK` |
| T7 | Missing org fails closed | Run any tenant query without setting `app.current_org` | returns no rows (current_setting returns NULL, so `org_id = NULL` matches nothing) |
| T8 | Pool isolation | Run org A work, then org B work, on a pool of size 1 so they share a physical connection | B's transaction never sees A's `app.current_org`; the `is_local` setting was reset |
| T9 | Endpoint-level | For every `/api/v1` route that takes a resource id, authenticate as org A and request org B's resource id | 404 (not 403, to avoid confirming existence), never B's data |
| T10 | Join safety | A query that joins two tenant tables under org A cannot pull a B row through the join | only A rows in the result |

T4-T8 are the ones that prove RLS catches a repository bug: they bypass the `WHERE org_id` filter on purpose and assert the database still isolates. CI runs the whole suite (RFC-000 11.4 lists it in the pipeline); a failure blocks the release.

---

## 6. check_results at scale

`check_results` is the firehose: ~10k inserts/sec sustained, 500k monitors, region-multiplied (RFC-000 6.2). It is the table whose volume drives the partitioning plan below.

Current state: `check_results` is a plain unpartitioned table. The monthly RANGE partitioning, the per-org early prune, and the rollups in this section are not built yet; they are kept here as the planned design. The shipped DDL is:

```sql
CREATE TABLE check_results (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id         BIGINT      NOT NULL,
  monitor_id     BIGINT      NOT NULL,
  region         TEXT        NOT NULL,                 -- region code that ran the check
  checked_at     TIMESTAMPTZ NOT NULL,
  healthy        BOOLEAN     NOT NULL,
  failure_reason TEXT,                                  -- NULL when healthy
  status_code    INT,                                   -- NULL on conn error/timeout/blocked
  latency_ms     INT,                                   -- NULL when no latency
  error_text     TEXT,                                  -- short, truncated
  UNIQUE (org_id, monitor_id, region, checked_at)       -- dedup identity
);

-- The dedup key from RFC-000 5.3 is the UNIQUE tuple (org_id, monitor_id, region, checked_at).
-- A redelivered job writes the same row -> ON CONFLICT DO NOTHING is a no-op (section 7.4).
-- The surrogate id is the primary key and is what incidents.first_result_id references.

CREATE INDEX idx_results_hot
  ON check_results (org_id, monitor_id, checked_at DESC, region);
```

### 6.1 Planned: partitioning by monthly RANGE on checked_at

This is the planned target, not yet built. Declarative RANGE partitioning of `check_results` by `checked_at`, one partition per calendar month.

```sql
-- Planned parent (no rows live here; it routes to monthly children).
CREATE TABLE check_results (
  id             BIGINT GENERATED ALWAYS AS IDENTITY,
  org_id         BIGINT      NOT NULL,
  monitor_id     BIGINT      NOT NULL,
  region         TEXT        NOT NULL,                 -- region code that ran the check
  checked_at     TIMESTAMPTZ NOT NULL,
  healthy        BOOLEAN     NOT NULL,
  failure_reason TEXT,                                  -- NULL when healthy
  status_code    INT,                                   -- NULL on conn error/timeout/blocked
  latency_ms     INT,                                   -- NULL when no latency
  error_text     TEXT,                                  -- short, truncated
  PRIMARY KEY (org_id, monitor_id, region, checked_at)  -- composite, includes the partition key
) PARTITION BY RANGE (checked_at);

-- Example monthly child (created ahead of time by the partition manager, section 6.4):
CREATE TABLE check_results_2026_06 PARTITION OF check_results
  FOR VALUES FROM ('2026-06-01 00:00:00+00') TO ('2026-07-01 00:00:00+00');
```

Why monthly grain (in the planned design):

| Consideration | Monthly | Weekly (rejected) | Daily (rejected) |
|---------------|---------|-------------------|------------------|
| Live partition count at max retention (180d) | ~7 | ~26 | ~180 |
| Retention drop precision | drop a month at a time; we keep one extra month and drop on month boundary, so worst case a row lives retention_days + ~30d | precise to a week | precise to a day |
| Planner overhead per query | low (few partitions to prune) | moderate | high (planning 180 partitions hurts) |
| Index maintenance | one set of indexes per month | 4x more | 30x more |

Monthly wins because the planner stays fast (a history query prunes to one or two partitions) and the partition count stays small even at 180-day retention. The cost is coarser retention precision: a row can outlive its plan retention by up to a month before its partition is dropped. That is acceptable; retention is a floor-guarantee of how long we keep data, not a hard ceiling, and the over-retention is bounded and uniform. Weekly would tighten that to a week but triples the partition count and the planning cost for a precision the product does not need.

### 6.2 Retention as a partition DROP

Per-plan retention (PRD-006, section 4.2 table: 7 / 30 / 90 / 180 days) becomes a partition lifecycle, not a `DELETE`. The v1 `DeleteResultsBefore` mass-delete is gone.

The wrinkle: retention is per plan, but a partition holds rows from all orgs of all plans. We cannot drop a month-partition while any org still needs rows in it. So the rule is:

- A partition is eligible to DROP only once its entire time range is older than the longest retention tier we sell (180 days, Custom). At that point no org can still need any row in it, so `DROP TABLE check_results_YYYY_MM` reclaims it instantly with no vacuum churn. This is the cheap, common path.
- For orgs on shorter tiers (Free 7d, Hobby 30d, Professional 90d), their rows in a not-yet-droppable partition are pruned by a lightweight per-org delete that the rollup job runs anyway (it reads the raw rows to build rollups, section 6.3, and deletes raw rows past that org's retention in the same pass). Because rollups already persist the aggregate the UI needs, deleting an org's raw rows early loses nothing user-visible.

So the architecture is: rollups are the durable history the product shows; raw `check_results` is a short-lived staging firehose that is dropped by partition at the 180-day outer bound and pruned per-org earlier by the rollup job. Status pages and history charts read rollups, not raw rows (section 6.3), so shorter-tier pruning never affects what a customer sees within their window.

### 6.3 Planned: hourly rollups

Current state: not built. There is no `check_rollups` table and no rollup job. This section is the planned design.

Rollups are per `(org_id, monitor_id, region, hour)`. They are how uptime and history charts stay fast and correct over the retention window without scanning the firehose.

```sql
CREATE TABLE check_rollups (
  org_id        BIGINT      NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  monitor_id    BIGINT      NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  region        TEXT        NOT NULL,
  bucket_hour   TIMESTAMPTZ NOT NULL,                  -- truncated to the hour, UTC
  checks_total  INT         NOT NULL,
  checks_ok     INT         NOT NULL,
  checks_failed INT         NOT NULL,
  latency_p50   INT,                                   -- ms, NULL if no successful checks
  latency_p95   INT,
  latency_max   INT,
  PRIMARY KEY (org_id, monitor_id, region, bucket_hour)
);
CREATE INDEX idx_rollups_monitor_time ON check_rollups (org_id, monitor_id, bucket_hour DESC);
```

Who owns producing rollups: a rollup job. It is not its own service; it is a leader-elected periodic task that runs inside the `alerting` service (alerting already reads `check.results` and already holds the per-monitor view, and it already runs in the control plane next to Postgres). It runs once per hour, a few minutes after the hour closes, and for each `(org, monitor, region)` with new raw rows in the just-closed hour:

1. Aggregate the raw `check_results` for that hour into one rollup row (`INSERT ... ON CONFLICT (pk) DO UPDATE`, so a re-run is idempotent).
2. In the same pass, delete that org's raw rows older than its `entitlements.retention_days` (the per-org early prune from 6.2).

Rollups are retained for the longest uptime window any status page shows (PRD-004: up to 90d, clamped to plan tier). They are tiny (one row per monitor per region per hour) so they persist cheaply well past the raw-row retention.

How status-page uptime is computed (PRD-004 section 3.3, incident-duration based per the consistency review):

- The status page shows uptime over a window (24h / 7d / 90d, clamped to the org's retention). Uptime is `1 - (sum of open-incident durations overlapping the window) / (monitored time in window)`. Incidents are the durable, retained record (kept for the life of the org, 4.3), so this number stays correct over the whole window regardless of raw-row pruning.
- The per-hour bars / latency sparkline on the page read `check_rollups`, not raw rows. A 90-day page reads at most `90 * 24 = 2160` rollup rows per monitor per region, a trivial scan, served off a read replica (section 10) and cached (RFC-000 6.2).

This keeps the status page fast and correct: the headline uptime comes from incidents (small, retained), the visual history comes from rollups (small, retained), and the firehose raw rows are never on the read path.

### 6.4 Partition management

A periodic task (same leader-elected runner as the rollup job) keeps partitions ahead and behind:

- Pre-create: always have the current and next two monthly partitions to exist before any row needs them, so an insert never hits a missing partition.
- Drop: once a monthly partition's whole range is older than 180 days, `DROP TABLE` it.

Both run idempotently (create-if-not-exists, drop-if-eligible). The DDL they emit (`CREATE TABLE ... PARTITION OF`, `DROP TABLE`) is plain SQL and does not go through the migration tool; partition lifecycle is data-plane maintenance, not schema change (section 8.4).

### 6.5 Indexes that matter

| Index | Table | Serves |
|-------|-------|--------|
| `PRIMARY KEY (org_id, monitor_id, region, checked_at)` | check_results | the dedup constraint and the exact-row upsert (section 7.4) |
| `idx_results_hot (org_id, monitor_id, checked_at DESC, region)` | check_results | monitor history newest-first; per-region by trailing range scan |
| `idx_rollups_monitor_time (org_id, monitor_id, bucket_hour DESC)` | check_rollups | status-page bars, dashboard history |
| `uniq_open_incident (org_id, monitor_id) WHERE ended_at IS NULL` | incidents | one-open-incident invariant; the idempotent open (RFC-000 8) |
| `idx_incidents_open (org_id, ended_at, started_at DESC)` | incidents | per-org incident list, open-first |
| `idx_monitors_enabled (org_id) WHERE enabled` | monitors | scheduler boot / rebuild |

The region dimension is intentionally a trailing column on the hot index, not a separate index. A query is almost always "this monitor's history" (all regions) or "this monitor, this region"; both are satisfied by a range scan on the composite index. A standalone `(region, ...)` index would only help "all monitors in region X," which is an ops query, not a hot product path, and can scan the rollups or a replica.

---

## 7. The Postgres Store

### 7.1 Driver: pgx + pgxpool (decision, confirms RFC-000 ADR-0002)

Decision: `github.com/jackc/pgx/v5` with `pgxpool`, used through the pgx native interface (not through `database/sql`).

| Option | Verdict | Why |
|--------|---------|-----|
| pgx native + pgxpool (chosen) | chosen | Fastest Postgres driver in Go, native support for the Postgres binary protocol, `COPY` for bulk result/rollup writes, `LISTEN/NOTIFY`, array types (`TEXT[]` for `monitors.regions`), and `JSONB` without round-tripping through `database/sql` interfaces. pgxpool gives us connection pooling with per-acquire hooks, which is where the `app.current_org` reset discipline lives (section 5.3). |
| database/sql + lib/pq | rejected | lib/pq is in maintenance mode and slower; using `database/sql` would also hide pgx features (binary protocol, COPY) behind the generic interface, costing us on the firehose write path. |
| database/sql + pgx stdlib adapter | rejected for the hot paths | pgx can run under `database/sql` via `stdlib`, but that gives up COPY and the native batch API on exactly the inserts that matter. We use pgx native everywhere for one consistent surface. |
| gorm (ORM) | rejected | An ORM hides the SQL, and this RFC's whole isolation story depends on the SQL being explicit and auditable (the `WHERE org_id`, the `set_config`, the `ON CONFLICT`). gorm's RLS-session-variable story is awkward, its partitioned-insert and COPY story is poor, and the generated SQL is hard to reason about for the cross-tenant suite. We keep raw, reviewed SQL. |

### 7.2 The interface, ported from v1

The v1 `Store` interface (`internal/store/store.go`) ports forward method-for-method where the shape carries, so the reused domain code and its tests move with minimal change (RFC-000 14). The changes are: every tenant method now operates under an org-scoped context (the org is in the context, not a parameter, so signatures stay close to v1), and new entity groups are added. `Migrate` moves out of the runtime store into the migration job (section 8).

```go
// PgStore is the Postgres Store. It owns the pgxpool and the cipher (reused
// internal/crypto), sets app.current_org per tenant transaction (section 5.3),
// and writes results partition-aware (section 7.4).
type Store interface {
    Close() error

    // monitors (org-scoped via ctx) - shapes carried from v1
    CreateMonitor(ctx context.Context, m *domain.Monitor) (int64, error)
    GetMonitor(ctx context.Context, id int64) (*domain.Monitor, error)
    ListMonitors(ctx context.Context) ([]*domain.Monitor, error)
    UpdateMonitor(ctx context.Context, m *domain.Monitor) error
    DeleteMonitor(ctx context.Context, id int64) error
    SetMonitorEnabled(ctx context.Context, id int64, enabled bool) error
    ListEnabledMonitors(ctx context.Context) ([]*domain.Monitor, error) // scheduler boot
    ListMonitorsWithStatus(ctx context.Context) ([]MonitorListItem, error)

    // channels (org-scoped) - carried from v1; config secrets encrypted via crypto
    CreateChannel(ctx context.Context, c *domain.Channel) (int64, error)
    GetChannel(ctx context.Context, id int64) (*domain.Channel, error)
    ListChannels(ctx context.Context) ([]*domain.Channel, error)
    UpdateChannel(ctx context.Context, c *domain.Channel) error
    DeleteChannel(ctx context.Context, id int64) error
    GetChannelsForMonitor(ctx context.Context, monitorID int64) ([]*domain.Channel, error)

    // check results (org-scoped, partition-aware, idempotent) - carried + extended with region
    InsertResult(ctx context.Context, r *domain.CheckResult) (int64, error)
    LatestResult(ctx context.Context, monitorID int64) (*domain.CheckResult, error)
    ListResults(ctx context.Context, q ResultQuery) ([]*domain.CheckResult, string, error)
    HasResults(ctx context.Context, monitorID int64) (bool, error)
    // DeleteResultsBefore is GONE: retention is partition drop + rollup-job prune (section 6.2)

    // incidents (org-scoped) - carried from v1; open uses the partial unique index
    OpenIncident(ctx context.Context, inc *domain.Incident) (int64, error)
    CloseIncident(ctx context.Context, id int64, endedAt time.Time, reason domain.CloseReason) error
    GetOpenIncident(ctx context.Context, monitorID int64) (*domain.Incident, error)
    ListIncidentsForMonitor(ctx context.Context, q IncidentQuery) ([]*domain.Incident, string, error)
    ListIncidents(ctx context.Context, q IncidentQuery) ([]*domain.Incident, string, error)

    // alert state (org-scoped) - carried from v1 verbatim, including SetAlertCounters
    GetAlertState(ctx context.Context, monitorID int64) (*domain.AlertState, error)
    SetConsecutiveFails(ctx context.Context, monitorID int64, n int) error
    SetAlertCounters(ctx context.Context, monitorID int64, consecutiveFails int, firstFailAt *time.Time) error

    // rollups (owned by the rollup job, section 6.3)
    UpsertRollups(ctx context.Context, rows []Rollup) error
    ListRollups(ctx context.Context, q RollupQuery) ([]Rollup, error)
    PruneRawResults(ctx context.Context, orgID, monitorID int64, before time.Time) (int64, error)

    // NEW entity groups (org-scoped unless noted) - shapes per section 4
    // users/identities/refresh-tokens are GLOBAL (not org-scoped): scoped by user_id
    Users() UserRepo            // global
    Orgs() OrgRepo              // org root + membership/seat/invitation
    ApiKeys() ApiKeyRepo        // org
    StatusPages() StatusPageRepo // org
    Webhooks() WebhookRepo      // org
    Billing() BillingRepo       // plans (global) + subscriptions/entitlements (org)
    Regions() RegionRepo        // global catalog + region_health
    Audit() AuditRepo           // org, append-only
    Idempotency() IdempotencyRepo // org
}
```

The v1 method shapes (`CreateMonitor`, `OpenIncident`, `SetAlertCounters`, the `ResultQuery`/`IncidentQuery` cursors, `MonitorListItem`) are kept exactly so the proven scheduler/alerting/checker code and the existing store tests port. The only removed method is `DeleteResultsBefore` (retention is now partition-based) and the admin/session methods (v1's single-admin model is replaced by the auth tables, RFC-003). `Migrate` is removed from the runtime interface and lives in the migration job.

### 7.3 RLS session setup on every tenant method

Every tenant method goes through `withOrg` (section 5.3) so `app.current_org` is set inside the transaction before any SQL touches a tenant table. Global methods (`Users()`, plan catalog reads, region catalog reads) do not set an org and run against tables that have no RLS.

### 7.4 Partition-aware, idempotent result writes

`InsertResult` writes into the plain `check_results` table (no partition routing today, section 6) and is idempotent under bus redelivery via the unique tuple:

```sql
INSERT INTO check_results
  (org_id, monitor_id, region, checked_at, healthy, failure_reason, status_code, latency_ms, error_text)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (org_id, monitor_id, region, checked_at) DO NOTHING
RETURNING id;
```

A redelivered job (RFC-000 5.3: scheduler stamps `checked_at` so a redelivery carries the same value) hits the conflict and is a no-op; the worker then re-emits the same `check.results` event, which alerting dedups (RFC-000 8). For the bulk path (a worker flushing a batch, or the rollup job), pgx `CopyFrom` into a staging then upsert is available, but the per-result `ON CONFLICT` insert is the default and is what the idempotency contract relies on.

### 7.5 Where the repository enforces invariants the DB cannot

A few invariants are cheaper or only expressible in the repository transaction, under `withOrg`:

- At-least-one-owner (PRD-001 I1): on remove-member / role-change, the repository counts owners in the same transaction and refuses to drop the last one. A DB constraint cannot express "at least one row with role=owner per org" without an expensive trigger.
- Seat accounting (I6): occupy / free a seat in the same transaction as the membership or invitation change, so the seat count stays exact.
- Idempotency-key conflict (PRD-005): on a repeat key with a different `request_hash`, return 409 rather than replay.

### 7.6 Read-replica routing

The Store holds two pools: a primary pool (writes and read-after-write reads) and a replica pool (read-heavy, lag-tolerant reads). Routing is explicit per method, covered in section 10.

---

## 8. Migrations

### 8.1 Tool: goose over embedded SQL (decision)

Decision: `pressly/goose` (`github.com/pressly/goose/v3`) reading numbered `.sql` files from `internal/store/migrations/` (`0000N_*.sql`), each with `-- +goose Up` and `-- +goose Down` sections, tracking applied versions in goose's own `goose_db_version` table. `schema.sql` is the frozen from-empty baseline; a fresh db applies the baseline then every migration, a real db runs the pending migrations forward.

RFC-000 6.3 left the choice between the v1 hand-rolled embedded runner and a library migration tool, with the deciding factor being partition and RLS DDL. That factor decides it for goose:

| Factor | goose (chosen) | v1 hand-rolled runner (rejected) |
|--------|--------------------------|----------------------------------|
| Plain ordered SQL files | yes, exactly what we want for the RLS policy blocks; multi-statement and dollar-quoted blocks wrap in `-- +goose StatementBegin`/`StatementEnd` | yes |
| Applied-version tracking | goose records applied versions in `goose_db_version` | the v1 runner tracked its own versions |
| Runs as a one-shot, not per-service at boot | yes; `MigrateUp` runs from a one-shot path (`make migrate` / a pre-deploy job) | the v1 runner ran at service start, which five services would race (RFC-000 6.3 explicitly moves this to a pre-deploy job) |
| Down sections | each goose file carries a `-- +goose Down` reverse, though our convention is still forward-only in production | yes |
| Dependency weight | one well-maintained library | zero deps, but we give up battle-tested apply logic to save one import |

The `store.Migrate(ctx)` contract from v1 is preserved in spirit: there is still a single entry point (`MigrateUp`) that applies the embedded SQL forward and records versions. goose is the engine underneath.

Migrations are forward-only in practice. Each file has a `-- +goose Down` section so a migration is reversible in dev, but a bad migration reaching production is fixed by a new forward migration, never a down against production data.

### 8.2 How it runs in Kubernetes

A `migrate` binary (`cmd/migrate`) runs as a Kubernetes Job before any service rollout, as a Helm pre-install/pre-upgrade hook (RFC-011 owns the Helm wiring). The Job:

1. Connects as the migration role (a role with `BYPASSRLS` and DDL rights, distinct from the service role).
2. Runs `MigrateUp` (goose). goose records each applied version in `goose_db_version`; a failed migration stops the run and a human investigates.
3. On success, the rollout of the five services proceeds. The services connect as the non-superuser, non-BYPASSRLS service role.

Because migrations run as one Job, the five services never race to migrate (the explicit fix for the v1 boot-time approach, RFC-000 6.3).

### 8.3 Migrating RLS policies

RLS policy DDL is ordinary SQL in a migration file. A migration that adds a new tenant table includes, in the same file: the `CREATE TABLE`, its indexes, `ALTER TABLE ... ENABLE/FORCE ROW LEVEL SECURITY`, and the `CREATE POLICY org_isolation`. The cross-tenant test suite (5.4) is what proves the policy was actually attached; a migration that creates a tenant table without its policy fails T4-T6 and blocks the release.

### 8.4 Migrating partitioned tables

- The initial `check_results` partitioned parent and the `check_rollups` table are created in a migration like any other table.
- Ongoing partition create/drop is NOT a migration. It is data-plane maintenance run by the leader-elected partition manager (section 6.4). Mixing routine partition rotation into the migration version stream would make the version count grow forever and couple monthly maintenance to deploys. The migration set owns the schema shape; the partition manager owns the rolling set of children.
- A schema change to `check_results` (a new column) is a migration on the parent; Postgres propagates it to existing and future children. We avoid such changes on the firehose where possible (an additive nullable column is cheap; a rewrite is not).

---

## 9. Backup, PITR, and DR

### 9.1 Managed Postgres stance

Per RFC-000 11.3, Postgres is a managed cloud offering. Backup, WAL archiving, and PITR are configured on the managed instance; we do not run our own base-backup cron. What this RFC fixes is the policy the managed instance must be configured to meet (RFC-011 provisions it via Terraform).

| Aspect | Policy |
|--------|--------|
| Automated full backup | daily |
| WAL archiving | continuous, for point-in-time recovery |
| PITR window | 7 days minimum (covers the common "bad migration / bad delete found within a week") |
| Backup retention | 30 days of daily backups |
| Cross-region backup copy | backups replicated to a second region so a home-region loss is recoverable (DR) |
| Encryption | backups encrypted at rest (managed-provider KMS) |

### 9.2 Point-in-time recovery

PITR is the primary recovery tool for "we shipped a bad migration" or "a bug deleted rows." We restore to a new instance at a timestamp just before the bad event, validate, then cut over. Because migrations are forward-only and run as a discrete Job (section 8), the "just before" timestamp is easy to pin: it is the migration Job start time.

### 9.3 Restore drill

A restore is only real if it is rehearsed. The drill, run quarterly (RFC-011 owns the schedule and runbook):

1. Trigger a PITR restore of production to a fresh instance at a chosen timestamp.
2. Point a staging copy of the services at it.
3. Run the cross-tenant suite (5.4) and a smoke test against the restored data.
4. Record the wall-clock restore time as the practical RTO; confirm it meets the DR target (RFC-011 sets the number).

### 9.4 The 14-day deletion grace and backups

Org deletion is a 14-day grace, then hard delete (RFC-000 open-questions notes this is locked; PRD-001). The interaction with backups:

- During grace, `organizations.status = 'deletion-pending'` and `deletion_pending_at` is set. The org is locked (no logins act on it) but its rows still exist. A restore during grace brings it back intact, which is the point of the grace: a mistaken delete is recoverable by the customer within 14 days without any restore at all (we just flip status back).
- At grace end, a hard delete removes the org's rows (`ON DELETE CASCADE` from `organizations` removes memberships, monitors, channels, incidents, status pages, api keys, webhooks, audit events, idempotency keys, rollups; raw `check_results` is not FK-cascaded because it is partitioned, so the hard delete also issues a scoped delete of that org's rows in the live partitions, and the rest age out by partition drop).
- Backups still contain the org's data until those backups age out of the 30-day retention. This is the known tension between "hard delete on request" and "we keep backups." The stance: hard delete removes the org from the live database immediately at grace end; backups are write-once and expire on their own 30-day cycle, and a GDPR erasure request is satisfied by the live delete plus the documented backup-expiry window. RFC-011 and the security pass own the formal data-retention statement; this RFC fixes that the live delete is immediate and cascading at grace end.

---

## 10. Read scaling

### 10.1 Primary plus read replicas

One primary (all writes) and N read replicas (managed, RFC-000 11.3). The Store holds two pgxpools: `primary` and `replica`. Routing is explicit per method, never automatic, so a developer chooses consistency deliberately.

| Read | Pool | Why |
|------|------|-----|
| Status-page serving (uptime, rollup bars) | replica | read-heavy, public, lag-tolerant (it is a derived view, RFC-000 8); must stay up even if the primary write path is slow or down |
| History / dashboard charts (rollups) | replica | read-heavy, lag-tolerant |
| Incident list, monitor list | replica | lag-tolerant |
| Entitlement source-of-truth read (cache miss) | primary | RFC-009 fail-closed-on-write needs the authoritative value; a stale replica could under- or over-grant |
| Read-after-write (e.g. GET right after POST monitor) | primary | see 10.2 |
| All writes | primary | the only writer |
| Scheduler boot rebuild | primary | it needs the authoritative enabled set; a one-time boot read on the primary is cheap |

### 10.2 Replication-lag and read-after-write

Replicas lag the primary by a small, variable amount. Most reads tolerate it (status, history, lists are derived views, RFC-000 8). The case that does not tolerate it is read-after-write: a user creates a monitor and immediately lists monitors, expecting to see it.

The rule: a read that is part of the same logical operation as a just-completed write, or a read the SPA issues immediately after a write it just made, goes to the primary. Concretely:

- The api write handler returns the created/updated resource from the primary in the write response itself (no follow-up read needed for the common case).
- For the SPA's "write then navigate and list" pattern, the api offers a `consistency=strong` hint on the affected read routes that pins that single read to the primary; the default is replica. RFC-012 owns the header/param surface.
- Background services (alerting, the rollup job) read the primary for anything they will write back, since they need current state.

This keeps the bulk of read traffic on replicas (the scale win) while making the handful of read-after-write paths correct by routing them to the primary. We do not attempt global causal consistency or LSN tracking in v1; explicit per-read routing is simpler and covers the real cases.

---

## 11. Open questions and dependencies

### 11.1 Dependencies this RFC hands off

| To | What |
|----|------|
| RFC-002 (eventing) | The dedup columns this RFC provides: `check_results` PK `(org_id, monitor_id, region, checked_at)`, the `uniq_open_incident` partial index, the `notify_dedup` backstop table, the `idempotency_keys` table. RFC-002 fixes the event field that fills each. |
| RFC-009 (entitlements) | The `plans`, `subscriptions`, `entitlements` tables. RFC-009 owns population, the Redis cache, and invalidation; RFC-001 owns the columns. |
| RFC-008 (multi-region) | The `regions` catalog and `region_health` tables. RFC-008 owns health detection and failover; RFC-001 owns the schema and the `monitors.regions[]` value-reference. |
| RFC-003 (auth) | `users`, `user_identities`, `refresh_tokens`, `api_keys`, the `audit_events.actor_type` split. RFC-003 owns token lifetimes, hashing, OIDC; RFC-001 owns the columns. |
| RFC-012 (API) | The external id codec (`mon_`/`inc_`/... prefix mapping, section 3.4) and the `consistency=strong` read hint (section 10.2). |
| RFC-011 (infra) | The migration Job wiring, the managed-Postgres backup/PITR/replica provisioning, the migration role vs service role split. |

### 11.2 Open questions

1. Audit retention as a partitioned stream. RFC-000 open-question 2 notes login-event audit volume (`auth.login`) may want its own retention separate from people-changes. `audit_events` is a single table here with per-plan `audit_log_retention_days` (Professional 30d, Custom 365d, Free/Hobby none). If login-event volume turns out to dominate, the cheap next step is to range-partition `audit_events` by `created_at` (same mechanism as `check_results`) and drop old partitions, and possibly split a high-volume `auth.login` stream into its own retention. Not done in v1; flagged so RFC-010 (retention/cost) and product can decide. Does not block this RFC.

2. Entitlement override vs derived. The `entitlements` table is stored per-org so a comped/enterprise override does not need a fake plan row. The open question for RFC-009: is the row always present (populated from the plan on every plan change) or absent-means-derive-from-plan. This RFC models it as always-present (a row per org, refreshed on plan change), which makes the read a single-row primary-key lookup and the fail-closed-on-write stance simple. RFC-009 confirms.

3. `seats.occupied_id` as a discriminated reference. A seat points at either a membership or an invitation (`occupied_kind` + `occupied_id`), which cannot be a single FK. The alternative was two nullable FK columns (`membership_id`, `invitation_id`) with a check that exactly one is set. The discriminated form is chosen for compactness; if seat queries that join through the reference become common, two nullable FKs (joinable directly) may be better. Flagged for RFC-001 implementation review; not load-bearing on any other RFC.

### 11.3 Notes where this RFC sharpens a PRD/RFC-000 detail

- Timestamps move from v1's "TEXT RFC3339" to `TIMESTAMPTZ`. This is a deliberate deviation from the v1 storage representation (RFC-000 14 says the store is replaced); the wire format stays RFC3339, produced at serialization. Called out so nobody ports the v1 TEXT-time helpers (`formatTime`/`parseTime` in `store.go`) into the Postgres store.
- `DeleteResultsBefore` from the v1 `Store` interface is removed. Retention is partition drop plus the rollup job's per-org prune (section 6.2). Any code carried from v1 that called it must move to the new model. Called out because it is the one v1 Store method that does not port.
- The down-policy denominator (healthy-reporting regions R) is RFC-008/RFC-006 logic, not schema. This RFC only stores `monitors.down_policy` and `monitors.regions[]`; the verdict math reads `region_health` at decision time. No schema choice here constrains the quorum math.
```
