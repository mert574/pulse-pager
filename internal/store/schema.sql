-- Single source-of-truth schema for early development. There are no incremental
-- migrations yet: this whole file is applied to a fresh (or reset) database by
-- store.ApplySchema. It drops the known tables first, so re-running it resets the
-- schema. Once the data model stabilizes we switch to versioned migrations.

DROP TABLE IF EXISTS org_webhooks;
DROP TABLE IF EXISTS notify_deliveries;
DROP TABLE IF EXISTS notify_dedup;
DROP TABLE IF EXISTS channels;
-- CASCADE: the public-lookup RLS policies on monitors/incidents/check_results
-- reference these two tables, so a plain DROP would fail while those tables still
-- exist. CASCADE drops the dependent policies with them; the referencing tables are
-- recreated below with the policies, so the reset stays clean.
DROP TABLE IF EXISTS status_page_monitors CASCADE;
DROP TABLE IF EXISTS status_pages CASCADE;
DROP TABLE IF EXISTS monitor_last_failure;
DROP TABLE IF EXISTS incident_annotations;
DROP TABLE IF EXISTS incidents;
DROP TABLE IF EXISTS check_results;
DROP TABLE IF EXISTS monitors;
-- identity tables (added with the identity/onboarding work package). Dropped
-- before organizations because they reference it.
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS invitations;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS user_identities;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS organizations;

-- Restricted role that RLS applies to. Services that handle user requests connect
-- as this role so a missed org filter fails safe (RFC-001 6.1). Created idempotently
-- since roles are cluster-level and survive a table reset. Password is a dev placeholder.
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'pulse_app') THEN
    CREATE ROLE pulse_app LOGIN PASSWORD 'pulse_app';
  END IF;
END
$$;

-- Global. The one cross-org row (RFC-001 4.1). A person, not org-scoped, so no
-- org_id and no org RLS: a user spans orgs. Access is mediated by the service
-- layer (auth path), which scopes by the authenticated user, not by org. See the
-- note above user_identities for why this is safe (RFC-001 5.2).
-- locale/timezone are the i18n preference columns (RFC-014 section 9): default to
-- en/UTC so v1 is correct with no backfill.
CREATE TABLE users (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  email         TEXT        NOT NULL,                 -- verified identity anchor
  email_verified BOOLEAN    NOT NULL DEFAULT false,
  name          TEXT        NOT NULL DEFAULT '',
  avatar_url    TEXT        NOT NULL DEFAULT '',
  locale        TEXT        NOT NULL DEFAULT 'en',     -- BCP-47 tag (RFC-014 9)
  timezone      TEXT        NOT NULL DEFAULT 'UTC',    -- IANA zone name (RFC-014 8/9)
  status        TEXT        NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active','deletion-pending','deleted')),
  deletion_pending_at TIMESTAMPTZ,                     -- set when status -> deletion-pending (RFC-015 14-day grace)
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_login_at TIMESTAMPTZ
);
-- email is the verified-email account-linking anchor (PRD-001 3.3). Unique by
-- lower(email) so casing does not split an identity; excludes hard-deleted rows.
CREATE UNIQUE INDEX uniq_users_email ON users (lower(email)) WHERE status <> 'deleted';

