package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/apigen"
	"pulse/internal/authn"
	"pulse/internal/authz"
	"pulse/internal/checkstate"
	"pulse/internal/domain"
	"pulse/internal/entitlements"
	"pulse/internal/events"
	"pulse/internal/region"
	"pulse/internal/store"
)

// This file implements the monitors slice (PRD-002): CRUD, check-now, and the
// results/incidents reads. The role gate is authz.Can (view = any member, create/
// edit/delete/check = member+). Full per-field validation runs server-side from
// PRD-002 appendix A; the plan gate (monitor cap, interval floor, region set) runs
// through internal/entitlements (PRD-006). Every error is the localizable
// {code, message, fields} envelope (RFC-012 / RFC-014). A create/update/delete/
// enable/disable publishes monitor.changed so the live schedule tracks it (PRD-006 5).

const (
	maxMonitorNameLen   = 200
	maxBodyContainsLen  = 1000
	maxBodyBytes        = 1 << 20 // ~1 MB (PRD-002 2.3)
	maxHeaders          = 50
	defaultResultsLimit = 100

	// Fixed scheduling for ssl monitors (BACKLOG: SSL-expiry). A cert changes
	// slowly, so we check once a day with a short connect timeout; these are not
	// user-set, the same way the 7/3/1-day notify thresholds are fixed.
	sslIntervalSeconds = 86400
	sslTimeoutSeconds  = 10
)

// MonitorPublisher publishes the monitor.changed signal so the scheduler picks up a
// live config change (RFC-002 monitor.changed, key: org_id). The api calls it after
// a create/update/delete; a nil publisher on the Server skips it.
type MonitorPublisher interface {
	MonitorChanged(ctx context.Context, orgID, monitorID int64) error
}

// CheckJobPublisher enqueues a check job onto the pipeline (the same topic the
// scheduler produces to), so check-now fans out per region through the worker instead
// of running in the api. A nil publisher on the Server skips enqueue (dev/test).
type CheckJobPublisher interface {
	PublishCheckJob(ctx context.Context, job events.CheckJob) error
}

// --- handlers ---

