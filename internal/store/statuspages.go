package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
)

// This file is the data-access for status pages (PRD-004). It has two halves:
//
//   - the org-scoped CRUD (create/get/list/update/delete + the displayed-monitor
//     join), all through WithOrg so RLS keys off the org;
//   - the PUBLIC read (GetPublicStatusPage) for the unauthenticated public page. It
//     runs with no org in context, on the app.public_page_slug capability (mirrors
//     the invitation token capability): the public-lookup RLS policies let only a
//     PUBLISHED page named by that slug, and the monitors/incidents/results of its
//     displayed monitors, be read. The public read NEVER selects the secret/internal
//     monitor columns (url, headers, body, assertions, failure detail) into its DTO;
//     the privacy boundary is this projection, not a UI filter (PRD-004 3.6).

// statusPageColumns is the column list every status-page read scans, in one place so
// the scanner and the queries cannot drift.
const statusPageColumns = `id, org_id, name, slug, logo_url, accent_color, theme,
	published, custom_domain, created_at, updated_at`

// scanStatusPage reads one status_pages row in statusPageColumns order.
func scanStatusPage(row pgx.Row) (*domain.StatusPage, error) {
	var (
		sp    domain.StatusPage
		theme string
	)
	if err := row.Scan(&sp.ID, &sp.OrgID, &sp.Name, &sp.Slug, &sp.LogoURL, &sp.AccentColor,
		&theme, &sp.Published, &sp.CustomDomain, &sp.CreatedAt, &sp.UpdatedAt); err != nil {
		return nil, err
	}
	sp.Theme = domain.StatusPageTheme(theme)
	return &sp, nil
}

// CreateStatusPage inserts a status page and returns it with its new id. A slug clash
// (the global unique index) is turned into ErrSlugTaken so the api answers with the
// per-field envelope. Org-scoped, via WithOrg.
func (p *Pool) CreateStatusPage(ctx context.Context, sp *domain.StatusPage) (*domain.StatusPage, error) {
	if sp.Theme == "" {
		sp.Theme = domain.ThemeLight
	}
	var out *domain.StatusPage
	err := p.WithOrg(ctx, sp.OrgID, func(tx pgx.Tx) error {
		var qerr error
		out, qerr = scanStatusPage(tx.QueryRow(ctx, `
			INSERT INTO status_pages (org_id, name, slug, logo_url, accent_color, theme, published, custom_domain)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			RETURNING `+statusPageColumns,
			sp.OrgID, sp.Name, sp.Slug, sp.LogoURL, sp.AccentColor, string(sp.Theme), sp.Published, sp.CustomDomain,
		))
		return qerr
	})
	if err != nil {
		if IsUniqueViolation(err) {
			return nil, ErrSlugTaken
		}
		return nil, err
	}
	return out, nil
}

// GetStatusPage returns one page in the org, or pgx.ErrNoRows if it does not exist
// (or belongs to another org: RLS hides it). Org-scoped, via WithOrg.
func (p *Pool) GetStatusPage(ctx context.Context, orgID, id int64) (*domain.StatusPage, error) {
	var sp *domain.StatusPage
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		var qerr error
		sp, qerr = scanStatusPage(tx.QueryRow(ctx,
			`SELECT `+statusPageColumns+` FROM status_pages WHERE id = $1 AND org_id = $2`, id, orgID))
		return qerr
	})
	if err != nil {
		return nil, err
	}
	return sp, nil
}

