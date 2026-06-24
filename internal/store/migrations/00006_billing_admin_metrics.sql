-- +goose Up
-- Cross-org billing aggregates for the operator admin panel (RFC-018). Like the
-- platform_* count functions in the baseline, these are SECURITY DEFINER so they
-- bypass the per-org RLS on subscriptions/payments and return aggregate rows only,
-- never row data. search_path is pinned so the definer can't be tricked.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION platform_subscription_counts()
  RETURNS TABLE(status text, count bigint)
  LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT status, count(*) FROM subscriptions GROUP BY status
$$;
-- +goose StatementEnd

-- Mirrored revenue is grouped by currency (money is never combined across currencies):
-- gross, total refunded, and the payment count, all from the read-only payments mirror.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION platform_payment_totals()
  RETURNS TABLE(currency text, gross bigint, refunded bigint, payments bigint)
  LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT currency, COALESCE(sum(amount), 0), COALESCE(sum(refunded_amount), 0), count(*)
  FROM payments GROUP BY currency
$$;
-- +goose StatementEnd

GRANT EXECUTE ON FUNCTION platform_subscription_counts() TO pulse_app;
GRANT EXECUTE ON FUNCTION platform_payment_totals() TO pulse_app;

-- +goose Down
DROP FUNCTION IF EXISTS platform_payment_totals();
DROP FUNCTION IF EXISTS platform_subscription_counts();
