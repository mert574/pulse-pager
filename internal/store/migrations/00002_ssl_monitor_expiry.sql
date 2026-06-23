-- +goose Up
-- SSL certificate-expiry monitor type (BACKLOG: SSL-expiry). An ssl monitor checks
-- a host's TLS cert and warns 7/3/1 days before expiry and once expired.

-- Tightest expiry threshold already notified, so alerting re-notifies only when a
-- tighter threshold is crossed (NULL = none yet, reset on recovery).
ALTER TABLE monitors ADD COLUMN ssl_warned_days INT;

-- Leaf cert NotAfter per result, so the history can show "expires in N days" over
-- time (null for http checks).
ALTER TABLE check_results ADD COLUMN cert_expires_at TIMESTAMPTZ;

-- Latest TLS certificate detail per ssl monitor. One row per monitor, overwritten on
-- each ssl check, so the rich detail stays off the high-volume check_results table
-- (same pattern as monitor_last_failure). Public cert metadata, not secret: stored
-- plaintext, org-scoped. The monitor detail page renders it as a certificate card.
CREATE TABLE monitor_cert (
  monitor_id  BIGINT PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE,
  org_id      BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  subject     TEXT   NOT NULL DEFAULT '',
  issuer      TEXT   NOT NULL DEFAULT '',
  not_before  TIMESTAMPTZ NOT NULL,
  not_after   TIMESTAMPTZ NOT NULL,
  dns_names   TEXT[] NOT NULL DEFAULT '{}',
  serial      TEXT   NOT NULL DEFAULT '',
  checked_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_monitor_cert_org ON monitor_cert (org_id);

ALTER TABLE monitor_cert ENABLE ROW LEVEL SECURITY;
ALTER TABLE monitor_cert FORCE ROW LEVEL SECURITY;
CREATE POLICY monitor_cert_org_isolation ON monitor_cert
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);

-- The restricted app role only has the privileges the baseline grants per table,
-- so a new table needs its own grant or every query as pulse_app is denied.
GRANT SELECT, INSERT, UPDATE, DELETE ON monitor_cert TO pulse_app;

-- +goose Down
DROP TABLE IF EXISTS monitor_cert;
ALTER TABLE check_results DROP COLUMN IF EXISTS cert_expires_at;
ALTER TABLE monitors DROP COLUMN IF EXISTS ssl_warned_days;
