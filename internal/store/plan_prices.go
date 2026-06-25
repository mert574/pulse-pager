package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
)

// plan_prices is the catalog mapping (plan, cycle) -> provider price id (RFC-018 6).
// It is platform config (no org, no RLS), so it runs on the bare pool. The billing
// checkout reads it to resolve which price to charge.

const planPriceColumns = `id, provider, plan, cycle, provider_price_id, has_trial,
	trial_days, custom_data, created_at, updated_at`

func scanPlanPrice(row pgx.Row) (*domain.PlanPrice, error) {
	var p domain.PlanPrice
	err := row.Scan(&p.ID, &p.Provider, &p.Plan, &p.Cycle, &p.ProviderPriceID,
		&p.HasTrial, &p.TrialDays, &p.CustomData, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListPlanPrices returns every catalog price for a provider, for building the
// checkout price map.
func (p *Pool) ListPlanPrices(ctx context.Context, provider string) ([]*domain.PlanPrice, error) {
	rows, err := p.Query(ctx,
		`SELECT `+planPriceColumns+` FROM plan_prices WHERE provider = $1 ORDER BY plan, cycle`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.PlanPrice
	for rows.Next() {
		pp, err := scanPlanPrice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pp)
	}
	return out, rows.Err()
}

// UpsertPlanPrice inserts or updates one catalog price (used to seed the catalog).
// custom_data defaults to an empty object when nil.
func (p *Pool) UpsertPlanPrice(ctx context.Context, pp *domain.PlanPrice) error {
	cd := pp.CustomData
	if len(cd) == 0 {
		cd = []byte("{}")
	}
	_, err := p.Exec(ctx, `
		INSERT INTO plan_prices (provider, plan, cycle, provider_price_id, has_trial, trial_days, custom_data, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7, now())
		ON CONFLICT (provider, plan, cycle, has_trial) DO UPDATE SET
			provider_price_id = EXCLUDED.provider_price_id,
			trial_days = EXCLUDED.trial_days,
			custom_data = EXCLUDED.custom_data,
			updated_at = now()`,
		pp.Provider, pp.Plan, pp.Cycle, pp.ProviderPriceID, pp.HasTrial, pp.TrialDays, cd)
	return err
}
