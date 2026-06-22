package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
)

// Org-level outbound webhooks (PRD-005 7, RFC-007 7). All rows are org-scoped (RLS
// on org_id) and go through WithOrg. The signing_secret is encrypted at rest with
// the pool's cipher (same AES-256-GCM the channel secrets use) and decrypted in
// memory on read; a nil cipher (dev/test) stores it as-is. The secret is shown to
// the user exactly once at create/rotate by the api layer; the store always returns
// the decrypted value to the deliverer, never to the API DTO.

const orgWebhookColumns = `id, org_id, url, signing_secret, enabled, events,
	created_by, created_at, updated_at, last_delivery_at, last_status, last_error`

// scanOrgWebhook reads one row and decrypts the signing secret in memory.
func (p *Pool) scanOrgWebhook(row pgx.Row) (*domain.OrgWebhook, error) {
	var (
		w          domain.OrgWebhook
		events     []string
		lastStatus *string
	)
	err := row.Scan(&w.ID, &w.OrgID, &w.URL, &w.SigningSecret, &w.Enabled, &events,
		&w.CreatedBy, &w.CreatedAt, &w.UpdatedAt, &w.LastDeliveryAt, &lastStatus, &w.LastError)
	if err != nil {
		return nil, err
	}
	if lastStatus != nil {
		w.LastStatus = *lastStatus
	}
	for _, e := range events {
		w.Events = append(w.Events, domain.OrgWebhookEvent(e))
	}
	if p.cipher != nil && w.SigningSecret != "" {
		dec, err := p.cipher.Decrypt(w.SigningSecret)
		if err != nil {
			return nil, err
		}
		w.SigningSecret = dec
	}
	return &w, nil
}

// encryptSecret returns the secret encrypted for storage; a nil cipher passes it
// through (dev/test without a key, matching the channel secret behavior).
func (p *Pool) encryptSecret(plaintext string) (string, error) {
	if p.cipher == nil || plaintext == "" {
		return plaintext, nil
	}
	return p.cipher.Encrypt(plaintext)
}

// CreateWebhook inserts an org webhook. The caller passes the raw signing secret
// (it is encrypted here before storage). events may be empty (= all types). Returns
// the new id; w.SigningSecret stays the raw value the caller hands back once.
func (p *Pool) CreateWebhook(ctx context.Context, w *domain.OrgWebhook) (int64, error) {
	enc, err := p.encryptSecret(w.SigningSecret)
	if err != nil {
		return 0, err
	}
	events := make([]string, 0, len(w.Events))
	for _, e := range w.Events {
		events = append(events, string(e))
	}
	var id int64
	err = p.WithOrg(ctx, w.OrgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO org_webhooks (org_id, url, signing_secret, enabled, events, created_by)
			VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING id`,
			w.OrgID, w.URL, enc, w.Enabled, events, w.CreatedBy,
		).Scan(&id)
	})
	if err != nil {
		return 0, err
	}
	w.ID = id
	return id, nil
}

// ListWebhooks returns every webhook in an org, newest first, with secrets decrypted
// in memory (the api DTO redacts them; the store hands back the real value).
func (p *Pool) ListWebhooks(ctx context.Context, orgID int64) ([]*domain.OrgWebhook, error) {
	var out []*domain.OrgWebhook
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+orgWebhookColumns+` FROM org_webhooks WHERE org_id = $1 ORDER BY id DESC`, orgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			w, err := p.scanOrgWebhook(rows)
			if err != nil {
				return err
			}
			out = append(out, w)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetWebhook loads one webhook by org + id. Returns pgx.ErrNoRows when the id is not
// in the org (RLS also hides another org's row).
func (p *Pool) GetWebhook(ctx context.Context, orgID, id int64) (*domain.OrgWebhook, error) {
	var w *domain.OrgWebhook
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+orgWebhookColumns+` FROM org_webhooks WHERE id = $1 AND org_id = $2`, id, orgID)
		v, err := p.scanOrgWebhook(row)
		if err != nil {
			return err
		}
		w = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}

// UpdateWebhook overwrites the mutable fields (url, enabled, events) of a webhook.
// The signing secret is not touched here; RotateWebhookSecret owns that. Returns
// pgx.ErrNoRows when the id is not in the org.
func (p *Pool) UpdateWebhook(ctx context.Context, w *domain.OrgWebhook) error {
	events := make([]string, 0, len(w.Events))
	for _, e := range w.Events {
		events = append(events, string(e))
	}
	return p.WithOrg(ctx, w.OrgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE org_webhooks
			SET url = $1, enabled = $2, events = $3, updated_at = now()
			WHERE id = $4 AND org_id = $5`,
			w.URL, w.Enabled, events, w.ID, w.OrgID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
}

// RotateWebhookSecret writes a new encrypted signing secret for a webhook. The
// caller passes the raw new secret (returned once to the user); the old secret is
// gone, so deliveries verify only against the new one. Returns pgx.ErrNoRows when
// the id is not in the org.
func (p *Pool) RotateWebhookSecret(ctx context.Context, orgID, id int64, rawSecret string) error {
	enc, err := p.encryptSecret(rawSecret)
	if err != nil {
		return err
	}
	return p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE org_webhooks SET signing_secret = $1, updated_at = now()
			WHERE id = $2 AND org_id = $3`,
			enc, id, orgID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
}

// DeleteWebhook removes a webhook. Returns rows affected so a no-op (wrong org/id)
// is distinguishable; delete is idempotent at the api layer.
func (p *Pool) DeleteWebhook(ctx context.Context, orgID, id int64) (int64, error) {
	var affected int64
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM org_webhooks WHERE id = $1 AND org_id = $2`, id, orgID)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	return affected, err
}

// ListEnabledWebhooks loads an org's enabled webhooks with decrypted signing secrets,
// for the deliverer. It returns only enabled rows so a disabled webhook is never
// delivered to. The deliverer further filters by subscribed event type in memory
// (domain.OrgWebhook.Subscribes), keeping the SQL simple.
func (p *Pool) ListEnabledWebhooks(ctx context.Context, orgID int64) ([]*domain.OrgWebhook, error) {
	var out []*domain.OrgWebhook
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+orgWebhookColumns+` FROM org_webhooks WHERE org_id = $1 AND enabled ORDER BY id`, orgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			w, err := p.scanOrgWebhook(rows)
			if err != nil {
				return err
			}
			out = append(out, w)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecordWebhookDelivery stamps the most recent delivery outcome on a webhook so a
// broken receiver is visible (PRD-005 7.2 failure visibility). status is delivered
// or failed; lastError is the short reason on failure (empty clears it). Org-scoped.
func (p *Pool) RecordWebhookDelivery(ctx context.Context, orgID, id int64, status, lastError string) error {
	var errPtr *string
	if lastError != "" {
		errPtr = &lastError
	}
	return p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE org_webhooks
			SET last_delivery_at = now(), last_status = $1, last_error = $2
			WHERE id = $3 AND org_id = $4`,
			status, errPtr, id, orgID)
		return err
	})
}
