package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
)

// Payments are the read-only provider mirror (RFC-018 4), org-scoped via WithOrg. The
// webhook sync path is the only writer; the billing screen and the admin refund UI
// read them.

const paymentColumns = `id, org_id, provider, provider_payment_id, amount, currency,
	status, period, hosted_invoice_url, refunded_amount, created_at`

func scanPayment(row pgx.Row) (*domain.Payment, error) {
	var p domain.Payment
	err := row.Scan(&p.ID, &p.OrgID, &p.Provider, &p.ProviderPaymentID, &p.Amount,
		&p.Currency, &p.Status, &p.Period, &p.HostedInvoiceURL, &p.RefundedAmount, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ApplyPaymentEvent upserts the payment mirror by (provider, provider_payment_id), so a
// payment.succeeded then a refund for the same payment update the one row (refunded_amount,
// status). Dedup and the raw-payload record are owned by the webhook inbox
// (RecordBillingEvent); the upsert here is idempotent, so re-running is harmless. Returns
// ErrOrgNotFound if the org is gone.
func (p *Pool) ApplyPaymentEvent(ctx context.Context, pay *domain.Payment) error {
	return p.WithOrg(ctx, pay.OrgID, func(tx pgx.Tx) error {
		// Guard the org exists / is active before mirroring (the payments FK would also
		// fail, but this gives the caller a clean permanent error to ack).
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT true FROM organizations WHERE id = $1 AND deleted_at IS NULL`, pay.OrgID).Scan(&exists); err != nil {
			if err == pgx.ErrNoRows {
				return ErrOrgNotFound
			}
			return err
		}

		_, err := tx.Exec(ctx, `
			INSERT INTO payments (org_id, provider, provider_payment_id, amount, currency,
				status, period, hosted_invoice_url, refunded_amount)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (provider, provider_payment_id) DO UPDATE SET
				amount = EXCLUDED.amount,
				currency = EXCLUDED.currency,
				status = EXCLUDED.status,
				period = EXCLUDED.period,
				hosted_invoice_url = EXCLUDED.hosted_invoice_url,
				refunded_amount = EXCLUDED.refunded_amount`,
			pay.OrgID, pay.Provider, pay.ProviderPaymentID, pay.Amount, pay.Currency,
			pay.Status, pay.Period, pay.HostedInvoiceURL, pay.RefundedAmount)
		return err
	})
}

// ListPayments returns an org's payments, newest first, for the billing screen.
func (p *Pool) ListPayments(ctx context.Context, orgID int64) ([]*domain.Payment, error) {
	var out []*domain.Payment
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+paymentColumns+` FROM payments WHERE org_id = $1 ORDER BY created_at DESC, id DESC`, orgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			pay, err := scanPayment(rows)
			if err != nil {
				return err
			}
			out = append(out, pay)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