-- Global. A user has 1..2 identities (google/github). Account linking on verified
-- email (PRD-001 3.3, RFC-003 2.4). provider is a free TEXT with a CHECK rather
-- than a Postgres ENUM, so adding 'sso' later (RFC-016) is one CHECK edit in a
-- migration, no type alter. provider_user_id is the provider's stable subject id.
-- Not org-scoped (keyed by user_id); reached only through the auth path, which
-- scopes by the authenticated user (RFC-001 5.2). Not under org RLS by design.
CREATE TABLE user_identities (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id          BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider         TEXT   NOT NULL CHECK (provider IN ('google','github','dev')), -- 'sso' added later (RFC-016); 'dev' is the local dev-login identity
  provider_user_id TEXT   NOT NULL,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- I4: one identity per (user, provider). I5: a provider account maps to one user.
CREATE UNIQUE INDEX uniq_identity_user_provider ON user_identities (user_id, provider);
CREATE UNIQUE INDEX uniq_identity_provider_subject ON user_identities (provider, provider_user_id);

-- Tenant root (RFC-001 4.1). RLS keys off its own id, not an org_id column (5.2).
-- plan_id is nullable here because the plans/subscriptions/entitlements catalog is
-- a later work package (RFC-001 4.2): no FK yet, filled in when those tables land.
-- default_locale/default_timezone are the tenant i18n defaults (RFC-014 9).
-- deleted_at marks the start of the 14-day soft-delete grace (RFC-015): a non-null
-- value means deletion-pending, the row is recoverable until the grace cascade runs.
CREATE TABLE organizations (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name             TEXT NOT NULL,
  slug             TEXT NOT NULL,                      -- shapes {slug}.pulse.app
  plan_id          BIGINT,                             -- nullable until the plans table exists (RFC-001 4.2)
  -- the org's billing tier (free/starter/team/business). An operator sets it directly
  -- until Stripe lands (PRD-006 6/8.3); the entitlement resolvers read it. Distinct
  -- from plan_id, which will point at the future plans/subscriptions catalog.
  plan             TEXT NOT NULL DEFAULT 'tier1',
  default_locale   TEXT NOT NULL DEFAULT 'en',         -- tenant default locale (RFC-014 9)
  default_timezone TEXT NOT NULL DEFAULT 'UTC',        -- tenant default zone (RFC-014 9)
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at       TIMESTAMPTZ                         -- non-null => 14-day soft-delete grace (RFC-015)
);
-- slug unique globally, excluding soft-deleted orgs so a name can be reused after deletion.
CREATE UNIQUE INDEX uniq_org_slug ON organizations (lower(slug)) WHERE deleted_at IS NULL;

CREATE TABLE monitors (
  id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id                BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  type                  TEXT    NOT NULL DEFAULT 'http',
  name                  TEXT    NOT NULL,
  url                   TEXT    NOT NULL,
  method                TEXT    NOT NULL DEFAULT 'GET',
  body                  TEXT    NOT NULL DEFAULT '',
  expected_status_codes TEXT    NOT NULL DEFAULT '200',
  timeout_seconds       INT     NOT NULL DEFAULT 10,
  interval_seconds      INT     NOT NULL DEFAULT 60,
  enabled               BOOLEAN NOT NULL DEFAULT true,
  max_latency_ms        INT,
  body_contains         TEXT,
  failure_threshold     INT     NOT NULL DEFAULT 1,
  regions               TEXT[]  NOT NULL DEFAULT '{home}',
  down_policy           TEXT    NOT NULL DEFAULT 'quorum',
  -- request headers (PRD-002 2.2). One JSON array of {key, value, secret}; a secret
  -- value is encrypted at rest (the crypto cipher) and redacted on read. Stored as
  -- JSONB so the array travels as one column with the rest of the monitor config.
  headers               JSONB   NOT NULL DEFAULT '[]'::jsonb,
  -- attached notification channels (PRD-002 2.2). Empty = tracked silently (master 6.4).
  notification_channel_ids BIGINT[] NOT NULL DEFAULT '{}',
  consecutive_fails     INT     NOT NULL DEFAULT 0,   -- alert state, survives restart (step 2)
  first_fail_at         TIMESTAMPTZ,                  -- first fail of current run (step 2)
  -- alerting redelivery watermark (RFC-006 section 5.3): the largest check_results.id
  -- whose round has been applied to this monitor's alert state. A re-delivered or
  -- older result has an id <= this, so the apply is a no-op. nullable = never applied yet.
  last_applied_result_id BIGINT,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_monitors_enabled ON monitors (enabled) WHERE enabled;

ALTER TABLE monitors ENABLE ROW LEVEL SECURITY;
ALTER TABLE monitors FORCE ROW LEVEL SECURITY;
CREATE POLICY monitors_org_isolation ON monitors
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);

-- Org-scoped (PRD-001 4.1, RFC-001 2): a user's role inside one org. Carries
-- org_id and gets org RLS like monitors. unique(org_id, user_id): a user has at
-- most one membership per org (I3). idx_membership_owner backs the owner-count
-- guard below and CountOwners.
CREATE TABLE memberships (
  id        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id    BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  user_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role      TEXT   NOT NULL CHECK (role IN ('owner','admin','member','viewer')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, user_id)
);
CREATE INDEX idx_membership_user ON memberships (user_id);
CREATE INDEX idx_membership_owner ON memberships (org_id) WHERE role = 'owner';

ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY memberships_org_isolation ON memberships
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- The "orgs I belong to" read (PRD-001 7.3) is user-scoped, not org-scoped: it
-- spans every org the user is a member of, so it cannot set one app.current_org.
-- This read-only capability lets a caller see its own membership rows when it sets
-- app.current_user. Policies are OR'd, so this only lets a user read its own
-- memberships across orgs; it does not widen org-scoped reads. Writes still go
-- through the org policy.
CREATE POLICY memberships_self_read ON memberships
  FOR SELECT
  USING (user_id = NULLIF(current_setting('app.current_user', true), '')::bigint);

-- At-least-one-owner invariant (PRD-001 I1, master 4). RFC-001 4.1 enforces this
-- in the repository transaction and RFC-003 7.5 asks for a DB backstop too. This
-- trigger is the backstop: it blocks any delete or role-change-away-from-owner
-- that would leave an active (not soft-deleted) org with zero owners, even under a
-- race. The service layer still refuses the last-owner action with a friendly
-- message ("transfer ownership first"); this just makes the DB fail closed.
CREATE OR REPLACE FUNCTION enforce_last_owner() RETURNS trigger AS $$
DECLARE
  org BIGINT;
  owner_count INT;
BEGIN
  -- Only care about a row that was an owner and is leaving the owner set.
  IF TG_OP = 'UPDATE' AND NEW.role = 'owner' THEN
    RETURN NEW; -- still an owner, no invariant risk
  END IF;
  org := OLD.org_id;
  -- If the org itself is gone or soft-deleted, the cascade/delete is allowed.
  IF NOT EXISTS (SELECT 1 FROM organizations WHERE id = org AND deleted_at IS NULL) THEN
    IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
  END IF;
  SELECT count(*) INTO owner_count FROM memberships
    WHERE org_id = org AND role = 'owner' AND id <> OLD.id;
  IF owner_count = 0 THEN
    RAISE EXCEPTION 'cannot remove or demote the last owner of org %', org
      USING ERRCODE = 'check_violation';
  END IF;
  IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_memberships_last_owner
  BEFORE UPDATE OF role OR DELETE ON memberships
  FOR EACH ROW EXECUTE FUNCTION enforce_last_owner();

-- Org-scoped (PRD-001 4.1). A pending offer to join an org. token is HASHED
-- (SHA-256 of the raw token, which lives only in the email link). state is the
-- invitation state machine (pending -> accepted/revoked/expired, PRD-001 6).
-- locale carries the invite-email locale so a cold invite localizes before the
-- user exists (RFC-014 7/9). Seat reservation is service-layer (PRD-001 5.1); the
-- columns model what the service needs (target_role, expires_at = created + 7d).
CREATE TABLE invitations (
  org_id      BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  email       TEXT   NOT NULL,
  role        TEXT   NOT NULL CHECK (role IN ('owner','admin','member','viewer')),
  state       TEXT   NOT NULL DEFAULT 'pending'
               CHECK (state IN ('pending','accepted','revoked','expired')),
  token_hash  TEXT   NOT NULL,                          -- HASHED (sha-256); raw token is in the email only
  locale      TEXT   NOT NULL DEFAULT 'en',             -- invite-email locale (RFC-014 7/9)
  created_by  BIGINT REFERENCES users(id) ON DELETE SET NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at  TIMESTAMPTZ NOT NULL,                     -- created_at + 7 days
  accepted_at TIMESTAMPTZ
);
-- I7: at most one pending invite per (org, email).
CREATE UNIQUE INDEX uniq_invite_pending ON invitations (org_id, lower(email)) WHERE state = 'pending';
CREATE UNIQUE INDEX uniq_invite_token ON invitations (token_hash);
CREATE INDEX idx_invite_org ON invitations (org_id);
CREATE INDEX idx_invite_expiry ON invitations (expires_at) WHERE state = 'pending';

ALTER TABLE invitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE invitations FORCE ROW LEVEL SECURITY;
CREATE POLICY invitations_org_isolation ON invitations
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- The invite-accept flow (RFC-003 2.6) loads an invitation by its token before any
-- org is in context (the public /invite/{token} page is pre-login). The token is an
-- unguessable capability, so a row whose token_hash matches the session-set
-- app.invite_token is readable without an org scope. Policies are OR'd, so this
-- only widens reads to the one row the caller already holds the token for; it does
-- not open cross-org listing (that still needs app.current_org). Read-only: writes
-- (accept/revoke) still go through the org-scoped policy.
CREATE POLICY invitations_token_lookup ON invitations
  FOR SELECT
  USING (token_hash = NULLIF(current_setting('app.invite_token', true), ''));

-- Global (keyed by user_id, not org). Opaque refresh tokens, hashed (RFC-003 4).
-- Rotation keeps the family_id; replaced_by points at the token that rotated this
-- one, so presenting an already-rotated token (one with a non-null replaced_by) is
-- the reuse-detection signal and the whole family gets revoked. token_hash is
-- SHA-256 of the raw token. Not org-scoped, no org RLS: reached only through the
-- auth path, scoped by the authenticated user (RFC-001 5.2). Access tokens are
-- stateless JWTs and are not stored.
CREATE TABLE refresh_tokens (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  family_id   BIGINT NOT NULL,                          -- one login chain; rotation keeps the family
  replaced_by BIGINT REFERENCES refresh_tokens(id),     -- set on rotation; non-null => already used
  token_hash  TEXT   NOT NULL,                          -- HASHED (sha-256 of the opaque token)
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at  TIMESTAMPTZ NOT NULL,
  revoked_at  TIMESTAMPTZ                               -- non-null => revoked
);
CREATE UNIQUE INDEX uniq_refresh_token_hash ON refresh_tokens (token_hash);
CREATE INDEX idx_refresh_user ON refresh_tokens (user_id) WHERE revoked_at IS NULL;
CREATE INDEX idx_refresh_family ON refresh_tokens (family_id);
CREATE INDEX idx_refresh_expires ON refresh_tokens (expires_at);

-- Org-scoped (PRD-005 2.1, RFC-003 5). A per-org API key for the public REST
-- surface. token_hash is the SHA-256 of the full presented key (pulse_sk_<...>);
-- the secret is shown once at creation and never stored in clear (RFC-003 5.2).
-- prefix is the non-secret leading chars, safe to list and log. role is member or
-- admin only (no owner-equivalent keys, PRD-001 App A). org is fixed by the key:
-- verify returns (org_id, role) with no JWT and no org header (RFC-003 5.4).
-- revoked_at non-null fails the key; revocation also busts the Redis cache.
CREATE TABLE api_keys (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id       BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  name         TEXT   NOT NULL DEFAULT '',
  prefix       TEXT   NOT NULL,                          -- non-secret leading chars (for the list/logs)
  token_hash   TEXT   NOT NULL,                          -- SHA-256 of the full pulse_sk_ key
  role         TEXT   NOT NULL CHECK (role IN ('member','admin')), -- keys max out at admin (PRD-001 App A)
  created_by   BIGINT REFERENCES users(id) ON DELETE SET NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at TIMESTAMPTZ,                              -- throttled async update on use (RFC-003 5.3)
  revoked_at   TIMESTAMPTZ                               -- non-null => revoked
);
CREATE UNIQUE INDEX uniq_api_key_hash ON api_keys (token_hash);
CREATE INDEX idx_api_key_org ON api_keys (org_id);

ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE api_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY api_keys_org_isolation ON api_keys
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- Verify happens before any org is in context: the presented key is the credential
-- and the key row carries its own org (RFC-003 5.4), so the by-hash lookup runs
-- with app.api_key_hash set and this capability policy lets the one matching row
-- through. Read-only; the secret is the unguessable capability. Listing and
-- create/revoke still go through the org-scoped policy.
CREATE POLICY api_keys_hash_lookup ON api_keys
  FOR SELECT
  USING (token_hash = NULLIF(current_setting('app.api_key_hash', true), ''));

CREATE TABLE check_results (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id         BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  monitor_id     BIGINT  NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  region         TEXT    NOT NULL,
  -- the scheduler tick this check belongs to. Same value across every region of one
  -- run, so the api/ui group a run's per-region rows into a single row. checked_at is
  -- the real run time and differs per region, so it cannot be used for grouping.
  scheduled_at   TIMESTAMPTZ NOT NULL,
  checked_at     TIMESTAMPTZ NOT NULL,
  healthy        BOOLEAN NOT NULL,
  failure_reason TEXT,
  status_code    INT,
  latency_ms     INT,
  error_text     TEXT,
  -- idempotency: a redelivered job re-writes the same row (RFC-002 6.2)
  UNIQUE (org_id, monitor_id, region, checked_at)
);
CREATE INDEX idx_results_monitor_time ON check_results (monitor_id, checked_at DESC);

ALTER TABLE check_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE check_results FORCE ROW LEVEL SECURITY;
CREATE POLICY check_results_org_isolation ON check_results
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);

