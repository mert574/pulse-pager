package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
	"pulse/internal/region"
)

// monitorColumns is the full column list every monitor read scans, in one place so
// the row scanner and the queries cannot drift. headers and notification_channel_ids
// land last so the read helpers slot them after the shared core columns.
const monitorColumns = `id, org_id, type, name, url, method, body, expected_status_codes,
	timeout_seconds, interval_seconds, enabled, max_latency_ms, body_contains,
	failure_threshold, regions, down_policy, headers, notification_channel_ids,
	created_at, updated_at`

// encodeHeaders marshals the headers to the JSONB column, encrypting any secret
// value at rest. A nil cipher (dev/test without a key) stores the value as-is. The
// stored shape is the same {key,value,secret} array the domain uses, so a decode
// reads back into domain.Header directly.
func (p *Pool) encodeHeaders(headers []domain.Header) ([]byte, error) {
	if len(headers) == 0 {
		return []byte("[]"), nil
	}
	out := make([]domain.Header, len(headers))
	for i, h := range headers {
		out[i] = h
		if h.Secret && h.Value != "" && p.cipher != nil {
			enc, err := p.cipher.Encrypt(h.Value)
			if err != nil {
				return nil, err
			}
			out[i].Value = enc
		}
	}
	return json.Marshal(out)
}

// decodeHeaders reverses encodeHeaders: it decrypts secret values back to plaintext
// in memory (the api redacts them again on serialize). A nil cipher leaves the value
// as-is. The rest of the app sees plaintext header values (mirrors domain.Channel).
func (p *Pool) decodeHeaders(raw []byte) ([]domain.Header, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var headers []domain.Header
	if err := json.Unmarshal(raw, &headers); err != nil {
		return nil, err
	}
	for i := range headers {
		if headers[i].Secret && headers[i].Value != "" && p.cipher != nil {
			dec, err := p.cipher.Decrypt(headers[i].Value)
			if err != nil {
				return nil, err
			}
			headers[i].Value = dec
		}
	}
	return headers, nil
}

// scanMonitor reads one monitor row in monitorColumns order, decoding the JSONB
// headers and the channel-id array, so every read path produces an identical
// domain.Monitor.
func (p *Pool) scanMonitor(row pgx.Row) (*domain.Monitor, error) {
	var (
		m          domain.Monitor
		typ        string
		method     string
		downPolicy string
		rawHeaders []byte
	)
	if err := row.Scan(
		&m.ID, &m.OrgID, &typ, &m.Name, &m.URL, &method, &m.Body, &m.ExpectedStatusCodes,
		&m.TimeoutSeconds, &m.IntervalSeconds, &m.Enabled, &m.MaxLatencyMs, &m.BodyContains,
		&m.FailureThreshold, &m.Regions, &downPolicy, &rawHeaders, &m.ChannelIDs,
		&m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, err
	}
	m.Type = domain.MonitorType(typ)
	m.Method = domain.Method(method)
	m.DownPolicy = domain.DownPolicy(downPolicy)
	headers, err := p.decodeHeaders(rawHeaders)
	if err != nil {
		return nil, err
	}
	m.Headers = headers
	return &m, nil
}

