-- +goose Up
-- Payment mirror (RFC-018 Phase 4): a read-only copy of provider payments for the
-- billing screen and the refund UI. Money is mirrored from the provider, never
-- computed in Pulse (RFC-018 8), so amounts are BIGINT minor units (cents) + currency,
-- never NUMERIC used for math. RLS-scoped like every tenant table.
CREATE TABLE payments (
  id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id              BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  provider            TEXT NOT NULL,
  provider_payment_id TEXT NOT NULL,
  amount              BIGINT NOT NULL,             -- minor units (cents)
  currency            TEXT NOT NULL,
  status              TEXT NOT NULL,
  period              TEXT NOT NULL DEFAULT '',
  hosted_invoice_url  TEXT NOT NULL DEFAULT '',
  refunded_amount     BIGINT NOT NULL DEFAULT 0,   -- minor units refunded so far
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_payments_provider ON payments (provider, provider_payment_id);
CREATE INDEX idx_payments_org ON payments (org_id);

ALTER TABLE payments ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments FORCE ROW LEVEL SECURITY;
CREATE POLICY payments_org_isolation ON payments
  USING (org_id = NULLIF(current_setting('app.current_org', true), '')::bigint);

GRANT SELECT, INSERT, UPDATE, DELETE ON payments TO pulse_app;

-- +goose Down
DROP TABLE IF EXISTS payments;