-- Incidents: one row per outage run (PRD-002 4). started_at is the first failing
-- check of the run; ended_at is null while open. cause_reason is the primary failure
-- reason that opened it; close_reason/closed_by are set on close. Org-scoped, cascades
-- with the monitor. The alerting engine owns the open/close writes; the api reads them
-- and does manual closes (member+ ack, admin+ close, PRD-001 7.2).
CREATE TABLE incidents (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id          BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  monitor_id      BIGINT  NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  started_at      TIMESTAMPTZ NOT NULL,
  ended_at        TIMESTAMPTZ,                -- null while open
  cause_reason    TEXT    NOT NULL,
  close_reason    TEXT    CHECK (close_reason IN ('recovered','disabled','manual')),
  closed_by       BIGINT  REFERENCES users(id) ON DELETE SET NULL,
  first_result_id BIGINT  REFERENCES check_results(id) ON DELETE SET NULL
);
CREATE INDEX idx_incidents_monitor_time ON incidents (monitor_id, started_at DESC);
-- At most one open incident per monitor (one run at a time).
CREATE UNIQUE INDEX uniq_incident_open ON incidents (monitor_id) WHERE ended_at IS NULL;

ALTER TABLE incidents ENABLE ROW LEVEL SECURITY;
ALTER TABLE incidents FORCE ROW LEVEL SECURITY;
CREATE POLICY incidents_org_isolation ON incidents
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);

