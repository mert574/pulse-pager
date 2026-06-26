-- +goose Up
-- Trial-eligibility window anchor (RFC-018 anti-abuse). The 35-day "recently had a
-- subscription that ended" check used to key off subscriptions.updated_at, but that is
-- "row last modified", not "when it ended": any later webhook that re-writes an already
-- canceled row (a trailing provider event, a re-sync) bumps updated_at and quietly
-- restarts the window. We add a stable ended_at that we stamp once when the subscription
-- leaves active/trialing and leave alone on later same-status writes, so the window
-- reflects when access actually ended.
ALTER TABLE subscriptions ADD COLUMN ended_at TIMESTAMPTZ;

-- Backfill: for rows that are already non-active, updated_at is the best anchor we have
-- (in the common flow the cancel was the last write), so seed ended_at from it. Active
-- and trialing rows keep ended_at NULL.
UPDATE subscriptions SET ended_at = updated_at WHERE status NOT IN ('active', 'trialing');

-- The function now keys the window off ended_at instead of updated_at. ended_at is NULL
-- while active/trialing, so the comparison naturally excludes those; the status filter
-- stays as a second guard. Everything else (the owner/admin scope, the SECURITY DEFINER
-- bypass, the pinned search_path) is unchanged from 00009.
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
      AND s.ended_at > now() - make_interval(days => p_days)
  )
$$;
-- +goose StatementEnd

-- +goose Down
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
ALTER TABLE subscriptions DROP COLUMN IF EXISTS ended_at;
