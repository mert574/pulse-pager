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

// ApplyPaymentEvent records a payment webhook idempotently in one transaction, the
// same shape as ApplySubscriptionEvent: dedup-insert into billing_events first, then
// upsert the payment by (provider, provider_payment_id) so a payment.succeeded then a
// payment.refunded for the same payment update the one row (refunded_amount, status).
// Returns applied=false when the event was already processed. ErrOrgNotFound if the org
// is gone.
func (p *Pool) ApplyPaymentEvent(ctx context.Context, providerEventID, eventType string, pay *domain.Payment) (applied bool, err error) {
	err = p.WithOrg(ctx, pay.OrgID, func(tx pgx.Tx) error {
		tag, derr := tx.Exec(ctx, `
			INSERT INTO billing_events (provider, provider_event_id, type, received_at)
			VALUES ($1,$2,$3, now())
			ON CONFLICT (provider, provider_event_id) DO NOTHING`,
			pay.Provider, providerEventID, eventType)
		if derr != nil {
			return derr
		}
		if tag.RowsAffected() == 0 {
			applied = false
			return nil
		}

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

		if _, err := tx.Exec(ctx, `
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
			pay.Status, pay.Period, pay.HostedInvoiceURL, pay.RefundedAmount); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx,
			`UPDATE billing_events SET processed_at = now()
			 WHERE provider = $1 AND provider_event_id = $2`,
			pay.Provider, providerEventID); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
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