-- Incident annotations: the operator timeline on an incident (PRD-002 4). One row
-- per note an org member adds while triaging. author_user_id is the writer; it stays
-- when the user is removed (ON DELETE SET NULL) so the note keeps its history.
-- Org-scoped, RLS like incidents, cascades with the incident and the org.
CREATE TABLE incident_annotations (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id         BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  incident_id    BIGINT  NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  author_user_id BIGINT  REFERENCES users(id) ON DELETE SET NULL,
  note           TEXT    NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_incident_annotations_incident ON incident_annotations (incident_id, created_at);

ALTER TABLE incident_annotations ENABLE ROW LEVEL SECURITY;
ALTER TABLE incident_annotations FORCE ROW LEVEL SECURITY;
CREATE POLICY incident_annotations_org_isolation ON incident_annotations
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);

-- Last failed check's response per monitor (PRD-002 3.8). One row per monitor,
-- overwritten on each failure, so it stays off the high-volume check_results table.
-- Operational data, not secret: stored plaintext, org-scoped, never on public pages.
CREATE TABLE monitor_last_failure (
  monitor_id  BIGINT PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE,
  org_id      BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  checked_at  TIMESTAMPTZ NOT NULL,
  status_code INT,
  headers     JSONB   NOT NULL DEFAULT '{}'::jsonb,
  body        TEXT    NOT NULL DEFAULT '',
  truncated   BOOLEAN NOT NULL DEFAULT false,
  captured_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_last_failure_org ON monitor_last_failure (org_id);

ALTER TABLE monitor_last_failure ENABLE ROW LEVEL SECURITY;
ALTER TABLE monitor_last_failure FORCE ROW LEVEL SECURITY;
CREATE POLICY monitor_last_failure_org_isolation ON monitor_last_failure
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);

