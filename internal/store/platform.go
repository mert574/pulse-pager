package store

import (
	"context"
	"database/sql"
)

// PlatformMetrics is the cross-org snapshot the operator admin panel reads. It is
// not org-scoped: it counts every org's data, so it runs on the bare pool (not
// WithOrg) and the RLS-protected counts go through the SECURITY DEFINER functions
// in schema.sql. Counts only, never row data.
type PlatformMetrics struct {
	Users           int64
	Orgs            int64
	MonitorsTotal   int64
	MonitorsEnabled int64
	Channels        int64
	OrgsByPlan      []PlanCount
	MonitorsByType  []MonitorTypeCount
	Signups         []SignupPoint

	// Activation: orgs that ever created a monitor, and the median seconds from org
	// signup to its first monitor. MedianTimeToFirstMonitorSeconds is nil when no
	// org has a monitor yet.
	OrgsWithMonitor                 int64
	MedianTimeToFirstMonitorSeconds *float64
	// ActiveOrgs7d is orgs with an enabled monitor checked in the last 7 days.
	ActiveOrgs7d int64
}

// PlanCount is the number of orgs on one plan.
type PlanCount struct {
	Plan  string
	Count int64
}

// MonitorTypeCount is the number of monitors of one check type, across all orgs.
type MonitorTypeCount struct {
	Type  string
	Count int64
}

// SignupPoint is one UTC day's new users and orgs, for the 30-day trend.
type SignupPoint struct {
	Date  string
	Users int64
	Orgs  int64
}

// PlatformMetrics reads the platform-wide totals. users and organizations are
// global tables (no RLS), so they count directly; monitors and channels are
// RLS-protected, so they go through the platform_*_count functions that bypass it.
func (p *Pool) PlatformMetrics(ctx context.Context) (*PlatformMetrics, error) {
	m := &PlatformMetrics{}

	if err := p.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&m.Users); err != nil {
		return nil, err
	}
	if err := p.QueryRow(ctx, `SELECT count(*) FROM organizations`).Scan(&m.Orgs); err != nil {
		return nil, err
	}
	if err := p.QueryRow(ctx, `SELECT total, enabled FROM platform_monitor_counts()`).
		Scan(&m.MonitorsTotal, &m.MonitorsEnabled); err != nil {
		return nil, err
	}
	if err := p.QueryRow(ctx, `SELECT platform_channel_count()`).Scan(&m.Channels); err != nil {
		return nil, err
	}

	// activation: orgs with a monitor + median time-to-first-monitor (nullable).
	var ttfm sql.NullFloat64
	if err := p.QueryRow(ctx, `SELECT orgs_with_monitor, median_ttfm_seconds FROM platform_activation()`).
		Scan(&m.OrgsWithMonitor, &ttfm); err != nil {
		return nil, err
	}
	if ttfm.Valid {
		v := ttfm.Float64
		m.MedianTimeToFirstMonitorSeconds = &v
	}
	if err := p.QueryRow(ctx, `SELECT platform_active_orgs_7d()`).Scan(&m.ActiveOrgs7d); err != nil {
		return nil, err
	}

	// orgs grouped by plan, ordered so the FE renders a stable list.
	planRows, err := p.Query(ctx, `SELECT plan, count(*) FROM organizations GROUP BY plan ORDER BY plan`)
	if err != nil {
		return nil, err
	}
	defer planRows.Close()
	for planRows.Next() {
		var pc PlanCount
		if err := planRows.Scan(&pc.Plan, &pc.Count); err != nil {
			return nil, err
		}
		m.OrgsByPlan = append(m.OrgsByPlan, pc)
	}
	if err := planRows.Err(); err != nil {
		return nil, err
	}

	// monitors grouped by check type (via the SECURITY DEFINER function so the
	// cross-org count bypasses RLS), ordered for a stable FE list.
	typeRows, err := p.Query(ctx, `SELECT monitor_type, count FROM platform_monitor_counts_by_type() ORDER BY monitor_type`)
	if err != nil {
		return nil, err
	}
	defer typeRows.Close()
	for typeRows.Next() {
		var mc MonitorTypeCount
		if err := typeRows.Scan(&mc.Type, &mc.Count); err != nil {
			return nil, err
		}
		m.MonitorsByType = append(m.MonitorsByType, mc)
	}
	if err := typeRows.Err(); err != nil {
		return nil, err
	}

	// 30-day signup trend, one row per day including days with zero signups
	// (generate_series fills the gaps so the FE gets a continuous series).
	const signupSQL = `
WITH days AS (
  SELECT generate_series(
           (now() AT TIME ZONE 'utc')::date - interval '29 days',
           (now() AT TIME ZONE 'utc')::date,
           interval '1 day'
         )::date AS d
)
SELECT to_char(days.d, 'YYYY-MM-DD'),
       (SELECT count(*) FROM users u WHERE (u.created_at AT TIME ZONE 'utc')::date = days.d),
       (SELECT count(*) FROM organizations o WHERE (o.created_at AT TIME ZONE 'utc')::date = days.d)
FROM days
ORDER BY days.d`
	sigRows, err := p.Query(ctx, signupSQL)
	if err != nil {
		return nil, err
	}
	defer sigRows.Close()
	for sigRows.Next() {
		var sp SignupPoint
		if err := sigRows.Scan(&sp.Date, &sp.Users, &sp.Orgs); err != nil {
			return nil, err
		}
		m.Signups = append(m.Signups, sp)
	}
	if err := sigRows.Err(); err != nil {
		return nil, err
	}

	return m, nil
}
