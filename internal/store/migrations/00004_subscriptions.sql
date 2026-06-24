-- +goose Up
-- Billing foundation (RFC-018 Phase 1): the provider sync path writes subscription
-- state here and the entitlement resolvers read it off organizations.plan. Only the
-- two tables the sync path actually touches land now; payments (Phase 4) and
-- plan_prices (Phase 3) come with their phases.

-- subscriptions: one per org, RLS-scoped like every tenant table. provider_price_id
-- is per-row because a Custom org points at its own negotiated price (RFC-018 7), not
-- a shared catalog entry. The webhook upserts this row; entitlements read the plan.
CREATE TABLE subscriptions (
  id                       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id                   BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  plan                     TEXT NOT NULL,                 -- tier1..tierCustom (entitlements.ParsePlan)
  billing_cycle            TEXT NOT NULL,                 -- 'monthly' | 'annual'
  status                   TEXT NOT NULL,                 -- 'trialing'|'active'|'past_due'|'canceled'
  provider                 TEXT NOT NULL,                 -- 'stub' | 'paddle'
  provider_customer_id     TEXT NOT NULL DEFAULT '',
  provider_subscription_id TEXT NOT NULL DEFAULT '',
  provider_price_id        TEXT NOT NULL DEFAULT '',
  current_period_end       TIMESTAMPTZ,
  cancel_at_period_end     BOOLEAN NOT NULL DEFAULT false,
  created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_subscriptions_org ON subscriptions (org_id);
-- Follow-on events (payment, cancel) may carry only the customer id, so the ingest
-- resolves org by this when the event has no org_id in custom_data (RFC-018 Phase 3).
CREATE INDEX idx_subscriptions_customer ON subscriptions (provider, provider_customer_id);

ALTER TABLE subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY subscriptions_org_isolation ON subscriptions
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);

-- The restricted app role only has the privileges the baseline grants per table, so a
-- new table needs its own grant or every query as pulse_app is denied.
GRANT SELECT, INSERT, UPDATE, DELETE ON subscriptions TO pulse_app;

-- billing_events: the webhook idempotency ledger. Platform-scope (no org context until
-- the event is parsed), so it is NOT under RLS, but it still needs the grant. The
-- unique key is the dedup anchor under at-least-once delivery.
CREATE TABLE billing_events (
  id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  provider          TEXT NOT NULL,
  provider_event_id TEXT NOT NULL,
  type              TEXT NOT NULL,
  received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  processed_at      TIMESTAMPTZ
);
CREATE UNIQUE INDEX idx_billing_events_dedup ON billing_events (provider, provider_event_id);
GRANT SELECT, INSERT, UPDATE, DELETE ON billing_events TO pulse_app;

-- subscription_org_by_customer maps a provider customer id back to its org, for the
-- ingest path before app.current_org is known. Like platform_monitor_counts it is
-- SECURITY DEFINER so it bypasses subscriptions RLS; it returns a scalar org id only,
-- never row data. SET search_path pins resolution so the definer can't be tricked.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION subscription_org_by_customer(p_provider text, p_customer_id text)
  RETURNS bigint
  LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT org_id FROM subscriptions
  WHERE provider = p_provider AND provider_customer_id = p_customer_id
  LIMIT 1
$$;
-- +goose StatementEnd
GRANT EXECUTE ON FUNCTION subscription_org_by_customer(text, text) TO pulse_app;

-- +goose Down
DROP FUNCTION IF EXISTS subscription_org_by_customer(text, text);
DROP TABLE IF EXISTS billing_events;
DROP TABLE IF EXISTS subscriptions;