-- Status pages (PRD-004). A public, shareable page scoped to one org that shows
-- whether selected services are up. It is a presentation layer over the same
-- monitor/incident data; it never probes. branding is name/logo/theme/accent only
-- (no full theming, PRD-004 2.2). state is draft|published; only published pages are
-- publicly reachable (PRD-004 6). slug is the public path segment, globally unique so
-- the public data endpoint can resolve a page by slug alone with no org in context
-- (the org-subdomain {org-slug}.pulse.app fronts this; the data endpoint takes the
-- page slug). custom_domain is reserved for the phased paid feature (PRD-004 9.1),
-- nullable and unused in v1. Org-scoped, RLS, cascades with the org.
CREATE TABLE status_pages (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id        BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  name          TEXT    NOT NULL,
  slug          TEXT    NOT NULL,
  logo_url      TEXT    NOT NULL DEFAULT '',
  accent_color  TEXT    NOT NULL DEFAULT '',
  theme         TEXT    NOT NULL DEFAULT 'light' CHECK (theme IN ('light','dark')),
  published     BOOLEAN NOT NULL DEFAULT false,
  custom_domain TEXT,                                  -- phased (PRD-004 9.1), owner/admin only
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- slug unique globally (the public data endpoint resolves a page by slug alone).
CREATE UNIQUE INDEX uniq_status_page_slug ON status_pages (lower(slug));
CREATE INDEX idx_status_pages_org ON status_pages (org_id);

ALTER TABLE status_pages ENABLE ROW LEVEL SECURITY;
ALTER TABLE status_pages FORCE ROW LEVEL SECURITY;
CREATE POLICY status_pages_org_isolation ON status_pages
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- The public page is served before any org is in context (the published page is
-- public by design, PRD-004 6/10). A published page whose slug matches the
-- session-set app.public_page_slug is readable without an org scope. Policies are
-- OR'd, so this only widens reads to the one published page the caller named; a
-- draft (published = false) never matches, so a draft's existence is not leaked
-- (PRD-004 6). Read-only: writes still go through the org-scoped policy.
CREATE POLICY status_pages_public_lookup ON status_pages
  FOR SELECT
  USING (published AND lower(slug) = lower(NULLIF(current_setting('app.public_page_slug', true), '')));

-- The displayed-monitor join (PRD-004 3): an ordered list of monitors shown on a
-- status page, each with a friendly public display_name that is the ONLY label the
-- public sees (the raw monitor url is never derived from this, PRD-004 3.1/3.6).
-- Org-scoped (carries org_id so RLS keys off it like every tenant table), cascades
-- with both the page and the monitor: deleting a monitor drops it from the page
-- automatically (PRD-004 12.3 edge case).
CREATE TABLE status_page_monitors (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id         BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  status_page_id BIGINT  NOT NULL REFERENCES status_pages(id) ON DELETE CASCADE,
  monitor_id     BIGINT  NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  display_name   TEXT    NOT NULL,
  sort_order     INT     NOT NULL DEFAULT 0,
  UNIQUE (status_page_id, monitor_id)
);
CREATE INDEX idx_status_page_monitors_page ON status_page_monitors (status_page_id, sort_order);

ALTER TABLE status_page_monitors ENABLE ROW LEVEL SECURITY;
ALTER TABLE status_page_monitors FORCE ROW LEVEL SECURITY;
CREATE POLICY status_page_monitors_org_isolation ON status_page_monitors
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- Public read: the displayed-monitor rows of a published page named by
-- app.public_page_slug are readable without an org scope, mirroring the page policy.
-- This only exposes the join row (monitor_id + friendly display_name + order); the
-- raw monitor secret/internal columns are never selected into the public projection.
CREATE POLICY status_page_monitors_public_lookup ON status_page_monitors
  FOR SELECT
  USING (EXISTS (
    SELECT 1 FROM status_pages sp
    WHERE sp.id = status_page_monitors.status_page_id
      AND sp.published
      AND lower(sp.slug) = lower(NULLIF(current_setting('app.public_page_slug', true), ''))));

-- Public read of the monitor/incident/check-result data behind a published page.
-- These widen SELECT (only) to rows whose monitor is displayed on the published page
-- named by app.public_page_slug, so the public projection can read the derived
-- status, uptime, and incidents without an org scope. They are OR'd with the
-- org-isolation policies, so they never open cross-org listing: the caller must name
-- a published page, and only that page's displayed monitors are reachable. The public
-- store NEVER selects the secret/internal monitor columns (url, headers, body,
-- assertions) into the public DTO; the privacy boundary is the projection (PRD-004 3.6).
CREATE POLICY monitors_public_lookup ON monitors
  FOR SELECT
  USING (EXISTS (
    SELECT 1 FROM status_page_monitors spm
    JOIN status_pages sp ON sp.id = spm.status_page_id
    WHERE spm.monitor_id = monitors.id
      AND sp.published
      AND lower(sp.slug) = lower(NULLIF(current_setting('app.public_page_slug', true), ''))));
CREATE POLICY check_results_public_lookup ON check_results
  FOR SELECT
  USING (EXISTS (
    SELECT 1 FROM status_page_monitors spm
    JOIN status_pages sp ON sp.id = spm.status_page_id
    WHERE spm.monitor_id = check_results.monitor_id
      AND sp.published
      AND lower(sp.slug) = lower(NULLIF(current_setting('app.public_page_slug', true), ''))));
CREATE POLICY incidents_public_lookup ON incidents
  FOR SELECT
  USING (EXISTS (
    SELECT 1 FROM status_page_monitors spm
    JOIN status_pages sp ON sp.id = spm.status_page_id
    WHERE spm.monitor_id = incidents.monitor_id
      AND sp.published
      AND lower(sp.slug) = lower(NULLIF(current_setting('app.public_page_slug', true), ''))));

-- Notification channels (PRD-003, RFC-001 4.3). A channel a monitor attaches to
-- via monitors.notification_channel_ids. config is the type-specific settings as
-- JSONB; secret subfields (slack/discord webhook_url, webhook url + custom_headers
-- values, smtp password) are encrypted at rest with the crypto cipher and decrypted
-- in memory on read (mirrors the monitor headers pattern). Org-scoped, RLS like
-- monitors, cascades with the org.
CREATE TABLE channels (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id     BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  name       TEXT    NOT NULL,
  type       TEXT    NOT NULL,             -- slack/discord/webhook/smtp/... (notify descriptor types)
  config     JSONB   NOT NULL DEFAULT '{}'::jsonb,
  enabled    BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_channels_org ON channels (org_id);

ALTER TABLE channels ENABLE ROW LEVEL SECURITY;
ALTER TABLE channels FORCE ROW LEVEL SECURITY;
CREATE POLICY channels_org_isolation ON channels
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);

-- Notify dedup backstop (RFC-007 section 4.2, RFC-001 4.6). The durable "this
-- notify event was handled" marker behind the Redis fast path. dedup_id is
-- hex(sha256(incident_id, event_type)); the unique (org_id, dedup_id) makes a
-- redelivered event a no-op insert so a duplicate is caught even after the Redis
-- key is evicted. created_at supports a background prune. Org-scoped, RLS.
CREATE TABLE notify_dedup (
  org_id     BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  dedup_id   TEXT   NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, dedup_id)
);
CREATE INDEX idx_notify_dedup_created ON notify_dedup (created_at);