// ListStatusPages returns the org's status pages, newest first. Org-scoped.
func (p *Pool) ListStatusPages(ctx context.Context, orgID int64) ([]*domain.StatusPage, error) {
	var out []*domain.StatusPage
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+statusPageColumns+` FROM status_pages WHERE org_id = $1 ORDER BY id DESC`, orgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			sp, err := scanStatusPage(rows)
			if err != nil {
				return err
			}
			out = append(out, sp)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CountStatusPages returns how many status pages the org has, the figure the page cap
// is checked against (PRD-004 2.3). Org-scoped, via WithOrg.
func (p *Pool) CountStatusPages(ctx context.Context, orgID int64) (int, error) {
	var n int
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM status_pages WHERE org_id = $1`, orgID).Scan(&n)
	})
	return n, err
}

// UpdateStatusPage overwrites a page's editable fields (name, slug, branding,
// published, custom_domain) and bumps updated_at. It returns pgx.ErrNoRows if no row
// in the org matches, and ErrSlugTaken on a slug clash. Org-scoped, via WithOrg.
func (p *Pool) UpdateStatusPage(ctx context.Context, sp *domain.StatusPage) (*domain.StatusPage, error) {
	if sp.Theme == "" {
		sp.Theme = domain.ThemeLight
	}
	var out *domain.StatusPage
	err := p.WithOrg(ctx, sp.OrgID, func(tx pgx.Tx) error {
		var qerr error
		out, qerr = scanStatusPage(tx.QueryRow(ctx, `
			UPDATE status_pages SET
				name = $3, slug = $4, logo_url = $5, accent_color = $6, theme = $7,
				published = $8, custom_domain = $9, updated_at = now()
			WHERE id = $1 AND org_id = $2
			RETURNING `+statusPageColumns,
			sp.ID, sp.OrgID, sp.Name, sp.Slug, sp.LogoURL, sp.AccentColor, string(sp.Theme),
			sp.Published, sp.CustomDomain,
		))
		return qerr
	})
	if err != nil {
		if IsUniqueViolation(err) {
			return nil, ErrSlugTaken
		}
		return nil, err
	}
	return out, nil
}

// DeleteStatusPage removes a page in the org. Its displayed-monitor join rows cascade
// via the schema FK. Returns the affected row count (0 = unknown or another org's).
// Org-scoped, via WithOrg.
func (p *Pool) DeleteStatusPage(ctx context.Context, orgID, id int64) (int64, error) {
	var affected int64
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM status_pages WHERE id = $1 AND org_id = $2`, id, orgID)
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

// ListStatusPageMonitors returns the displayed-monitor entries of a page in order.
// Org-scoped, via WithOrg.
func (p *Pool) ListStatusPageMonitors(ctx context.Context, orgID, pageID int64) ([]*domain.StatusPageMonitor, error) {
	var out []*domain.StatusPageMonitor
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, org_id, status_page_id, monitor_id, display_name, sort_order
			FROM status_page_monitors
			WHERE status_page_id = $1 AND org_id = $2
			ORDER BY sort_order, id`, pageID, orgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m domain.StatusPageMonitor
			if err := rows.Scan(&m.ID, &m.OrgID, &m.PageID, &m.MonitorID, &m.DisplayName, &m.SortOrder); err != nil {
				return err
			}
			out = append(out, &m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetStatusPageMonitors replaces the page's displayed-monitor list with entries in
// one transaction: it checks the page exists in the org, verifies every monitor id
// belongs to the org (so a foreign monitor cannot be slipped onto a page), then wipes
// and reinserts the join rows in the given order. Returns ErrStatusPageNotFound if the
// page is not in the org, and ErrMonitorNotInOrg if any monitor id is not. Org-scoped.
func (p *Pool) SetStatusPageMonitors(ctx context.Context, orgID, pageID int64, entries []*domain.StatusPageMonitor) error {
	return p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		// Page must exist in the org.
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM status_pages WHERE id = $1 AND org_id = $2)`,
			pageID, orgID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrStatusPageNotFound
		}
		// Every monitor must belong to the org. RLS already scopes the read to the org,
		// so a count short of the request means at least one id is foreign or unknown.
		ids := make([]int64, 0, len(entries))
		for _, e := range entries {
			ids = append(ids, e.MonitorID)
		}
		if len(ids) > 0 {
			var found int
			if err := tx.QueryRow(ctx,
				`SELECT count(DISTINCT id) FROM monitors WHERE org_id = $1 AND id = ANY($2)`,
				orgID, ids).Scan(&found); err != nil {
				return err
			}
			if found != distinctCount(ids) {
				return ErrMonitorNotInOrg
			}
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM status_page_monitors WHERE status_page_id = $1 AND org_id = $2`,
			pageID, orgID); err != nil {
			return err
		}
		for i, e := range entries {
			if _, err := tx.Exec(ctx, `
				INSERT INTO status_page_monitors (org_id, status_page_id, monitor_id, display_name, sort_order)
				VALUES ($1,$2,$3,$4,$5)`,
				orgID, pageID, e.MonitorID, e.DisplayName, i); err != nil {
				return err
			}
		}
		return nil
	})
}

// distinctCount returns how many distinct values ids holds, so SetStatusPageMonitors
// can tell a duplicate id apart from a foreign id.
func distinctCount(ids []int64) int {
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	return len(seen)
}

// ErrStatusPageNotFound is returned when a status page does not exist in the org.
var ErrStatusPageNotFound = errors.New("status page not found")

// ErrMonitorNotInOrg is returned when a displayed-monitor entry references a monitor
// that is not in the org (so it cannot be added to the page).
var ErrMonitorNotInOrg = errors.New("monitor not in org")

// --- public read (PRD-004 3.6, 4, 5) ---

// publicMonitorRow is one displayed monitor's server-side data read on the public
// path: the friendly name, the bits needed to derive the public status (enabled,
// has-results, open-incident), and the start/end of the most recent incident for the
// public incident list. It carries NO url/headers/body/assertions: those columns are
// never selected here, which is the privacy boundary (PRD-004 3.6).
type publicMonitorRow struct {
	monitorID   int64
	displayName string
	enabled     bool
	hasResults  bool
	openInc     bool
	lastIncAt   *time.Time
	lastIncEnd  *time.Time
}

// GetPublicStatusPage returns the public-safe projection of a PUBLISHED page by slug,
// for the unauthenticated public endpoint. It runs with NO org scope, on the
// app.public_page_slug capability (mirrors the invitation token capability): the
// public-lookup RLS policies let only the published page named by slug, and the data
// of its displayed monitors, be read. A draft or unknown slug returns pgx.ErrNoRows
// (the public-lookup policy matches only published rows, so a draft's existence is not
// leaked, PRD-004 6). For each visible displayed monitor it derives the public status,
// the 24h/7d/90d uptime, and a recent history strip; it also gathers recent public
// incidents. The raw monitor url/method/headers/body/assertions/failure-detail are
// never present in the result: this function does not select those columns.
func (p *Pool) GetPublicStatusPage(ctx context.Context, slug string) (*domain.PublicStatusPage, error) {
	var out *domain.PublicStatusPage
	err := p.withPublicPage(ctx, slug, func(tx pgx.Tx) error {
		sp, err := scanStatusPage(tx.QueryRow(ctx, `
			SELECT `+statusPageColumns+` FROM status_pages
			WHERE published AND lower(slug) = lower($1)`, slug))
		if err != nil {
			return err
		}

		// Displayed monitors with only the public-safe bits. status_code/latency/url are
		// deliberately NOT in this SELECT. The recent incident (started/ended) feeds the
		// public incident list and lets a recovered monitor read as resolved.
		rows, err := tx.Query(ctx, `
			SELECT spm.monitor_id, spm.display_name, m.enabled,
				EXISTS (SELECT 1 FROM check_results r WHERE r.monitor_id = m.id) AS has_results,
				EXISTS (SELECT 1 FROM incidents i WHERE i.monitor_id = m.id AND i.ended_at IS NULL) AS open_inc,
				last_inc.started_at, last_inc.ended_at
			FROM status_page_monitors spm
			JOIN monitors m ON m.id = spm.monitor_id
			LEFT JOIN LATERAL (
				SELECT started_at, ended_at FROM incidents i
				WHERE i.monitor_id = m.id
				ORDER BY i.started_at DESC LIMIT 1
			) last_inc ON true
			WHERE spm.status_page_id = $1
			ORDER BY spm.sort_order, spm.id`, sp.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		var displayed []publicMonitorRow
		for rows.Next() {
			var r publicMonitorRow
			if err := rows.Scan(&r.monitorID, &r.displayName, &r.enabled, &r.hasResults,
				&r.openInc, &r.lastIncAt, &r.lastIncEnd); err != nil {
				return err
			}
			displayed = append(displayed, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		page := &domain.PublicStatusPage{
			Name:        sp.Name,
			Slug:        sp.Slug,
			LogoURL:     sp.LogoURL,
			AccentColor: sp.AccentColor,
			Theme:       sp.Theme,
		}
		var statuses []domain.PublicStatus
		maxWindow := ""
		for _, r := range displayed {
			st := domain.DerivePublicStatus(domain.DeriveStatus(r.enabled, r.hasResults, r.openInc))
			if st == domain.PublicHidden {
				// A disabled monitor is hidden from the page entirely (PRD-004 3.5).
				continue
			}
			statuses = append(statuses, st)
			summary, hist, err := p.publicMonitorUptime(ctx, tx, r.monitorID)
			if err != nil {
				return err
			}
			if summary.Has90d {
				maxWindow = "90d"
			} else if summary.Has7d && maxWindow != "90d" {
				maxWindow = "7d"
			} else if summary.Has24h && maxWindow == "" {
				maxWindow = "24h"
			}
			page.Monitors = append(page.Monitors, domain.PublicDisplayedMonitor{
				DisplayName: r.displayName,
				Status:      st,
				Uptime:      summary,
				History:     hist,
			})

			// A recent incident on a visible monitor surfaces publicly: open shows as
			// ongoing, closed shows resolved with a duration (PRD-004 5.1).
			if r.lastIncAt != nil {
				inc := domain.PublicIncident{DisplayName: r.displayName, StartedAt: *r.lastIncAt}
				if r.lastIncEnd != nil {
					inc.EndedAt = r.lastIncEnd
					secs := int(r.lastIncEnd.Sub(*r.lastIncAt).Seconds())
					inc.DurationSeconds = &secs
					inc.Resolved = true
				}
				page.Incidents = append(page.Incidents, inc)
			}
		}
		page.Banner = domain.DeriveBanner(statuses)
		page.UptimeMaxWindow = maxWindow
		out = page
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// publicMonitorUptime computes one monitor's 24h/7d/90d uptime (healthy ratio over
// the window) and a recent up/down history strip, from check_results only. It runs on
// the same public-page transaction so the public-lookup RLS lets the rows through. It
// reads no latency/status/failure detail beyond the healthy flag (the history is
// up/down only, PRD-004 3.4). A window with no results reports 100 and Has* false so
// the caller can label "no data" rather than imply a measurement (PRD-004 3.3).
func (p *Pool) publicMonitorUptime(ctx context.Context, tx pgx.Tx, monitorID int64) (domain.UptimeSummary, []domain.PublicHistoryPoint, error) {
	now := time.Now().UTC()
	var summary domain.UptimeSummary

	windows := []struct {
		since time.Time
		pct   *float64
		has   *bool
	}{
		{now.Add(-24 * time.Hour), &summary.Uptime24h, &summary.Has24h},
		{now.Add(-7 * 24 * time.Hour), &summary.Uptime7d, &summary.Has7d},
		{now.Add(-90 * 24 * time.Hour), &summary.Uptime90d, &summary.Has90d},
	}
	for _, w := range windows {
		var total, healthy int
		if err := tx.QueryRow(ctx, `
			SELECT count(*), count(*) FILTER (WHERE healthy)
			FROM check_results
			WHERE monitor_id = $1 AND checked_at >= $2`, monitorID, w.since).Scan(&total, &healthy); err != nil {
			return summary, nil, err
		}
		if total == 0 {
			*w.pct = 100
			*w.has = false
			continue
		}
		*w.pct = float64(healthy) / float64(total) * 100
		*w.has = true
	}

	// Recent history strip: the last N checks as up/down, oldest..newest. up/down only.
	rows, err := tx.Query(ctx, `
		SELECT checked_at, healthy FROM check_results
		WHERE monitor_id = $1
		ORDER BY checked_at DESC
		LIMIT 30`, monitorID)
	if err != nil {
		return summary, nil, err
	}
	defer rows.Close()
	var hist []domain.PublicHistoryPoint
	for rows.Next() {
		var pt domain.PublicHistoryPoint
		if err := rows.Scan(&pt.At, &pt.Up); err != nil {
			return summary, nil, err
		}
		hist = append(hist, pt)
	}
	if err := rows.Err(); err != nil {
		return summary, nil, err
	}
	// Reverse to oldest..newest so the bar renders left-to-right in time order.
	for i, j := 0, len(hist)-1; i < j; i, j = i+1, j-1 {
		hist[i], hist[j] = hist[j], hist[i]
	}
	return summary, hist, nil
}

// withPublicPage runs fn in a transaction with app.public_page_slug set, so the
// public-lookup RLS policies on status_pages and the displayed-monitor data let the
// one published page (and only its monitors/incidents/results) be read without an org
// scope. Mirrors WithOrg / withInviteToken but for the public-page capability.
func (p *Pool) withPublicPage(ctx context.Context, slug string, fn func(tx pgx.Tx) error) (err error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err = tx.Exec(ctx, "SELECT set_config('app.public_page_slug', $1, true)", slug); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
