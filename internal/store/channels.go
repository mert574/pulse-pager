package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
)

// GetChannelsForMonitor loads the enabled channels attached to a monitor, with
// their secret config subfields decrypted in memory, ready for notify.Dispatch.
// It reads monitors.notification_channel_ids and joins the channels rows, all
// org-scoped through WithOrg so RLS applies. A disabled channel or a dangling id
// (deleted channel) is simply absent from the result, which is the no-op the
// notifier wants (RFC-007 3.3). A monitor with no attached channels returns an
// empty slice and no error.
//
// secretKeysFor decides which config keys to decrypt per type; pass the notify
// registry's SecretKeys. A nil cipher (dev/test without a key) returns config as
// stored, matching the monitor-headers behavior.
func (p *Pool) GetChannelsForMonitor(ctx context.Context, orgID, monitorID int64, secretKeysFor func(domain.ChannelType) []string) ([]*domain.Channel, error) {
	var out []*domain.Channel
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT c.id, c.org_id, c.name, c.type, c.config, c.enabled
			FROM channels c
			JOIN monitors m ON m.id = $1 AND m.org_id = $2
			WHERE c.org_id = $2
				AND c.enabled
				AND c.id = ANY (m.notification_channel_ids)
			ORDER BY c.id`,
			monitorID, orgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			ch, err := p.scanChannel(rows, secretKeysFor)
			if err != nil {
				return err
			}
			out = append(out, ch)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// scanChannel reads one channel row and decrypts its secret config subfields. The
// config is a JSONB object whose secret string values were encrypted on write;
// custom_headers (a nested object) has each value encrypted, mirroring the secret
// stringlist the descriptor declares.
func (p *Pool) scanChannel(row pgx.Row, secretKeysFor func(domain.ChannelType) []string) (*domain.Channel, error) {
	var (
		ch     domain.Channel
		typ    string
		rawCfg []byte
	)
	if err := row.Scan(&ch.ID, &ch.OrgID, &ch.Name, &typ, &rawCfg, &ch.Enabled); err != nil {
		return nil, err
	}
	ch.Type = domain.ChannelType(typ)

	cfg := map[string]any{}
	if len(rawCfg) > 0 {
		if err := json.Unmarshal(rawCfg, &cfg); err != nil {
			return nil, err
		}
	}
	if err := p.decryptChannelConfig(ch.Type, cfg, secretKeysFor); err != nil {
		return nil, err
	}
	ch.Config = cfg
	return &ch, nil
}

// decryptChannelConfig decrypts the secret keys of one channel's config in place.
// A nil cipher leaves values as-is (dev/test). custom_headers is a nested map whose
// every value is secret, so it is decrypted value-by-value.
func (p *Pool) decryptChannelConfig(t domain.ChannelType, cfg map[string]any, secretKeysFor func(domain.ChannelType) []string) error {
	if p.cipher == nil || secretKeysFor == nil {
		return nil
	}
	for _, key := range secretKeysFor(t) {
		v, ok := cfg[key]
		if !ok || v == nil {
			continue
		}
		switch val := v.(type) {
		case string:
			if val == "" {
				continue
			}
			dec, err := p.cipher.Decrypt(val)
			if err != nil {
				return err
			}
			cfg[key] = dec
		case map[string]any:
			// custom_headers: a nested object of header name -> secret value.
			for hk, hv := range val {
				s, ok := hv.(string)
				if !ok || s == "" {
					continue
				}
				dec, err := p.cipher.Decrypt(s)
				if err != nil {
					return err
				}
				val[hk] = dec
			}
		}
	}
	return nil
}

// EncryptChannelConfig encrypts the secret keys of a channel config for storage,
// the inverse of the read path. It is used by tests and, later, the api channel
// CRUD layer to write a channel row. A nil cipher stores values as-is. It returns
// the JSONB-ready bytes.
func (p *Pool) EncryptChannelConfig(t domain.ChannelType, cfg map[string]any, secretKeysFor func(domain.ChannelType) []string) ([]byte, error) {
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	if p.cipher != nil && secretKeysFor != nil {
		for _, key := range secretKeysFor(t) {
			v, ok := out[key]
			if !ok || v == nil {
				continue
			}
			switch val := v.(type) {
			case string:
				if val == "" {
					continue
				}
				enc, err := p.cipher.Encrypt(val)
				if err != nil {
					return nil, err
				}
				out[key] = enc
			case map[string]any:
				hdrs := make(map[string]any, len(val))
				for hk, hv := range val {
					s, ok := hv.(string)
					if !ok || s == "" {
						hdrs[hk] = hv
						continue
					}
					enc, err := p.cipher.Encrypt(s)
					if err != nil {
						return nil, err
					}
					hdrs[hk] = enc
				}
				out[key] = hdrs
			}
		}
	}
	return json.Marshal(out)
}

// channelColumns is the channel projection both the list and the single-row reads
// share, in the order scanChannel expects.
const channelColumns = `id, org_id, name, type, config, enabled`

// ListChannels returns every channel for an org (enabled and disabled), newest
// last by id, with secret config subfields decrypted in memory. Org-scoped through
// WithOrg so RLS applies. secretKeysFor decides which keys to decrypt per type;
// pass the notify registry's SecretKeys. An org with no channels returns an empty
// slice and no error.
func (p *Pool) ListChannels(ctx context.Context, orgID int64, secretKeysFor func(domain.ChannelType) []string) ([]*domain.Channel, error) {
	var out []*domain.Channel
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+channelColumns+` FROM channels WHERE org_id = $1 ORDER BY id`, orgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			ch, err := p.scanChannel(rows, secretKeysFor)
			if err != nil {
				return err
			}
			out = append(out, ch)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetChannel reads one channel by id within the org, with secret config decrypted.
// An unknown id (or one in another org, hidden by RLS) returns pgx.ErrNoRows.
func (p *Pool) GetChannel(ctx context.Context, orgID, id int64, secretKeysFor func(domain.ChannelType) []string) (*domain.Channel, error) {
	var ch *domain.Channel
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+channelColumns+` FROM channels WHERE id = $1 AND org_id = $2`, id, orgID)
		c, err := p.scanChannel(row, secretKeysFor)
		if err != nil {
			return err
		}
		ch = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ch, nil
}

// CreateChannel inserts a channel and sets ch.ID. The config's secret subfields are
// encrypted at rest via the descriptor's secret keys (secretKeysFor); a nil cipher
// stores them as-is (dev/test). Org-scoped through WithOrg so RLS stamps the tenant.
func (p *Pool) CreateChannel(ctx context.Context, ch *domain.Channel, secretKeysFor func(domain.ChannelType) []string) error {
	cfg, err := p.EncryptChannelConfig(ch.Type, ch.Config, secretKeysFor)
	if err != nil {
		return err
	}
	return p.WithOrg(ctx, ch.OrgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO channels (org_id, name, type, config, enabled)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id`,
			ch.OrgID, ch.Name, string(ch.Type), cfg, ch.Enabled).Scan(&ch.ID)
	})
}

