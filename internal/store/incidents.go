package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
)

// ErrIncidentNotOpen is returned by ManualCloseIncident when the incident exists but
// is already closed. The api turns it into a 409 (you cannot close a closed incident).
var ErrIncidentNotOpen = errors.New("incident is not open")

// IncidentAction is the persisted incident transition the store applies. It mirrors
// alerting.Action without importing that package, so the store stays free of the
// alerting dependency (the alerting service maps its Decision onto a store.Decision).
type IncidentAction int

const (
	// IncidentNone changes no incident; only the counters and watermark are written.
	IncidentNone IncidentAction = iota
	// IncidentOpen opens a new incident (guarded by the open-incident unique index).
	IncidentOpen
	// IncidentClose closes the open incident (conditional on it still being open).
	IncidentClose
)

// Decision is the persisted shape of one alerting round's outcome, the input to
// ApplyAlertDecision. The alerting service builds it from the pure
// alerting.Decision after running the state machine.
type Decision struct {
	Action IncidentAction

	NewConsecutive int        // consecutive_fails to write
	NewFirstFailAt *time.Time // first_fail_at to write (nil clears it)

	IncidentStartedAt time.Time            // set on IncidentOpen (first fail of the run)
	CauseReason       domain.FailureReason // set on IncidentOpen
	IncidentEndedAt   time.Time            // set on IncidentClose

	// NewSSLWarnedDays is ssl_warned_days to write (nil clears it). Renotify means
	// the incident is unchanged but a notify should fire against the open incident
	// (an ssl expiry threshold crossing, BACKLOG: SSL-expiry).
	NewSSLWarnedDays *int
	Renotify         bool
}

// AppliedDecision is what ApplyAlertDecision did. Applied is true only when the
// incident action actually happened (an open that won the unique index, or a close
// that touched a still-open row); the caller emits a notify only then. Skipped is
// true when the watermark dropped the whole round (a redelivery). On an open,
// Incident is the new incident; on a close, the closed incident with EndedAt set so
// the caller computes the recovery duration.
type AppliedDecision struct {
	Applied  bool
	Skipped  bool
	Incident *domain.Incident
}