// CreateMonitor inserts a monitor and returns its new id. Org-scoped data; the
// caller supplies m.OrgID. The api create path validates and applies the entitlement
// gate before calling this; the scheduler pipeline seeds with it directly.
func (p *Pool) CreateMonitor(ctx context.Context, m *domain.Monitor) (int64, error) {
	if m.Type == "" {
		m.Type = domain.MonitorHTTP
	}
	if m.DownPolicy == "" {
		m.DownPolicy = domain.DownPolicyQuorum
	}
	if len(m.Regions) == 0 {
		m.Regions = []string{region.Default}
	}
	headers, err := p.encodeHeaders(m.Headers)
	if err != nil {
		return 0, err
	}
	channelIDs := m.ChannelIDs
	if channelIDs == nil {
		channelIDs = []int64{}
	}
	var id int64
	// Org-scoped insert through WithOrg so RLS lets the row through under the restricted
	// pulse_app role (the policy checks org_id = app.current_org). A superuser pool
	// (the scheduler/pipeline seeder) bypasses RLS, so this is harmless there too.
	err = p.WithOrg(ctx, m.OrgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO monitors (org_id, type, name, url, method, body, expected_status_codes,
				timeout_seconds, interval_seconds, enabled, max_latency_ms, body_contains,
				failure_threshold, regions, down_policy, headers, notification_channel_ids)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
			RETURNING id`,
			m.OrgID, string(m.Type), m.Name, m.URL, string(m.Method), m.Body, m.ExpectedStatusCodes,
			m.TimeoutSeconds, m.IntervalSeconds, m.Enabled, m.MaxLatencyMs, m.BodyContains,
			m.FailureThreshold, m.Regions, string(m.DownPolicy), headers, channelIDs,
		).Scan(&id)
	})
	if err != nil {
		return 0, err
	}
	m.ID = id
	return id, nil
}

// EnabledMonitor is an enabled monitor plus when it was last checked (nil if never),
// the input the scheduler needs to decide due-ness from persisted state.
type EnabledMonitor struct {
	Monitor       *domain.Monitor
	LastCheckedAt *time.Time
}

// ListEnabledMonitorsWithLastCheck returns every enabled monitor with its most recent
// check time, read with a lateral join on the (monitor_id, checked_at DESC) index. The
// scheduler uses LastCheckedAt to seed its next-run from persisted state, so a restart
// (the service is killed often on CD) resumes the real schedule instead of dispatching
// every monitor at once. This is a deliberate control-plane cross-tenant read (the
// scheduler dispatches for every org), so it does not go through WithOrg and relies on
// the scheduler running on a role that bypasses RLS.
func (p *Pool) ListEnabledMonitorsWithLastCheck(ctx context.Context) ([]EnabledMonitor, error) {
	rows, err := p.Query(ctx, `
		SELECT `+prefixColumns("m", monitorColumns)+`, last.checked_at
		FROM monitors m
		LEFT JOIN LATERAL (
			SELECT checked_at FROM check_results r
			WHERE r.monitor_id = m.id
			ORDER BY r.checked_at DESC
			LIMIT 1
		) last ON true
		WHERE m.enabled`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EnabledMonitor
	for rows.Next() {
		var (
			m          domain.Monitor
			typ        string
			method     string
			downPolicy string
			rawHeaders []byte
			lastAt     *time.Time
		)
		if err := rows.Scan(
			&m.ID, &m.OrgID, &typ, &m.Name, &m.URL, &method, &m.Body, &m.ExpectedStatusCodes,
			&m.TimeoutSeconds, &m.IntervalSeconds, &m.Enabled, &m.MaxLatencyMs, &m.BodyContains,
			&m.FailureThreshold, &m.Regions, &downPolicy, &rawHeaders, &m.ChannelIDs,
			&m.CreatedAt, &m.UpdatedAt, &lastAt,
		); err != nil {
			return nil, err
		}
		m.Type = domain.MonitorType(typ)
		m.Method = domain.Method(method)
		m.DownPolicy = domain.DownPolicy(downPolicy)
		headers, err := p.decodeHeaders(rawHeaders)
		if err != nil {
			return nil, err
		}
		m.Headers = headers
		out = append(out, EnabledMonitor{Monitor: &m, LastCheckedAt: lastAt})
	}
	return out, rows.Err()
}

// GetMonitor returns one monitor in the org, or pgx.ErrNoRows if it does not exist
// (or belongs to another org: RLS hides it). Org-scoped, via WithOrg.
func (p *Pool) GetMonitor(ctx context.Context, orgID, id int64) (*domain.Monitor, error) {
	var m *domain.Monitor
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		m, err = p.scanMonitor(tx.QueryRow(ctx,
			`SELECT `+monitorColumns+` FROM monitors WHERE id = $1 AND org_id = $2`, id, orgID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

// MonitorListRow is one row of the list view (PRD-002 history/status reads): the
// monitor plus its derived status bits (last check time/latency, whether an incident
// is open). The status itself is derived by the caller from these plus enabled.
type MonitorListRow struct {
	Monitor       *domain.Monitor
	LastCheckedAt *time.Time
	LastLatencyMs *int
	LastHealthy   *bool
	IncidentOpen  bool
	// CertExpiresAt is the latest TLS cert expiry for an ssl monitor (nil for http
	// or a never-checked ssl monitor), so the list can show the expiry column.
	CertExpiresAt *time.Time
}

// ListMonitors returns the org's monitors with the derived list-view bits: the most
// recent check's time/latency/health and whether an incident is open. Org-scoped.
// The latest check is read with a lateral join on the (monitor_id, checked_at DESC)
// index so the list stays one query.
func (p *Pool) ListMonitors(ctx context.Context, orgID int64) ([]MonitorListRow, error) {
	var out []MonitorListRow
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+prefixColumns("m", monitorColumns)+`,
				last.checked_at, last.latency_ms, last.healthy,
				EXISTS (SELECT 1 FROM incidents i WHERE i.monitor_id = m.id AND i.ended_at IS NULL) AS incident_open,
				mc.not_after
			FROM monitors m
			LEFT JOIN LATERAL (
				SELECT checked_at, latency_ms, healthy
				FROM check_results r
				WHERE r.monitor_id = m.id
				ORDER BY r.checked_at DESC
				LIMIT 1
			) last ON true
			LEFT JOIN monitor_cert mc ON mc.monitor_id = m.id
			WHERE m.org_id = $1
			ORDER BY m.id`, orgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			row, err := p.scanMonitorListRow(rows)
			if err != nil {
				return err
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// scanMonitorListRow scans the monitor columns followed by the four derived bits.
func (p *Pool) scanMonitorListRow(rows pgx.Rows) (MonitorListRow, error) {
	var (
		m          domain.Monitor
		typ        string
		method     string
		downPolicy string
		rawHeaders []byte
		row        MonitorListRow
	)
	if err := rows.Scan(
		&m.ID, &m.OrgID, &typ, &m.Name, &m.URL, &method, &m.Body, &m.ExpectedStatusCodes,
		&m.TimeoutSeconds, &m.IntervalSeconds, &m.Enabled, &m.MaxLatencyMs, &m.BodyContains,
		&m.FailureThreshold, &m.Regions, &downPolicy, &rawHeaders, &m.ChannelIDs,
		&m.CreatedAt, &m.UpdatedAt,
		&row.LastCheckedAt, &row.LastLatencyMs, &row.LastHealthy, &row.IncidentOpen,
		&row.CertExpiresAt,
	); err != nil {
		return MonitorListRow{}, err
	}
	m.Type = domain.MonitorType(typ)
	m.Method = domain.Method(method)
	m.DownPolicy = domain.DownPolicy(downPolicy)
	headers, err := p.decodeHeaders(rawHeaders)
	if err != nil {
		return MonitorListRow{}, err
	}
	m.Headers = headers
	row.Monitor = &m
	return row, nil
}

// UpdateMonitor overwrites a monitor's editable fields and bumps updated_at. It
// returns pgx.ErrNoRows if no row in the org matches (unknown or another org's).
// Org-scoped, via WithOrg.
func (p *Pool) UpdateMonitor(ctx context.Context, m *domain.Monitor) (*domain.Monitor, error) {
	if m.Type == "" {
		m.Type = domain.MonitorHTTP
	}
	if m.DownPolicy == "" {
		m.DownPolicy = domain.DownPolicyQuorum
	}
	if len(m.Regions) == 0 {
		m.Regions = []string{region.Default}
	}
	headers, err := p.encodeHeaders(m.Headers)
	if err != nil {
		return nil, err
	}
	channelIDs := m.ChannelIDs
	if channelIDs == nil {
		channelIDs = []int64{}
	}
	var updated *domain.Monitor
	err = p.WithOrg(ctx, m.OrgID, func(tx pgx.Tx) error {
		var qerr error
		updated, qerr = p.scanMonitor(tx.QueryRow(ctx, `
			UPDATE monitors SET
				type = $3, name = $4, url = $5, method = $6, body = $7, expected_status_codes = $8,
				timeout_seconds = $9, interval_seconds = $10, enabled = $11, max_latency_ms = $12,
				body_contains = $13, failure_threshold = $14, regions = $15, down_policy = $16,
				headers = $17, notification_channel_ids = $18, updated_at = now()
			WHERE id = $1 AND org_id = $2
			RETURNING `+monitorColumns,
			m.ID, m.OrgID, string(m.Type), m.Name, m.URL, string(m.Method), m.Body, m.ExpectedStatusCodes,
			m.TimeoutSeconds, m.IntervalSeconds, m.Enabled, m.MaxLatencyMs, m.BodyContains,
			m.FailureThreshold, m.Regions, string(m.DownPolicy), headers, channelIDs,
		))
		return qerr
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// DeleteMonitor removes a monitor in the org. Its check_results, incidents, and
// last-failure snapshot cascade via the schema FKs. It returns the affected row
// count (0 = unknown or another org's). Org-scoped, via WithOrg.
func (p *Pool) DeleteMonitor(ctx context.Context, orgID, id int64) (int64, error) {
	var affected int64
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM monitors WHERE id = $1 AND org_id = $2`, id, orgID)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// ListResults returns one monitor's check results newest-first, sliced to a time
// range (since), optionally to one region, and paged by a cursor (the checked_at to
// read before). limit caps the page; the api derives the cursor and range. An empty
// region returns every region's checks interleaved. Org-scoped, via WithOrg.
func (p *Pool) ListResults(ctx context.Context, orgID, monitorID int64, since time.Time, before *time.Time, region string, limit int) ([]*domain.CheckResult, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []*domain.CheckResult
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, org_id, monitor_id, region, scheduled_at, checked_at, healthy,
				failure_reason, status_code, latency_ms, error_text, cert_expires_at
			FROM check_results
			WHERE monitor_id = $1 AND org_id = $2 AND checked_at >= $3
				AND ($4::timestamptz IS NULL OR checked_at < $4)
				AND ($5 = '' OR region = $5)
			ORDER BY checked_at DESC
			LIMIT $6`,
			monitorID, orgID, since, before, region, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				r      domain.CheckResult
				reason *string
			)
			if err := rows.Scan(&r.ID, &r.OrgID, &r.MonitorID, &r.Region, &r.ScheduledAt, &r.CheckedAt,
				&r.Healthy, &reason, &r.StatusCode, &r.LatencyMs, &r.ErrorText, &r.CertExpiresAt); err != nil {
				return err
			}
			if reason != nil {
				fr := domain.FailureReason(*reason)
				r.FailureReason = &fr
			}
			out = append(out, &r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListIncidents returns one monitor's incidents newest-first, paged by a cursor (the
// started_at to read before). limit caps the page. Org-scoped, via WithOrg.
func (p *Pool) ListIncidents(ctx context.Context, orgID, monitorID int64, before *time.Time, limit int) ([]*domain.Incident, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []*domain.Incident
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, org_id, monitor_id, started_at, ended_at, cause_reason,
				close_reason, closed_by, first_result_id
			FROM incidents
			WHERE monitor_id = $1 AND org_id = $2
				AND ($3::timestamptz IS NULL OR started_at < $3)
			ORDER BY started_at DESC
			LIMIT $4`,
			monitorID, orgID, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				inc         domain.Incident
				cause       string
				closeReason *string
			)
			if err := rows.Scan(&inc.ID, &inc.OrgID, &inc.MonitorID, &inc.StartedAt, &inc.EndedAt,
				&cause, &closeReason, &inc.ClosedBy, &inc.FirstResultID); err != nil {
				return err
			}
			inc.CauseReason = domain.FailureReason(cause)
			if closeReason != nil {
				cr := domain.CloseReason(*closeReason)
				inc.CloseReason = &cr
			}
			out = append(out, &inc)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CreateIncident inserts an incident (used by tests and, later, the alerting engine
// when it persists incident state). Org-scoped, via WithOrg.
func (p *Pool) CreateIncident(ctx context.Context, inc *domain.Incident) (int64, error) {
	var closeReason *string
	if inc.CloseReason != nil {
		s := string(*inc.CloseReason)
		closeReason = &s
	}
	var id int64
	err := p.WithOrg(ctx, inc.OrgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO incidents (org_id, monitor_id, started_at, ended_at, cause_reason,
				close_reason, closed_by, first_result_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			RETURNING id`,
			inc.OrgID, inc.MonitorID, inc.StartedAt, inc.EndedAt, string(inc.CauseReason),
			closeReason, inc.ClosedBy, inc.FirstResultID,
		).Scan(&id)
	})
	if err != nil {
		return 0, err
	}
	inc.ID = id
	return id, nil
}

// CountEnabledMonitors returns how many enabled monitors the org has, the figure the
// monitor cap is checked against (PRD-006 4.3: disabled monitors do not count).
// Org-scoped, via WithOrg.
func (p *Pool) CountEnabledMonitors(ctx context.Context, orgID int64) (int, error) {
	var n int
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM monitors WHERE org_id = $1 AND enabled`, orgID).Scan(&n)
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// prefixColumns prefixes each comma-separated column in cols with "alias.". It keeps
// the list-view query readable without repeating the long monitorColumns string.
func prefixColumns(alias, cols string) string {
	var b []byte
	col := make([]byte, 0, 32)
	flush := func() {
		trimmed := trimSpace(col)
		if len(trimmed) > 0 {
			if len(b) > 0 {
				b = append(b, ',', ' ')
			}
			b = append(b, alias...)
			b = append(b, '.')
			b = append(b, trimmed...)
		}
		col = col[:0]
	}
	for i := 0; i < len(cols); i++ {
		if cols[i] == ',' {
			flush()
			continue
		}
		col = append(col, cols[i])
	}
	flush()
	return string(b)
}

// trimSpace trims ASCII whitespace (incl. newlines/tabs) from both ends of b. The
// column list spans lines, so the per-column tokens carry indentation to strip.
func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && isSpace(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// InsertCheckResult writes one result. It is idempotent: a redelivered job that
// produces the same (org_id, monitor_id, region, checked_at) is a no-op insert.
// Org-scoped through WithOrg so it lands under the restricted pulse_app role (the
// api check-now path); a superuser pool (the worker) bypasses RLS so this is harmless
// there too.
func (p *Pool) InsertCheckResult(ctx context.Context, r *domain.CheckResult) error {
	var failureReason *string
	if r.FailureReason != nil {
		s := string(*r.FailureReason)
		failureReason = &s
	}
	return p.WithOrg(ctx, r.OrgID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO check_results (org_id, monitor_id, region, scheduled_at, checked_at, healthy,
				failure_reason, status_code, latency_ms, error_text, cert_expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT (org_id, monitor_id, region, checked_at) DO NOTHING`,
			r.OrgID, r.MonitorID, r.Region, r.ScheduledAt, r.CheckedAt, r.Healthy,
			failureReason, r.StatusCode, r.LatencyMs, r.ErrorText, r.CertExpiresAt,
		)
		return err
	})
}

// UpsertMonitorLastFailure overwrites the per-monitor last-failure snapshot
// (PRD-002 3.8). One row per monitor, replaced on each new failure. Org-scoped via
// WithOrg (see InsertCheckResult).
func (p *Pool) UpsertMonitorLastFailure(ctx context.Context, orgID, monitorID int64, snap *domain.ResponseSnapshot, checkedAt time.Time) error {
	headers, err := json.Marshal(snap.Headers)
	if err != nil {
		return err
	}
	return p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO monitor_last_failure (monitor_id, org_id, checked_at, status_code, headers, body, truncated)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (monitor_id) DO UPDATE SET
				org_id      = EXCLUDED.org_id,
				checked_at  = EXCLUDED.checked_at,
				status_code = EXCLUDED.status_code,
				headers     = EXCLUDED.headers,
				body        = EXCLUDED.body,
				truncated   = EXCLUDED.truncated,
				captured_at = now()`,
			monitorID, orgID, checkedAt, snap.StatusCode, string(headers), snap.Body, snap.Truncated,
		)
		return err
	})
}

// UpsertMonitorCert overwrites the per-ssl-monitor certificate detail (BACKLOG:
// SSL-expiry). One row per monitor, replaced on each ssl check. Org-scoped via
// WithOrg (see InsertCheckResult).
func (p *Pool) UpsertMonitorCert(ctx context.Context, orgID, monitorID int64, c *domain.CertInfo, checkedAt time.Time) error {
	return p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO monitor_cert (monitor_id, org_id, subject, issuer, not_before, not_after, dns_names, serial, checked_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (monitor_id) DO UPDATE SET
				org_id     = EXCLUDED.org_id,
				subject    = EXCLUDED.subject,
				issuer     = EXCLUDED.issuer,
				not_before = EXCLUDED.not_before,
				not_after  = EXCLUDED.not_after,
				dns_names  = EXCLUDED.dns_names,
				serial     = EXCLUDED.serial,
				checked_at = EXCLUDED.checked_at`,
			monitorID, orgID, c.Subject, c.Issuer, c.NotBefore, c.NotAfter, c.DNSNames, c.Serial, checkedAt,
		)
		return err
	})
}

// GetMonitorCert returns the latest certificate detail for an ssl monitor, or nil
// when none has been recorded yet (a never-checked or non-ssl monitor). Org-scoped
// via WithOrg.
func (p *Pool) GetMonitorCert(ctx context.Context, orgID, monitorID int64) (*domain.CertInfo, error) {
	var ci *domain.CertInfo
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		var c domain.CertInfo
		err := tx.QueryRow(ctx, `
			SELECT subject, issuer, not_before, not_after, dns_names, serial
			FROM monitor_cert WHERE monitor_id = $1 AND org_id = $2`, monitorID, orgID,
		).Scan(&c.Subject, &c.Issuer, &c.NotBefore, &c.NotAfter, &c.DNSNames, &c.Serial)
		if err != nil {
			if err == pgx.ErrNoRows {
				return nil
			}
			return err
		}
		ci = &c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ci, nil
}