// UpdateChannel overwrites a channel's name, type, config, and enabled flag. The
// config's secret subfields are re-encrypted from the in-memory plaintext, so the
// caller must merge unchanged secrets back in before calling (a blank secret field
// would otherwise wipe the stored value). Returns pgx.ErrNoRows if the channel is
// not in the org.
func (p *Pool) UpdateChannel(ctx context.Context, ch *domain.Channel, secretKeysFor func(domain.ChannelType) []string) error {
	cfg, err := p.EncryptChannelConfig(ch.Type, ch.Config, secretKeysFor)
	if err != nil {
		return err
	}
	return p.WithOrg(ctx, ch.OrgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE channels
			SET name = $1, type = $2, config = $3, enabled = $4, updated_at = now()
			WHERE id = $5 AND org_id = $6`,
			ch.Name, string(ch.Type), cfg, ch.Enabled, ch.ID, ch.OrgID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
}

// DeleteChannel removes a channel by id within the org and reports whether a row
// was deleted (false = unknown id, so the handler can stay idempotent). A deleted
// channel id left dangling in a monitor's notification_channel_ids is simply skipped
// on dispatch (GetChannelsForMonitor), so no cleanup is needed here.
func (p *Pool) DeleteChannel(ctx context.Context, orgID, id int64) (bool, error) {
	var deleted bool
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM channels WHERE id = $1 AND org_id = $2`, id, orgID)
		if err != nil {
			return err
		}
		deleted = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

// ClaimNotifyDedup is the durable dedup backstop (RFC-007 4.2): it inserts the
// dedup id and reports whether this caller is the first to handle the event. A
// false return means the row already existed, so the event is a duplicate and must
// be skipped. The unique (org_id, dedup_id) makes the INSERT ... ON CONFLICT a
// no-op for a redelivery, so two racing consumers cannot both claim it. Org-scoped.
func (p *Pool) ClaimNotifyDedup(ctx context.Context, orgID int64, dedupID string) (bool, error) {
	var claimed bool
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO notify_dedup (org_id, dedup_id) VALUES ($1, $2)
			ON CONFLICT (org_id, dedup_id) DO NOTHING`,
			orgID, dedupID)
		if err != nil {
			return err
		}
		claimed = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return claimed, nil
}

// RecordDelivery upserts the per-(incident, channel, event_type) delivery outcome
// (RFC-007 6.1) so the incident timeline shows whether each channel was reached. A
// redelivery upserts the same row rather than duplicating. It takes plain fields
// (not a struct) so the notify.DeliveryRecorder interface this satisfies needs no
// shared type, and notify never imports store (no package cycle). Org-scoped.
func (p *Pool) RecordDelivery(ctx context.Context, orgID, incidentID, channelID int64, eventType, status string, attempts int, lastError string) error {
	var lastErr *string
	if lastError != "" {
		lastErr = &lastError
	}
	return p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO notify_deliveries
				(org_id, incident_id, channel_id, event_type, status, attempts, last_error, delivered_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7, now())
			ON CONFLICT (org_id, incident_id, channel_id, event_type) DO UPDATE SET
				status       = EXCLUDED.status,
				attempts     = EXCLUDED.attempts,
				last_error   = EXCLUDED.last_error,
				delivered_at = now()`,
			orgID, incidentID, channelID, eventType, status, attempts, lastErr)
		return err
	})
}