ALTER TABLE notify_dedup ENABLE ROW LEVEL SECURITY;
ALTER TABLE notify_dedup FORCE ROW LEVEL SECURITY;
CREATE POLICY notify_dedup_org_isolation ON notify_dedup
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);

-- Per-(notify event, channel) delivery outcome (RFC-007 section 6.1). The incident
-- timeline reads this so the team sees that channel X was delivered or failed.
-- Keyed by (org_id, incident_id, channel_id, event_type) so a redelivery upserts
-- rather than duplicates, and a later recovery for the same incident writes its own
-- row (different event_type). Org-scoped, RLS, cascades with the channel.
CREATE TABLE notify_deliveries (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id       BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  incident_id  BIGINT  NOT NULL,
  channel_id   BIGINT  NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  event_type   TEXT    NOT NULL,           -- down / recovery
  status       TEXT    NOT NULL CHECK (status IN ('delivered','failed')),
  attempts     INT     NOT NULL DEFAULT 0,
  last_error   TEXT,
  delivered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, incident_id, channel_id, event_type)
);
CREATE INDEX idx_notify_deliveries_incident ON notify_deliveries (incident_id);

ALTER TABLE notify_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE notify_deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY notify_deliveries_org_isolation ON notify_deliveries
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);

-- Org-level outbound webhooks (PRD-005 section 7, RFC-007 section 7). Distinct from
-- the per-monitor generic-webhook channel: this is a programmatic event feed for the
-- whole org. signing_secret is encrypted at rest (crypto cipher), decrypted in memory
-- by the deliverer, and shown to the user exactly once at create/rotate. events is the
-- subscribed event-type list; an empty array means all types. last_delivery_* records
-- the most recent outcome so a broken receiver is visible. Org-scoped, RLS like
-- channels, cascades with the org.
CREATE TABLE org_webhooks (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id           BIGINT  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  url              TEXT    NOT NULL,
  signing_secret   TEXT    NOT NULL,             -- encrypted at rest; never returned after create/rotate
  enabled          BOOLEAN NOT NULL DEFAULT true,
  events           TEXT[]  NOT NULL DEFAULT '{}'::text[], -- subscribed event types; empty = all
  created_by       BIGINT  REFERENCES users(id) ON DELETE SET NULL,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_delivery_at TIMESTAMPTZ,
  last_status      TEXT    CHECK (last_status IN ('delivered','failed')),
  last_error       TEXT
);
CREATE INDEX idx_org_webhooks_org ON org_webhooks (org_id);

