-- +goose Up
-- Cross-org monitor counts split by check type, for the admin panel (BACKLOG:
-- SSL-expiry). Like platform_monitor_counts it is SECURITY DEFINER so it bypasses
-- the per-org RLS on monitors; it returns aggregate counts only, never row data.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION platform_monitor_counts_by_type()
  RETURNS TABLE(monitor_type text, count bigint)
  LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
  SELECT type, count(*) FROM monitors GROUP BY type
$$;
-- +goose StatementEnd
GRANT EXECUTE ON FUNCTION platform_monitor_counts_by_type() TO pulse_app;

-- +goose Down
DROP FUNCTION IF EXISTS platform_monitor_counts_by_type();