// ListMonitors returns the org's monitors with the derived status, last check, and
// open-incident flag (PRD-002 list view). Any member may view (ActionViewMonitoring).
func (s *Server) ListMonitors(ctx context.Context, _ apigen.ListMonitorsRequestObject) (apigen.ListMonitorsResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.ListMonitors401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canMonitor(p, authzView); !d.Allowed {
		return apigen.ListMonitors403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	rows, err := s.store.ListMonitors(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	floor := s.monitorLimits(ctx, p.OrgID).EffectiveIntervalFloor()
	items := make([]apigen.MonitorListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, monitorListItemDTO(row, floor))
	}
	return apigen.ListMonitors200JSONResponse(items), nil
}

// GetMonitor returns one monitor in the org (PRD-002). Any member may view; an
// unknown monitor (or one in another org, hidden by RLS) is a 404.
func (s *Server) GetMonitor(ctx context.Context, req apigen.GetMonitorRequestObject) (apigen.GetMonitorResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.GetMonitor401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canMonitor(p, authzView); !d.Allowed {
		return apigen.GetMonitor403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	id, ok := parseMonitorID(req.Id)
	if !ok {
		return apigen.GetMonitor404JSONResponse{NotFoundJSONResponse: notFound("monitor not found")}, nil
	}
	m, err := s.store.GetMonitor(ctx, p.OrgID, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.GetMonitor404JSONResponse{NotFoundJSONResponse: notFound("monitor not found")}, nil
		}
		return nil, err
	}
	dto := monitorDTO(m)
	// Attach the latest cert detail for an ssl monitor so the detail page can render
	// the certificate card (BACKLOG: SSL-expiry). Off the alerting hot path: this
	// read only happens on the detail view.
	if m.Type == domain.MonitorSSL {
		if cert, err := s.store.GetMonitorCert(ctx, p.OrgID, id); err != nil {
			return nil, err
		} else if cert != nil {
			c := certInfoDTO(cert)
			dto.Cert = &c
		}
	}
	return apigen.GetMonitor200JSONResponse(dto), nil
}

// CreateMonitor validates and creates a monitor (PRD-002, PRD-006). It gates on the
// manage-monitors role (member+), runs full per-field validation, then the plan gate
// (monitor cap, interval floor, region set), encrypts secret headers via the store,
// persists, and publishes monitor.changed.
func (s *Server) CreateMonitor(ctx context.Context, req apigen.CreateMonitorRequestObject) (apigen.CreateMonitorResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.CreateMonitor401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canMonitor(p, authzManage); !d.Allowed {
		return apigen.CreateMonitor403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	if req.Body == nil {
		return apigen.CreateMonitor422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	limits := s.monitorLimits(ctx, p.OrgID)
	m, fieldErrs := monitorFromInput(p.OrgID, 0, *req.Body, limits)
	if len(fieldErrs) > 0 {
		return apigen.CreateMonitor422JSONResponse{ValidationFailedJSONResponse: fieldValidationFailed(fieldErrs)}, nil
	}

	// Monitor cap: enabled monitors against the plan cap (PRD-006 4.3). Only an
	// enabled new monitor counts toward the cap, so a disabled create always fits.
	if m.Enabled {
		used, err := s.store.CountEnabledMonitors(ctx, p.OrgID)
		if err != nil {
			return nil, err
		}
		if used >= limits.MonitorsCap {
			return apigen.CreateMonitor402JSONResponse(monitorLimitReached(limits.MonitorsCap)), nil
		}
	}

	if _, err := s.store.CreateMonitor(ctx, m); err != nil {
		return nil, err
	}
	s.publishChanged(ctx, p.OrgID, m.ID)
	// No synchronous first check here: the scheduler dispatches a brand-new (never
	// checked) enabled monitor on its next tick (internal/scheduler dispatchDue), so it
	// gets checked right away through the normal pipeline, and the per-region live state
	// populates from that. Running a check inline would block create on a real HTTP
	// request and double-check the monitor.
	return apigen.CreateMonitor201JSONResponse(monitorDTO(m)), nil
}

// UpdateMonitor validates and overwrites a monitor (PRD-002). Same gate and
// validation as create; the monitor must exist in the org (404 otherwise). Enabling
// a disabled monitor re-checks the cap so an update cannot slip past it.
func (s *Server) UpdateMonitor(ctx context.Context, req apigen.UpdateMonitorRequestObject) (apigen.UpdateMonitorResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.UpdateMonitor401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canMonitor(p, authzManage); !d.Allowed {
		return apigen.UpdateMonitor403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	if req.Body == nil {
		return apigen.UpdateMonitor422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	id, ok := parseMonitorID(req.Id)
	if !ok {
		return apigen.UpdateMonitor404JSONResponse{NotFoundJSONResponse: notFound("monitor not found")}, nil
	}
	existing, err := s.store.GetMonitor(ctx, p.OrgID, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.UpdateMonitor404JSONResponse{NotFoundJSONResponse: notFound("monitor not found")}, nil
		}
		return nil, err
	}
	limits := s.monitorLimits(ctx, p.OrgID)
	m, fieldErrs := monitorFromInput(p.OrgID, id, *req.Body, limits)
	if len(fieldErrs) > 0 {
		return apigen.UpdateMonitor422JSONResponse{ValidationFailedJSONResponse: fieldValidationFailed(fieldErrs)}, nil
	}

	// Re-enabling a disabled monitor adds one to the enabled count, so it re-checks
	// the cap. A monitor that was already enabled does not (it already counts).
	if m.Enabled && !existing.Enabled {
		used, err := s.store.CountEnabledMonitors(ctx, p.OrgID)
		if err != nil {
			return nil, err
		}
		if used >= limits.MonitorsCap {
			return apigen.UpdateMonitor402JSONResponse(monitorLimitReached(limits.MonitorsCap)), nil
		}
	}

	updated, err := s.store.UpdateMonitor(ctx, m)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.UpdateMonitor404JSONResponse{NotFoundJSONResponse: notFound("monitor not found")}, nil
		}
		return nil, err
	}
	s.publishChanged(ctx, p.OrgID, updated.ID)
	return apigen.UpdateMonitor200JSONResponse(monitorDTO(updated)), nil
}

// DeleteMonitor removes a monitor and its results/incidents (cascade), then
// publishes monitor.changed so the scheduler drops it. Member+ may delete.
func (s *Server) DeleteMonitor(ctx context.Context, req apigen.DeleteMonitorRequestObject) (apigen.DeleteMonitorResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.DeleteMonitor401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canMonitor(p, authzManage); !d.Allowed {
		return apigen.DeleteMonitor403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	id, ok := parseMonitorID(req.Id)
	if !ok {
		// An unknown id reads as a no-op success (delete is idempotent).
		return apigen.DeleteMonitor204Response{}, nil
	}
	if _, err := s.store.DeleteMonitor(ctx, p.OrgID, id); err != nil {
		return nil, err
	}
	s.publishChanged(ctx, p.OrgID, id)
	return apigen.DeleteMonitor204Response{}, nil
}

// CheckNow runs a check for the monitor right now and returns its result (PRD-002
// 6, check-now). Member+ may run it. It loads the monitor (404 if unknown), runs the
// checker synchronously honoring the plan's failure-snapshot feature, persists the
// result (and the snapshot on a response-level failure), and returns the result.
// CheckNow triggers a check immediately (RFC-004 §9). It does not run the check in the
// api: it enqueues one job per region onto the same pipeline scheduled checks use, so a
// manual check is identical to a scheduled one and fans out to every region. It returns
// 202 with the regions set to "scheduled"; the per-region progress shows up in the
// monitor's live region states (GetMonitorRegionStates), which the frontend polls.
//
// Gates: member+ (manage); an API key is additionally subject to the plan's API-write
// gate (a manual check is a write-class action); a disabled monitor is 409; the
// per-monitor burst cooldown and per-org sustained budget are 429.
func (s *Server) CheckNow(ctx context.Context, req apigen.CheckNowRequestObject) (apigen.CheckNowResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.CheckNow401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canMonitor(p, authzManage); !d.Allowed {
		return apigen.CheckNow403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	// A manual check is a write: an API key on a read-only plan may not trigger it
	// (PRD-006). Human sessions are not subject to the API-write gate.
	plan := s.orgPlan(ctx, p.OrgID)
	if p.Kind == authz.ActorAPIKey && !entitlements.APIWriteAllowed(plan) {
		return apigen.CheckNow403JSONResponse{ForbiddenJSONResponse: apiWriteForbidden()}, nil
	}
	id, ok := parseMonitorID(req.Id)
	if !ok {
		return apigen.CheckNow404JSONResponse{NotFoundJSONResponse: notFound("monitor not found")}, nil
	}
	m, err := s.store.GetMonitor(ctx, p.OrgID, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.CheckNow404JSONResponse{NotFoundJSONResponse: notFound("monitor not found")}, nil
		}
		return nil, err
	}
	if !m.Enabled {
		return apigen.CheckNow409JSONResponse{ConflictJSONResponse: conflictCode("monitor_disabled", "enable the monitor to run a check")}, nil
	}

	// Manual-check rate limit (PRD-006): a per-monitor burst cooldown plus a per-org
	// sustained budget so the button cannot be used as free high-frequency monitoring or
	// to overload the probe fleet. It lives in Redis, so it holds across api instances
	// and survives a restart. If Redis errors, fail open (availability over the limit).
	if s.cooldown != nil {
		lim := entitlements.CheckNowLimitsFor(plan)
		if dec := checkNowAllowed(ctx, s.cooldown, p.OrgID, m.ID, lim); !dec.allowed {
			return checkNowRateLimitedResponse{dec: dec}, nil
		}
	}

	// Fan out one job per region, exactly like the scheduler (internal/scheduler), so a
	// manual check and a scheduled check are the same thing. Mark each region scheduled
	// first so the live element shows it queued immediately.
	regions := m.Regions
	if len(regions) == 0 {
		regions = []string{region.Default}
	}
	now := time.Now().UTC()
	out := make([]apigen.RegionState, 0, len(regions))
	for _, region := range regions {
		if s.state != nil {
			if serr := checkstate.SetScheduled(ctx, s.state, m.ID, region, m.IntervalSeconds); serr != nil {
				return nil, serr
			}
		}
		if s.jobs != nil {
			job := events.CheckJob{
				JobID:       fmt.Sprintf("checknow:%d:%s:%d", m.ID, region, now.Unix()),
				OrgID:       p.OrgID,
				Region:      region,
				ScheduledAt: now,
				Monitor:     *m,
			}
			if perr := s.jobs.PublishCheckJob(ctx, job); perr != nil {
				return nil, perr
			}
		}
		out = append(out, apigen.RegionState{Region: region, State: apigen.RegionStateState(checkstate.StateScheduled), UpdatedAt: now})
	}
	return apigen.CheckNow202JSONResponse(apigen.CheckNowAccepted{
		MonitorId: strconv.FormatInt(m.ID, 10),
		Regions:   out,
	}), nil
}

// CheckNowGate is the Redis-backed seam for the manual-check rate limit: the burst
// layer claims a per-monitor cooldown key atomically (SetIfAbsent) and the sustained
// layer is a fixed-window counter (Incr + Expire). TTL/GetCache read the remaining wait
// and the current window count. *kv.Client satisfies it; nil disables the limit.
type CheckNowGate interface {
	SetIfAbsent(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	TTL(ctx context.Context, key string) (time.Duration, error)
	Incr(ctx context.Context, key string) (int64, error)
	Expire(ctx context.Context, key string, ttl time.Duration) error
	GetCache(ctx context.Context, key string) (string, bool, error)
}

// rateDecision is the outcome of the manual-check rate limit: whether to allow, and if
// not, which layer/scope was hit and how long to wait, so the response can tell the
// frontend exactly what happened (countdown + upsell).
type rateDecision struct {
	allowed       bool
	retryAfter    int
	limit         string // "burst" | "window"
	scope         string // "monitor" | "org"
	max           int
	windowSeconds int
}

// checkNowAllowed runs the two-layer manual-check rate limit: a per-monitor burst
// cooldown and a per-org sustained window budget.
//
// Layer order matters: it peeks the sustained window first (without consuming) so a
// caller already over the org budget is told to wait the long window, not the short
// cooldown. Then it claims the burst cooldown (the consume step); only if that succeeds
// does it count the check against the org window. Because the cooldown lets at most one
// check per monitor through per gap, the window counter is effectively serialized, so
// the peek-then-incr is race-free in practice. Any Redis error fails open.
func checkNowAllowed(ctx context.Context, gate CheckNowGate, orgID, monitorID int64, lim entitlements.CheckNowLimits) rateDecision {
	cdKey := fmt.Sprintf("checknow:cd:%d:%d", orgID, monitorID)
	winKey := fmt.Sprintf("checknow:win:%d", orgID) // per-org, not per-monitor
	window := time.Duration(lim.WindowSeconds) * time.Second
	cooldown := time.Duration(lim.CooldownSeconds) * time.Second

	// Sustained (per-org) layer, peek only: if the budget is spent, wait the window.
	if v, ok, err := gate.GetCache(ctx, winKey); err == nil && ok {
		if n, perr := strconv.Atoi(v); perr == nil && n >= lim.MaxPerWindow {
			return rateDecision{retryAfter: retryAfterFrom(ctx, gate, winKey, window), limit: "window", scope: "org", max: lim.MaxPerWindow, windowSeconds: lim.WindowSeconds}
		}
	}

	// Burst (per-monitor) layer, consume: claim the cooldown slot for this monitor.
	acquired, err := gate.SetIfAbsent(ctx, cdKey, "1", cooldown)
	if err != nil {
		return rateDecision{allowed: true} // fail open
	}
	if !acquired {
		return rateDecision{retryAfter: retryAfterFrom(ctx, gate, cdKey, cooldown), limit: "burst", scope: "monitor", max: 1, windowSeconds: lim.CooldownSeconds}
	}

	// Sustained layer, consume: count this check, starting the window on the first.
	n, err := gate.Incr(ctx, winKey)
	if err == nil && n == 1 {
		_ = gate.Expire(ctx, winKey, window)
	}
	if err == nil && int(n) > lim.MaxPerWindow {
		return rateDecision{retryAfter: retryAfterFrom(ctx, gate, winKey, window), limit: "window", scope: "org", max: lim.MaxPerWindow, windowSeconds: lim.WindowSeconds}
	}
	return rateDecision{allowed: true}
}

// retryAfterFrom reports how many whole seconds to wait, read from the key's TTL and
// falling back to the full duration when the TTL is unknown. Never less than 1.
func retryAfterFrom(ctx context.Context, gate CheckNowGate, key string, full time.Duration) int {
	remaining := full
	if ttl, err := gate.TTL(ctx, key); err == nil && ttl > 0 {
		remaining = ttl
	}
	secs := int(remaining.Seconds())
	if secs < 1 {
		secs = 1
	}
	return secs
}

// checkNowRateLimitedResponse is the 429 for a rate-limited manual check. It sets the
// Retry-After header and a localizable envelope whose fields tell the frontend which
// layer/scope was hit, the limit, and to offer an upgrade.
type checkNowRateLimitedResponse struct {
	dec rateDecision
}

func (r checkNowRateLimitedResponse) VisitCheckNowResponse(w http.ResponseWriter) error {
	ra := r.dec.retryAfter
	fields := map[string]string{
		"retry_after":    strconv.Itoa(ra),
		"limit":          r.dec.limit,
		"scope":          r.dec.scope,
		"max":            strconv.Itoa(r.dec.max),
		"window_seconds": strconv.Itoa(r.dec.windowSeconds),
		"upgrade":        "check_now_rate_limited",
	}
	body := apigen.ErrorResponse{Error: apigen.Error{
		Code:    "check_now_rate_limited",
		Message: fmt.Sprintf("you are checking too often; try again in %d seconds", ra),
		Fields:  &fields,
	}}
	return apigen.CheckNow429JSONResponse{TooManyRequestsJSONResponse: apigen.TooManyRequestsJSONResponse{
		Body:    body,
		Headers: apigen.TooManyRequestsResponseHeaders{RetryAfter: &ra},
	}}.VisitCheckNowResponse(w)
}

// ListResults returns the monitor's check history, newest first, scoped to the
// range query (24h/7d/90d) and paged by the cursor. Any member may view.
func (s *Server) ListResults(ctx context.Context, req apigen.ListResultsRequestObject) (apigen.ListResultsResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.ListResults401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canMonitor(p, authzView); !d.Allowed {
		return apigen.ListResults403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	id, ok := parseMonitorID(req.Id)
	if !ok {
		return apigen.ListResults200JSONResponse(apigen.PageCheckResult{Items: []apigen.CheckResult{}}), nil
	}
	since := rangeSince(req.Params.Range)
	before := parseTimeCursor(req.Params.Cursor)
	region := ""
	if req.Params.Region != nil {
		region = *req.Params.Region
	}
	results, err := s.store.ListResults(ctx, p.OrgID, id, since, before, region, defaultResultsLimit)
	if err != nil {
		return nil, err
	}
	items := make([]apigen.CheckResult, 0, len(results))
	for _, r := range results {
		items = append(items, checkResultDTO(r))
	}
	page := apigen.PageCheckResult{Items: items}
	if len(results) == defaultResultsLimit {
		page.NextCursor = nextTimeCursor(results[len(results)-1].CheckedAt)
	}
	return apigen.ListResults200JSONResponse(page), nil
}

// ListMonitorIncidents returns the monitor's incidents, newest first, paged by the
// cursor. Any member may view.
func (s *Server) ListMonitorIncidents(ctx context.Context, req apigen.ListMonitorIncidentsRequestObject) (apigen.ListMonitorIncidentsResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.ListMonitorIncidents401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canMonitor(p, authzView); !d.Allowed {
		return apigen.ListMonitorIncidents403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	id, ok := parseMonitorID(req.Id)
	if !ok {
		return apigen.ListMonitorIncidents200JSONResponse(apigen.PageIncident{Items: []apigen.Incident{}}), nil
	}
	before := parseTimeCursor(req.Params.Cursor)
	incidents, err := s.store.ListIncidents(ctx, p.OrgID, id, before, defaultResultsLimit)
	if err != nil {
		return nil, err
	}
	items := make([]apigen.Incident, 0, len(incidents))
	for _, inc := range incidents {
		items = append(items, incidentDTO(inc))
	}
	page := apigen.PageIncident{Items: items}
	if len(incidents) == defaultResultsLimit {
		page.NextCursor = nextTimeCursor(incidents[len(incidents)-1].StartedAt)
	}
	return apigen.ListMonitorIncidents200JSONResponse(page), nil
}

// --- authz helper ---

// monitorAction names the two role gates the monitor handlers use.
type monitorAction int

const (
	authzView   monitorAction = iota // view = any member (ActionViewMonitoring)
	authzManage                      // create/edit/delete/check = member+ (ActionManageMonitors)
)

// canMonitor runs the role gate for a monitor action through authz.Can (never
// reimplemented here): view maps to ActionViewMonitoring (any member), manage maps to
// ActionManageMonitors (member+), per the PRD-001 7.2 matrix.
func (s *Server) canMonitor(p authn.Principal, action monitorAction) authz.Decision {
	a := authz.ActionViewMonitoring
	if action == authzManage {
		a = authz.ActionManageMonitors
	}
	return authz.Can(p.Actor(), a, authz.Resource{OrgID: p.OrgID})
}

// --- validation + mapping ---

// monitorFromInput validates a MonitorInput against PRD-002 appendix A and the plan
// limits, returning the domain.Monitor and a per-field error map (empty = valid).
// It does not touch the DB; the cap check runs separately in the handler.
func monitorFromInput(orgID, id int64, in apigen.MonitorInput, limits entitlements.MonitorLimits) (*domain.Monitor, map[string]string) {
	errs := map[string]string{}

	// Default an empty type to http so older clients keep working; validate it.
	typ := domain.MonitorType(in.Type)
	if in.Type == "" {
		typ = domain.MonitorHTTP
	}
	if typ != domain.MonitorHTTP && typ != domain.MonitorSSL {
		errs["type"] = "type must be one of http, ssl"
	}
	isSSL := typ == domain.MonitorSSL

	name := strings.TrimSpace(in.Name)
	if name == "" {
		errs["name"] = "name is required"
	} else if len(name) > maxMonitorNameLen {
		errs["name"] = fmt.Sprintf("name must be at most %d characters", maxMonitorNameLen)
	}

	// An ssl monitor checks a TLS cert, so the target is a host (an https URL or a
	// bare host[:port]); the http-only request fields (method/body/assertions) do
	// not apply and are forced to harmless defaults below.
	if isSSL {
		if !validSSLTarget(in.Url) {
			errs["url"] = "url must be a host (https URL or host[:port])"
		}
	} else if !validURL(in.Url) {
		errs["url"] = "url must be an absolute http or https URL with a host"
	}

	method := domain.Method(in.Method)
	if in.Method == "" || isSSL {
		method = "GET"
	}
	// expected_status_codes is optional (empty = do not assert the status). ssl
	// ignores it entirely, so force a harmless value there.
	expected := strings.TrimSpace(in.ExpectedStatusCodes)
	if isSSL {
		expected = "200"
	}
	if !isSSL {
		if !validMethod(method) {
			errs["method"] = "method must be one of GET, POST, PUT, PATCH, DELETE, HEAD"
		}
		if in.Body != "" {
			if !bodyAllowed(method) {
				errs["body"] = "body is only allowed for POST, PUT, or PATCH"
			} else if len(in.Body) > maxBodyBytes {
				errs["body"] = "body is too large"
			}
		}
		if expected != "" && !validExpectedStatusCodes(expected) {
			errs["expected_status_codes"] = "expected_status_codes must be codes (100..599) and/or 2xx/3xx/4xx/5xx"
		}
	}

	// Scheduling, regions, and the http-only request/assertion fields are all
	// user-set for http. For ssl they are fixed product behavior (like the notify
	// thresholds): a cert changes slowly, so we check once a day from one region and
	// open on the first failing check. We override them and skip their validation.
	timeoutSeconds := in.TimeoutSeconds
	intervalSeconds := in.IntervalSeconds
	failureThreshold := in.FailureThreshold
	dp := domain.DownPolicy(in.DownPolicy)
	if in.DownPolicy == "" {
		dp = domain.DownPolicyQuorum
	}
	body := in.Body
	bodyContains := in.BodyContains
	maxLatency := in.MaxLatencyMs
	var headers []domain.Header
	var regions []string

	if isSSL {
		timeoutSeconds = sslTimeoutSeconds
		intervalSeconds = sslIntervalSeconds // daily
		failureThreshold = 1                 // deterministic: open on the first failing check
		dp = domain.DownPolicyAny            // single region, so any == quorum == all
		regions = []string{region.Default}
		body = ""
		bodyContains = nil
		maxLatency = nil
	} else {
		if in.TimeoutSeconds < 1 || in.TimeoutSeconds > 60 {
			errs["timeout_seconds"] = "timeout_seconds must be between 1 and 60"
		}

		// interval: hard floor 30, >= timeout, and >= the plan's tier floor (PRD-006).
		floor := limits.EffectiveIntervalFloor()
		switch {
		case in.IntervalSeconds < entitlements.HardIntervalFloorSeconds:
			errs["interval_seconds"] = fmt.Sprintf("interval_seconds must be at least %d", entitlements.HardIntervalFloorSeconds)
		case in.TimeoutSeconds >= 1 && in.TimeoutSeconds <= 60 && in.IntervalSeconds < in.TimeoutSeconds:
			errs["interval_seconds"] = "interval_seconds must be greater than or equal to timeout_seconds"
		case in.IntervalSeconds < floor:
			errs["interval_seconds"] = belowFloorMessage(floor)
		}

		if in.MaxLatencyMs != nil && *in.MaxLatencyMs <= 0 {
			errs["max_latency_ms"] = "max_latency_ms must be a positive integer"
		}
		if in.BodyContains != nil && len(*in.BodyContains) > maxBodyContainsLen {
			errs["body_contains"] = fmt.Sprintf("body_contains must be at most %d characters", maxBodyContainsLen)
		}
		// A monitor needs something to assert: at least one of the status codes, a
		// body-contains string, or a max latency. All three empty means the check
		// could never fail, so reject it (surfaced on expected_status_codes).
		hasBodyContains := in.BodyContains != nil && strings.TrimSpace(*in.BodyContains) != ""
		hasMaxLatency := in.MaxLatencyMs != nil && *in.MaxLatencyMs > 0
		if expected == "" && !hasBodyContains && !hasMaxLatency && errs["expected_status_codes"] == "" {
			errs["expected_status_codes"] = "set at least one assertion: expected status codes, body contains, or max latency"
		}
		if in.FailureThreshold < 1 {
			errs["failure_threshold"] = "failure_threshold must be at least 1"
		}
		hs, headerErr := validateHeaders(in.Headers)
		if headerErr != "" {
			errs["headers"] = headerErr
		}
		headers = hs

		if !validDownPolicy(dp) {
			errs["down_policy"] = "down_policy must be one of any, quorum, all"
		}
		rs, regionErr := validateRegions(in.Regions, limits)
		if regionErr != "" {
			errs["regions"] = regionErr
		}
		regions = rs
	}

	channelIDs, channelErr := parseChannelIDs(in.NotificationChannelIds)
	if channelErr != "" {
		errs["notification_channel_ids"] = channelErr
	}

	m := &domain.Monitor{
		ID:                  id,
		OrgID:               orgID,
		Type:                typ,
		Name:                name,
		URL:                 in.Url,
		Method:              method,
		Headers:             headers,
		Body:                body,
		ExpectedStatusCodes: expected,
		TimeoutSeconds:      timeoutSeconds,
		IntervalSeconds:     intervalSeconds,
		Enabled:             in.Enabled,
		MaxLatencyMs:        maxLatency,
		BodyContains:        bodyContains,
		FailureThreshold:    failureThreshold,
		Regions:             regions,
		DownPolicy:          dp,
		ChannelIDs:          channelIDs,
	}
	return m, errs
}

// validURL enforces the appendix A url rule: an absolute URL with scheme http or
// https only, and a host.
func validURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}

// validSSLTarget accepts an ssl monitor's target: either an absolute https/http
// URL with a host, or a bare host[:port] (BACKLOG: SSL-expiry). The checker
// defaults the port to 443.
func validSSLTarget(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	if u, err := url.Parse(raw); err == nil && u.IsAbs() {
		return u.Host != ""
	}
	// Bare host or host:port. Reject anything with a scheme-like or path part.
	if strings.ContainsAny(raw, "/ ") {
		return false
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		return host != ""
	}
	return true // bare hostname, no port
}

func validMethod(m domain.Method) bool {
	switch m {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
		return true
	}
	return false
}

// bodyAllowed reports whether a body may be set for the method (POST/PUT/PATCH only).
func bodyAllowed(m domain.Method) bool {
	switch m {
	case "POST", "PUT", "PATCH":
		return true
	}
	return false
}

func validDownPolicy(d domain.DownPolicy) bool {
	switch d {
	case domain.DownPolicyAny, domain.DownPolicyQuorum, domain.DownPolicyAll:
		return true
	}
	return false
}

// validExpectedStatusCodes parses the comma-separated list of explicit codes
// (100..599) and/or class wildcards (2xx/3xx/4xx/5xx). It rejects an empty list and
// any token that is neither.
func validExpectedStatusCodes(raw string) bool {
	parts := strings.Split(raw, ",")
	seen := false
	for _, part := range parts {
		tok := strings.TrimSpace(part)
		if tok == "" {
			continue
		}
		seen = true
		switch strings.ToLower(tok) {
		case "2xx", "3xx", "4xx", "5xx", "1xx":
			continue
		}
		n, err := strconv.Atoi(tok)
		if err != nil || n < 100 || n > 599 {
			return false
		}
	}
	return seen
}

// validateHeaders enforces the appendix A header rules: non-empty keys, no duplicate
// keys, and at most maxHeaders. It returns the domain headers (secret default false)
// and an error message, empty when valid.
func validateHeaders(in []apigen.MonitorHeader) ([]domain.Header, string) {
	if len(in) == 0 {
		return nil, ""
	}
	if len(in) > maxHeaders {
		return nil, fmt.Sprintf("at most %d headers are allowed", maxHeaders)
	}
	out := make([]domain.Header, 0, len(in))
	seen := map[string]bool{}
	for _, h := range in {
		key := strings.TrimSpace(h.Key)
		if key == "" {
			return nil, "header keys cannot be empty"
		}
		lower := strings.ToLower(key)
		if seen[lower] {
			return nil, "header keys must be unique"
		}
		seen[lower] = true
		value := ""
		if h.Value != nil {
			value = *h.Value
		}
		out = append(out, domain.Header{Key: key, Value: value, Secret: h.Secret})
	}
	return out, ""
}

// validateRegions enforces the appendix A region rules plus the plan gate: required
// non-empty, no duplicates, each region in the plan's allowed set, and no more than
// the plan's per-monitor cap (PRD-006). It returns the cleaned region list.
func validateRegions(in []string, limits entitlements.MonitorLimits) ([]string, string) {
	if len(in) == 0 {
		return nil, "at least one region is required"
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, r := range in {
		region := strings.TrimSpace(r)
		if region == "" {
			return nil, "region codes cannot be empty"
		}
		if seen[region] {
			return nil, "regions must not contain duplicates"
		}
		seen[region] = true
		if !limits.AllowsRegion(region) {
			return nil, regionNotInPlanMessage(region)
		}
		out = append(out, region)
	}
	if len(out) > limits.RegionsPerMonitorCap {
		return nil, fmt.Sprintf("your plan allows at most %d regions per monitor", limits.RegionsPerMonitorCap)
	}
	return out, ""
}

// parseChannelIDs parses the string channel ids into int64s. An unparseable id is a
// per-field error. Existence in the org is checked when the channels store lands; for
// now the ids are stored as given (PRD-002 2.3 note: each must reference a channel in
// the same org, enforced once channels are a real resource).
func parseChannelIDs(in []string) ([]int64, string) {
	if len(in) == 0 {
		return nil, ""
	}
	out := make([]int64, 0, len(in))
	for _, raw := range in {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, "notification_channel_ids must be channel ids"
		}
		out = append(out, n)
	}
	return out, ""
}

// --- DTO mapping ---

// monitorDTO maps a domain.Monitor to the API Monitor shape. Secret header values
// are redacted (value omitted) so a secret is never returned (PRD-002 2.2 / master 13).
func monitorDTO(m *domain.Monitor) apigen.Monitor {
	headers := make([]apigen.MonitorHeader, 0, len(m.Headers))
	for _, h := range m.Headers {
		hdr := apigen.MonitorHeader{Key: h.Key, Secret: h.Secret}
		if !h.Secret {
			v := h.Value
			hdr.Value = &v
		}
		headers = append(headers, hdr)
	}
	channelIDs := make([]string, 0, len(m.ChannelIDs))
	for _, id := range m.ChannelIDs {
		channelIDs = append(channelIDs, strconv.FormatInt(id, 10))
	}
	return apigen.Monitor{
		Id:                     strconv.FormatInt(m.ID, 10),
		OrgId:                  strconv.FormatInt(m.OrgID, 10),
		Type:                   apigen.MonitorType(m.Type),
		Name:                   m.Name,
		Url:                    m.URL,
		Method:                 apigen.Method(m.Method),
		Headers:                headers,
		Body:                   m.Body,
		ExpectedStatusCodes:    m.ExpectedStatusCodes,
		TimeoutSeconds:         m.TimeoutSeconds,
		IntervalSeconds:        m.IntervalSeconds,
		Enabled:                m.Enabled,
		MaxLatencyMs:           m.MaxLatencyMs,
		BodyContains:           m.BodyContains,
		FailureThreshold:       m.FailureThreshold,
		NotificationChannelIds: channelIDs,
		Regions:                m.Regions,
		DownPolicy:             apigen.DownPolicy(m.DownPolicy),
		CreatedAt:              m.CreatedAt,
		UpdatedAt:              m.UpdatedAt,
	}
}

// monitorListItemDTO maps a list row to the API MonitorListItem, deriving the status
// from enabled + has-results + open-incident (domain.DeriveStatus, PRD-002 12.1) and
// the next scheduled check from the last check time + interval.
func monitorListItemDTO(row store.MonitorListRow, intervalFloor int) apigen.MonitorListItem {
	m := row.Monitor
	hasResults := row.LastCheckedAt != nil
	status := domain.DeriveStatus(m.Enabled, hasResults, row.IncidentOpen)
	// The effective cadence is the stored interval floored to what the plan allows, so
	// the UI never shows a faster cadence than the scheduler actually runs (PRD-006).
	eff := m.IntervalSeconds
	if eff < intervalFloor {
		eff = intervalFloor
	}
	return apigen.MonitorListItem{
		Id:              strconv.FormatInt(m.ID, 10),
		Type:            apigen.MonitorType(m.Type),
		Name:            m.Name,
		Url:             m.URL,
		Enabled:         m.Enabled,
		Status:          apigen.CoverageStatus(status),
		LastCheckAt:     row.LastCheckedAt,
		NextCheckAt:     nextCheckAt(m.Enabled, row.LastCheckedAt, eff),
		IntervalSeconds: eff,
		LastLatencyMs:   row.LastLatencyMs,
		IncidentOpen:    row.IncidentOpen,
		CertExpiresAt:   row.CertExpiresAt,
	}
}

// nextCheckAt is when the next scheduled check is due, derived from the last check
// time plus the interval. It is read from persisted state (the last check_result),
// so it survives a scheduler restart and matches what the scheduler computes. Returns
// nil for a disabled monitor (no next check) or one that has never been checked (it is
// due immediately, so the UI shows "soon" rather than a stale timestamp).
func nextCheckAt(enabled bool, lastCheckedAt *time.Time, intervalSeconds int) *time.Time {
	if !enabled || lastCheckedAt == nil {
		return nil
	}
	next := lastCheckedAt.Add(time.Duration(intervalSeconds) * time.Second)
	return &next
}

// checkResultDTO maps a domain.CheckResult to the API CheckResult shape.
func checkResultDTO(r *domain.CheckResult) apigen.CheckResult {
	dto := apigen.CheckResult{
		Id:          strconv.FormatInt(r.ID, 10),
		MonitorId:   strconv.FormatInt(r.MonitorID, 10),
		Region:      r.Region,
		ScheduledAt: r.ScheduledAt,
		CheckedAt:   r.CheckedAt,
		Healthy:     r.Healthy,
		StatusCode:  r.StatusCode,
		LatencyMs:   r.LatencyMs,
		Error:       r.ErrorText,
	}
	if r.FailureReason != nil {
		fr := apigen.FailureReason(*r.FailureReason)
		dto.FailureReason = &fr
	}
	dto.CertExpiresAt = r.CertExpiresAt
	return dto
}

// certInfoDTO maps the latest cert detail to the API CertInfo shape.
func certInfoDTO(c *domain.CertInfo) apigen.CertInfo {
	return apigen.CertInfo{
		Subject:   c.Subject,
		Issuer:    c.Issuer,
		NotBefore: c.NotBefore,
		NotAfter:  c.NotAfter,
		DnsNames:  c.DNSNames,
		Serial:    c.Serial,
	}
}

// incidentDTO maps a domain.Incident to the API Incident shape, computing the
// duration (open since started_at, or ended-started once closed).
func incidentDTO(inc *domain.Incident) apigen.Incident {
	dto := apigen.Incident{
		Id:          strconv.FormatInt(inc.ID, 10),
		MonitorId:   strconv.FormatInt(inc.MonitorID, 10),
		StartedAt:   inc.StartedAt,
		EndedAt:     inc.EndedAt,
		CauseReason: apigen.FailureReason(inc.CauseReason),
	}
	if inc.CloseReason != nil {
		cr := apigen.CloseReason(*inc.CloseReason)
		dto.CloseReason = &cr
	}
	if inc.EndedAt != nil {
		secs := int(inc.EndedAt.Sub(inc.StartedAt).Seconds())
		dto.DurationSeconds = &secs
	}
	return dto
}

// --- cursors + range ---

// rangeSince maps the results range query to the earliest checked_at to read. An
// absent or unknown range defaults to the last 24 hours.
func rangeSince(r *apigen.ResultsRange) time.Time {
	now := time.Now().UTC()
	if r == nil {
		return now.Add(-24 * time.Hour)
	}
	switch *r {
	case "7d":
		return now.Add(-7 * 24 * time.Hour)
	case "90d":
		return now.Add(-90 * 24 * time.Hour)
	default:
		return now.Add(-24 * time.Hour)
	}
}

// parseTimeCursor decodes an opaque time cursor (RFC3339 nanos). An empty or bad
// cursor reads as no cursor (start from the newest row).
func parseTimeCursor(c *string) *time.Time {
	if c == nil || *c == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, *c)
	if err != nil {
		return nil
	}
	return &t
}

// nextTimeCursor encodes the boundary time as the cursor for the next page.
func nextTimeCursor(t time.Time) *string {
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

// --- helpers ---

// parseMonitorID parses the path id into an int64.
func parseMonitorID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// monitorLimits resolves the org's monitor plan limits from its current plan
// (orgPlan reads the operator-set tier). The limits come from the resolver (PRD-006),
// not a literal.
func (s *Server) monitorLimits(ctx context.Context, orgID int64) entitlements.MonitorLimits {
	return s.monitors.MonitorLimits(orgID, s.orgPlan(ctx, orgID))
}

// publishChanged emits monitor.changed for the scheduler. A nil publisher or a publish
// error does not fail the request: the write already landed and the scheduler also
// rebuilds from Postgres on its scan, so a missed signal is at worst a short delay.
func (s *Server) publishChanged(ctx context.Context, orgID, monitorID int64) {
	if s.changed == nil {
		return
	}
	_ = s.changed.MonitorChanged(ctx, orgID, monitorID)
}

// --- localizable error envelopes ---

// fieldValidationFailed builds the per-field validation envelope (RFC-012 / RFC-014):
// the stable validation_failed code plus a fields map of {field: message}.
func fieldValidationFailed(fields map[string]string) apigen.ValidationFailedJSONResponse {
	return apigen.ValidationFailedJSONResponse{Error: apigen.Error{
		Code:    "validation_failed",
		Message: "one or more fields are invalid",
		Fields:  &fields,
	}}
}

// monitorLimitReached is the localizable upsell envelope when an org is at its
// monitor cap (PRD-006 6.1: code monitor_limit_reached). The cap rides in fields so
// the FE can interpolate the message (RFC-014).
func monitorLimitReached(cap int) apigen.ErrorResponse {
	fields := map[string]string{"limit": strconv.Itoa(cap)}
	return apigen.ErrorResponse{Error: apigen.Error{
		Code:    "monitor_limit_reached",
		Message: fmt.Sprintf("your plan allows %d enabled monitors; disable or delete one, or upgrade to add more", cap),
		Fields:  &fields,
	}}
}

// belowFloorMessage is the per-field message for an interval under the plan floor
// (PRD-006 6.1: the field error is interval_below_plan_floor; the code rides in the
// fields map alongside the floor so the FE can show the upsell).
func belowFloorMessage(floor int) string {
	return fmt.Sprintf("your plan's minimum check interval is %d seconds", floor)
}

// regionNotInPlanMessage is the per-field message for a region outside the plan set
// (PRD-006 6.1: region_not_in_plan).
func regionNotInPlanMessage(region string) string {
	return fmt.Sprintf("region %q is not included in your plan", region)
}