ALTER TABLE org_webhooks ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_webhooks FORCE ROW LEVEL SECURITY;
CREATE POLICY org_webhooks_org_isolation ON org_webhooks
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);

GRANT SELECT, INSERT, UPDATE, DELETE ON organizations TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON users TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON user_identities TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON memberships TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON invitations TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON refresh_tokens TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON api_keys TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON monitors TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON check_results TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON incidents TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON incident_annotations TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON monitor_last_failure TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON status_pages TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON status_page_monitors TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON channels TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON notify_dedup TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON notify_deliveries TO pulse_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON org_webhooks TO pulse_app;

-- Platform admin metrics need cross-org counts on the RLS-protected tables
-- (monitors, channels). A normal query with no app.current_org set returns zero
-- rows under FORCE ROW LEVEL SECURITY, so the counts go through SECURITY DEFINER
-- functions: they run as the owner (the superuser that applies this schema), which
-- bypasses RLS. The functions only return aggregate counts, never row data, so they
-- cannot leak one org's monitors to another. EXECUTE is granted to pulse_app so the
-- count works whether the app connects as the owner or the restricted role.
CREATE OR REPLACE FUNCTION platform_monitor_counts(OUT total bigint, OUT enabled bigint)
  LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT count(*), count(*) FILTER (WHERE enabled) FROM monitors