// UpsertCheckResult writes one result and returns its row id. It is idempotent on
// (org_id, monitor_id, region, checked_at): a redelivered result re-finds the same
// row and returns the same id, so no duplicate row is ever created (RFC-002 6.2,
// ADR-0011). This is the control-plane persist that the alerting consumer owns: the
// worker emits the event only and never writes Postgres, so the durable row and the
// alerting trigger share one write path. The id is assigned here at persist time
// (not at the worker), which is why alerting reads it back for the watermark.
//
// ON CONFLICT DO UPDATE (not DO NOTHING) so the INSERT path RETURNING always yields
// the id: a plain DO NOTHING returns no row on conflict. The update touches one
// column with its own value, so the row content does not change on a redelivery.
// Org-scoped via WithOrg.
func (p *Pool) UpsertCheckResult(ctx context.Context, r *domain.CheckResult) (int64, error) {
	var failureReason *string
	if r.FailureReason != nil {
		s := string(*r.FailureReason)
		failureReason = &s
	}
	var id int64
	err := p.WithOrg(ctx, r.OrgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO check_results (org_id, monitor_id, region, scheduled_at, checked_at, healthy,
				failure_reason, status_code, latency_ms, error_text, cert_expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT (org_id, monitor_id, region, checked_at)
				DO UPDATE SET org_id = EXCLUDED.org_id
			RETURNING id`,
			r.OrgID, r.MonitorID, r.Region, r.ScheduledAt, r.CheckedAt, r.Healthy,
			failureReason, r.StatusCode, r.LatencyMs, r.ErrorText, r.CertExpiresAt,
		).Scan(&id)
	})
	if err != nil {
		return 0, err
	}
	r.ID = id
	return id, nil
}

// GetAlertState loads a monitor's alert state: the counters, the watermark, and the
// open incident (nil if none). It is the read the alerting service runs before the
// pure state machine. Org-scoped via WithOrg. Returns pgx.ErrNoRows if the monitor
// does not exist in the org.
func (p *Pool) GetAlertState(ctx context.Context, orgID, monitorID int64) (*domain.AlertState, error) {
	var st *domain.AlertState
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		s, err := scanAlertState(tx.QueryRow(ctx, `
			SELECT consecutive_fails, first_fail_at, last_applied_result_id, ssl_warned_days
			FROM monitors WHERE id = $1 AND org_id = $2`, monitorID, orgID))
		if err != nil {
			return err
		}
		open, err := getOpenIncidentTx(ctx, tx, orgID, monitorID)
		if err != nil {
			return err
		}
		s.OpenIncident = open
		st = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

// scanAlertState reads a monitor's alert columns from a row (tx-scoped). The open
// incident, if any, is loaded separately so this stays a single monitor row scan.
func scanAlertState(row pgx.Row) (*domain.AlertState, error) {
	var (
		st         domain.AlertState
		lastID     *int64
		firstAt    *time.Time
		consec     int
		sslWarnedD *int
	)
	if err := row.Scan(&consec, &firstAt, &lastID, &sslWarnedD); err != nil {
		return nil, err
	}
	st.ConsecutiveFails = consec
	st.FirstFailAt = firstAt
	st.LastAppliedResultID = lastID
	st.SSLWarnedDays = sslWarnedD
	return &st, nil
}

// ApplyAlertDecision persists one alerting round's decision idempotently in a single
// transaction (RFC-006 section 5.1). It re-loads the state FOR UPDATE so the apply is
// serialized per monitor, re-checks the watermark inside the transaction, then writes
// the incident action, the counters, and the watermark atomically. The caller passes
// the Decision it computed from the pure state machine (run against the state from
// GetAlertState); the watermark, the open-incident unique index, and the conditional
// close are the real redelivery guards, so a decision computed a moment earlier stays
// safe (per-monitor partitioning means the only concurrency is a rebalance overlap).
//
// maxResultID is the round's watermark candidate (the largest check_results.id in the
// round; single-region today, so the one result's id). firstResultID links the opened
// incident to the failing result (0 = unset). If maxResultID is not newer than the
// stored watermark the whole apply is a no-op and Skipped is true.
//
// On an open it returns the new incident; on a close the closed incident with EndedAt
// set so the caller computes the recovery duration. Applied is true only when the
// incident action actually happened (an open that won the unique index, or a close
// that touched a still-open row); the caller emits a notify only then.
func (p *Pool) ApplyAlertDecision(ctx context.Context, m *domain.Monitor, maxResultID, firstResultID int64, d Decision) (AppliedDecision, error) {
	var res AppliedDecision
	err := p.WithOrg(ctx, m.OrgID, func(tx pgx.Tx) error {
		// Lock the monitor's alert row so two consumers (a rebalance overlap) cannot
		// apply the same monitor concurrently. The watermark is the correctness
		// backstop; the lock keeps the common case clean.
		state, err := scanAlertState(tx.QueryRow(ctx, `
			SELECT consecutive_fails, first_fail_at, last_applied_result_id, ssl_warned_days
			FROM monitors WHERE id = $1 AND org_id = $2 FOR UPDATE`, m.ID, m.OrgID))
		if err != nil {
			return err
		}

		// Already applied this round or a newer one: drop before doing any work.
		if state.LastAppliedResultID != nil && maxResultID <= *state.LastAppliedResultID {
			res.Skipped = true
			return nil
		}

		// Load the open incident so the conditional close targets the right row.
		open, err := getOpenIncidentTx(ctx, tx, m.OrgID, m.ID)
		if err != nil {
			return err
		}

		switch d.Action {
		case IncidentOpen:
			inc := &domain.Incident{
				OrgID:       m.OrgID,
				MonitorID:   m.ID,
				StartedAt:   d.IncidentStartedAt,
				CauseReason: d.CauseReason,
			}
			if firstResultID != 0 {
				inc.FirstResultID = &firstResultID
			}
			// Guarded by uniq_incident_open: a concurrent open already in place makes
			// this return no row, so we treat it as already-open and emit nothing.
			var newID int64
			scanErr := tx.QueryRow(ctx, `
				INSERT INTO incidents (org_id, monitor_id, started_at, cause_reason, first_result_id)
				VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT (monitor_id) WHERE ended_at IS NULL DO NOTHING
				RETURNING id`,
				inc.OrgID, inc.MonitorID, inc.StartedAt, string(inc.CauseReason), inc.FirstResultID,
			).Scan(&newID)
			switch {
			case scanErr == nil:
				inc.ID = newID
				res.Incident = inc
				res.Applied = true
			case errors.Is(scanErr, pgx.ErrNoRows):
				// already open: no-op, no notify
			default:
				return scanErr
			}

		case IncidentClose:
			if open == nil {
				break
			}
			reason := string(domain.CloseRecovered)
			tag, uerr := tx.Exec(ctx, `
				UPDATE incidents SET ended_at = $1, close_reason = $2
				WHERE id = $3 AND ended_at IS NULL`,
				d.IncidentEndedAt, reason, open.ID)
			if uerr != nil {
				return uerr
			}
			if tag.RowsAffected() == 1 {
				closed := *open
				ended := d.IncidentEndedAt
				closed.EndedAt = &ended
				cr := domain.CloseRecovered
				closed.CloseReason = &cr
				res.Incident = &closed
				res.Applied = true
			}
		}

		// An ssl renotify (a tighter expiry threshold crossed) changes no incident,
		// but the caller must emit against the still-open incident, so surface it.
		if d.Renotify && open != nil {
			res.Incident = open
			res.Applied = true
		}

		// Counters + watermark + ssl warned level, guarded by the watermark so a
		// stale replay updates zero rows even if it slipped past the early check above.
		_, err = tx.Exec(ctx, `
			UPDATE monitors
			SET consecutive_fails = $1, first_fail_at = $2, last_applied_result_id = $3,
				ssl_warned_days = $6
			WHERE id = $4 AND org_id = $5
				AND (last_applied_result_id IS NULL OR last_applied_result_id < $3)`,
			d.NewConsecutive, d.NewFirstFailAt, maxResultID, m.ID, m.OrgID, d.NewSSLWarnedDays)
		return err
	})
	if err != nil {
		return AppliedDecision{}, err
	}
	return res, nil
}

// ListOrgIncidents returns the org's incidents across every monitor, newest first,
// paged by a cursor (the started_at to read before). When openOnly is true only the
// currently open incidents are returned (ended_at IS NULL). limit caps the page.
// Org-scoped, via WithOrg. This is the global incidents list the api serves at
// /orgs/{orgId}/incidents (the per-monitor list lives in ListIncidents).
func (p *Pool) ListOrgIncidents(ctx context.Context, orgID int64, openOnly bool, before *time.Time, limit int) ([]*domain.Incident, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []*domain.Incident
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, org_id, monitor_id, started_at, ended_at, cause_reason,
				close_reason, closed_by, first_result_id
			FROM incidents
			WHERE org_id = $1
				AND (NOT $2 OR ended_at IS NULL)
				AND ($3::timestamptz IS NULL OR started_at < $3)
			ORDER BY started_at DESC, id DESC
			LIMIT $4`,
			orgID, openOnly, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			inc, err := scanIncidentRow(rows)
			if err != nil {
				return err
			}
			out = append(out, inc)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetIncident loads one incident in the org by id, or pgx.ErrNoRows if it does not
// exist in the org (RLS hides other orgs' rows). Org-scoped, via WithOrg.
func (p *Pool) GetIncident(ctx context.Context, orgID, id int64) (*domain.Incident, error) {
	var inc *domain.Incident
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, org_id, monitor_id, started_at, ended_at, cause_reason,
				close_reason, closed_by, first_result_id
			FROM incidents
			WHERE id = $1 AND org_id = $2`, id, orgID)
		got, err := scanIncidentRow(row)
		if err != nil {
			return err
		}
		inc = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inc, nil
}

// ListIncidentAnnotations returns one incident's annotations oldest-first (the
// natural timeline order). Org-scoped, via WithOrg.
func (p *Pool) ListIncidentAnnotations(ctx context.Context, orgID, incidentID int64) ([]*domain.IncidentAnnotation, error) {
	var out []*domain.IncidentAnnotation
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, org_id, incident_id, author_user_id, note, created_at
			FROM incident_annotations
			WHERE incident_id = $1 AND org_id = $2
			ORDER BY created_at ASC, id ASC`, incidentID, orgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.IncidentAnnotation
			if err := rows.Scan(&a.ID, &a.OrgID, &a.IncidentID, &a.AuthorUserID, &a.Note, &a.CreatedAt); err != nil {
				return err
			}
			out = append(out, &a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AddIncidentAnnotation adds one note to an incident's timeline and returns the new
// row. It checks the incident exists in the org first (so a bad id is pgx.ErrNoRows,
// not a foreign-key error), then inserts. Org-scoped, via WithOrg.
func (p *Pool) AddIncidentAnnotation(ctx context.Context, orgID, incidentID, authorUserID int64, note string) (*domain.IncidentAnnotation, error) {
	a := &domain.IncidentAnnotation{OrgID: orgID, IncidentID: incidentID, Note: note}
	if authorUserID != 0 {
		uid := authorUserID
		a.AuthorUserID = &uid
	}
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		var exists int
		if err := tx.QueryRow(ctx,
			`SELECT 1 FROM incidents WHERE id = $1 AND org_id = $2`, incidentID, orgID).Scan(&exists); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO incident_annotations (org_id, incident_id, author_user_id, note)
			VALUES ($1,$2,$3,$4)
			RETURNING id, created_at`,
			orgID, incidentID, a.AuthorUserID, note,
		).Scan(&a.ID, &a.CreatedAt)
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// ManualCloseIncident closes an open incident as an operator override (PRD-002 4): it
// sets ended_at, a distinct manual close_reason, and closed_by, only while the row is
// still open. It returns the closed incident, ErrNoRows if no such incident exists in
// the org, or ErrIncidentNotOpen if it is already closed (the caller turns that into a
// 409). Org-scoped, via WithOrg. No notify is emitted: a manual close is an operator
// action, not a recovery, so the alerting/notify path is not touched.
func (p *Pool) ManualCloseIncident(ctx context.Context, orgID, id, byUserID int64, endedAt time.Time) (*domain.Incident, error) {
	var inc *domain.Incident
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		// Lock the row so a concurrent close (or a racing recovery) cannot both win.
		row := tx.QueryRow(ctx, `
			SELECT id, org_id, monitor_id, started_at, ended_at, cause_reason,
				close_reason, closed_by, first_result_id
			FROM incidents
			WHERE id = $1 AND org_id = $2
			FOR UPDATE`, id, orgID)
		got, err := scanIncidentRow(row)
		if err != nil {
			return err
		}
		if got.EndedAt != nil {
			return ErrIncidentNotOpen
		}
		var by *int64
		if byUserID != 0 {
			u := byUserID
			by = &u
		}
		reason := string(domain.CloseManual)
		if _, err := tx.Exec(ctx, `
			UPDATE incidents SET ended_at = $1, close_reason = $2, closed_by = $3
			WHERE id = $4 AND ended_at IS NULL`,
			endedAt, reason, by, id); err != nil {
			return err
		}
		ended := endedAt
		got.EndedAt = &ended
		cr := domain.CloseManual
		got.CloseReason = &cr
		got.ClosedBy = by
		inc = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inc, nil
}

// scanIncidentRow scans one incidents row in the standard column order into a
// domain.Incident. Used by the list, get, and manual-close reads.
func scanIncidentRow(row pgx.Row) (*domain.Incident, error) {
	var (
		inc         domain.Incident
		cause       string
		closeReason *string
	)
	if err := row.Scan(&inc.ID, &inc.OrgID, &inc.MonitorID, &inc.StartedAt, &inc.EndedAt,
		&cause, &closeReason, &inc.ClosedBy, &inc.FirstResultID); err != nil {
		return nil, err
	}
	inc.CauseReason = domain.FailureReason(cause)
	if closeReason != nil {
		cr := domain.CloseReason(*closeReason)
		inc.CloseReason = &cr
	}
	return &inc, nil
}

// getOpenIncidentTx loads the monitor's open incident inside a transaction, or nil if
// none is open. Used by ApplyAlertDecision after locking the monitor row.
func getOpenIncidentTx(ctx context.Context, tx pgx.Tx, orgID, monitorID int64) (*domain.Incident, error) {
	var (
		inc         domain.Incident
		cause       string
		closeReason *string
	)
	err := tx.QueryRow(ctx, `
		SELECT id, org_id, monitor_id, started_at, ended_at, cause_reason,
			close_reason, closed_by, first_result_id
		FROM incidents
		WHERE monitor_id = $1 AND org_id = $2 AND ended_at IS NULL`,
		monitorID, orgID).Scan(&inc.ID, &inc.OrgID, &inc.MonitorID, &inc.StartedAt, &inc.EndedAt,
		&cause, &closeReason, &inc.ClosedBy, &inc.FirstResultID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	inc.CauseReason = domain.FailureReason(cause)
	if closeReason != nil {
		cr := domain.CloseReason(*closeReason)
		inc.CloseReason = &cr
	}
	return &inc, nil
}
