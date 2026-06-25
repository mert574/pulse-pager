-- +goose Up
-- Trial eligibility (RFC-018). Two parts.
--
-- 1) A plan/cycle now has two provider prices: one with a free trial (new customers) and
-- one without it (people who recently had a subscription, so they don't get another
-- trial). has_trial tells them apart. Existing rows are the trialled ones, so it defaults
-- to true; the trialless rows are seeded with false. The unique key gains has_trial so
-- both prices can live under the same plan/cycle.
ALTER TABLE plan_prices ADD COLUMN has_trial BOOLEAN NOT NULL DEFAULT true;
DROP INDEX IF EXISTS idx_plan_prices;
CREATE UNIQUE INDEX idx_plan_prices ON plan_prices (provider, plan, cycle, has_trial);

-- 2) A person is denied a new free trial if they recently controlled a subscription that
-- ended: any org where they are owner/admin whose subscription is non-active (not active
-- or trialing) and was last touched within the window. SECURITY DEFINER so it reads
-- across the person's orgs without per-org RLS, returning a boolean only, never row data;
-- search_path is pinned, matching the audited 00003/00004 definer functions.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION person_had_recent_inactive_subscription(p_user_id bigint, p_days int)
  RETURNS boolean
  LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT EXISTS(
    SELECT 1
    FROM subscriptions s
    JOIN memberships m ON m.org_id = s.org_id
    WHERE m.user_id = p_user_id
      AND m.role IN ('owner', 'admin')
      AND s.status NOT IN ('active', 'trialing')
      AND s.updated_at > now() - make_interval(days => p_days)
  )
$$;
-- +goose StatementEnd
GRANT EXECUTE ON FUNCTION person_had_recent_inactive_subscription(bigint, int) TO pulse_app;

-- +goose Down
DROP FUNCTION IF EXISTS person_had_recent_inactive_subscription(bigint, int);
DELETE FROM plan_prices WHERE has_trial = false;
DROP INDEX IF EXISTS idx_plan_prices;
CREATE UNIQUE INDEX idx_plan_prices ON plan_prices (provider, plan, cycle);
ALTER TABLE plan_prices DROP COLUMN IF EXISTS has_trial;