$$;

CREATE OR REPLACE FUNCTION platform_channel_count() RETURNS bigint
  LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT count(*) FROM channels
$$;

GRANT EXECUTE ON FUNCTION platform_monitor_counts() TO pulse_app;
GRANT EXECUTE ON FUNCTION platform_channel_count() TO pulse_app;

-- Activation: how many orgs ever created a monitor, and the median time from org
-- signup to its first monitor (seconds). Joins the RLS-protected monitors table to
-- organizations, so it is SECURITY DEFINER like the counts above. median is NULL
-- when no org has a monitor yet.
CREATE OR REPLACE FUNCTION platform_activation(
    OUT orgs_with_monitor bigint,
    OUT median_ttfm_seconds double precision)
  LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  WITH first_mon AS (
    SELECT org_id, min(created_at) AS first_at FROM monitors GROUP BY org_id
  )
  SELECT count(*),
         percentile_cont(0.5) WITHIN GROUP (
           ORDER BY EXTRACT(EPOCH FROM (fm.first_at - o.created_at)))
  FROM first_mon fm JOIN organizations o ON o.id = fm.org_id
$$;

-- Active orgs: orgs with at least one enabled monitor that got a check in the last
-- 7 days. The "signed up and actually using it" signal. Touches monitors and
-- check_results (both RLS-protected), so SECURITY DEFINER.
CREATE OR REPLACE FUNCTION platform_active_orgs_7d() RETURNS bigint
  LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT count(DISTINCT m.org_id)
  FROM monitors m
  WHERE m.enabled
    AND EXISTS (
      SELECT 1 FROM check_results cr
      WHERE cr.monitor_id = m.id
        AND cr.checked_at > now() - interval '7 days'
    )
$$;

GRANT EXECUTE ON FUNCTION platform_activation() TO pulse_app;
GRANT EXECUTE ON FUNCTION platform_active_orgs_7d() TO pulse_app;
