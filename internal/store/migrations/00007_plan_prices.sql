-- +goose Up
-- plan_prices maps a catalog plan + cycle to the billing provider's price id
-- (RFC-018 6), so checkout knows which price to charge. Platform catalog (no org,
-- no RLS), like plan config; it just needs the grant. trial_days is the native
-- provider trial for display; custom_data is a jsonb passthrough of the provider's
-- price metadata so we can keep extra fields without a schema change. Custom is NOT
-- here (per-org price, RFC-018 7). Rows are seeded per deployment (live vs sandbox
-- price ids differ), not in this migration.
CREATE TABLE plan_prices (
  id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  provider          TEXT NOT NULL,
  plan              TEXT NOT NULL,                 -- tier2 | tier3 (paid catalog tiers)
  cycle             TEXT NOT NULL,                 -- monthly | annual
  provider_price_id TEXT NOT NULL,
  trial_days        INT NOT NULL DEFAULT 0,
  custom_data       JSONB NOT NULL DEFAULT '{}',
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_plan_prices ON plan_prices (provider, plan, cycle);

GRANT SELECT, INSERT, UPDATE, DELETE ON plan_prices TO pulse_app;

-- Also store the raw, verified webhook payload for every billing event (RFC-018 8), so
-- we keep a full audit/debug trail and can reprocess if our handling changes. jsonb so
-- we can query into it. Folded in here rather than a separate migration because this
-- billing batch is not applied yet and billing_events' own migration (00004) is already
-- committed, so we don't edit it.
ALTER TABLE billing_events ADD COLUMN payload JSONB NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE billing_events DROP COLUMN IF EXISTS payload;
DROP TABLE IF EXISTS plan_prices;
