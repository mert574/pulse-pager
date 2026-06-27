package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"pulse/internal/apigen"
	"pulse/internal/authn"
	"pulse/internal/authz"
	"pulse/internal/domain"
	"pulse/internal/store"
)

// This file implements the status-pages slice (PRD-004): the authed, org-scoped CRUD
// plus a publish toggle, and the UNAUTHENTICATED public read. The role gate is
// authz.Can: create/edit/publish/select-monitors = member+ (ActionManageStatusPages);
// custom-domain config = owner/admin (ActionConfigureCustomDomain, phased). The plan
// gate blocks creating past status_pages_cap (PRD-004 2.3) with the localizable
// status_page_limit_reached envelope. Every error is the {code, message, fields}
// envelope (RFC-012 / RFC-014).
//
// The public read (GetPublicStatusPage) is the privacy boundary: it returns the
// store's public projection, which is assembled only from the PRD-004 3.6 left-column
// fields. The raw monitor url/method/headers/body/assertions/failure detail are never
// selected into that projection, so they cannot leak through this endpoint. The public
// route is registered OUTSIDE Identify/RequireOrg (router.go), so it needs no auth and
// no org context.

const (
	maxStatusPageNameLen   = 200
	maxStatusPageSlugLen   = 63
	maxDisplayNameLen      = 200
	maxAccentColorLen      = 32
	maxLogoURLLen          = 2048
	publicPageCacheSeconds = 30
)

// slugPattern is the URL-safe slug rule (PRD-004 2): lowercase letters, digits, and
// hyphens, not starting or ending with a hyphen.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// --- handlers (management) ---

// ListStatusPages returns the org's status pages with their displayed-monitor lists.
// Any member may view (ActionViewMonitoring covers viewing in-app, PRD-004 10).
func (s *Server) ListStatusPages(ctx context.Context, _ apigen.ListStatusPagesRequestObject) (apigen.ListStatusPagesResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.ListStatusPages401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canStatusPage(p, statusPageView); !d.Allowed {
		return apigen.ListStatusPages403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	pages, err := s.store.ListStatusPages(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	out := make([]apigen.StatusPage, 0, len(pages))
	for _, sp := range pages {
		entries, err := s.store.ListStatusPageMonitors(ctx, p.OrgID, sp.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, statusPageDTO(sp, entries))
	}
	return apigen.ListStatusPages200JSONResponse(out), nil
}

// GetStatusPage returns one status page in the org with its displayed monitors. Any
// member may view; an unknown page (or one in another org, hidden by RLS) is a 404.
func (s *Server) GetStatusPage(ctx context.Context, req apigen.GetStatusPageRequestObject) (apigen.GetStatusPageResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.GetStatusPage401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canStatusPage(p, statusPageView); !d.Allowed {
		return apigen.GetStatusPage403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	id, ok := parseMonitorID(string(req.Id))
	if !ok {
		return apigen.GetStatusPage404JSONResponse{NotFoundJSONResponse: notFound("status page not found")}, nil
	}
	sp, entries, err := s.loadStatusPage(ctx, p.OrgID, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.GetStatusPage404JSONResponse{NotFoundJSONResponse: notFound("status page not found")}, nil
		}
		return nil, err
	}
	return apigen.GetStatusPage200JSONResponse(statusPageDTO(sp, entries)), nil
}

// CreateStatusPage validates and creates a status page (PRD-004 2). Member+ may
// create. It validates name/slug/branding and the displayed-monitor entries, runs the
// plan cap (status_pages_cap), persists the page, sets its displayed monitors (each
// must belong to the org), and returns the full page.
func (s *Server) CreateStatusPage(ctx context.Context, req apigen.CreateStatusPageRequestObject) (apigen.CreateStatusPageResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.CreateStatusPage401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canStatusPage(p, statusPageManage); !d.Allowed {
		return apigen.CreateStatusPage403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	if req.Body == nil {
		return apigen.CreateStatusPage422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	in := apigen.StatusPageInput(*req.Body)
	sp, entries, fieldErrs := statusPageFromInput(p.OrgID, 0, in)
	if len(fieldErrs) > 0 {
		return apigen.CreateStatusPage422JSONResponse{ValidationFailedJSONResponse: fieldValidationFailed(fieldErrs)}, nil
	}

	// Plan cap: an org cannot create more pages than its plan allows (PRD-004 2.3).
	cap := s.statusPageCap(ctx, p.OrgID)
	used, err := s.store.CountStatusPages(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	if used >= cap {
		return apigen.CreateStatusPage402JSONResponse(statusPageLimitReached(cap)), nil
	}

	created, err := s.store.CreateStatusPage(ctx, sp)
	if err != nil {
		if errors.Is(err, store.ErrSlugTaken) {
			return apigen.CreateStatusPage422JSONResponse{ValidationFailedJSONResponse: fieldValidationFailed(map[string]string{"slug": "slug is already taken"})}, nil
		}
		return nil, err
	}
	if resp, err := s.applyMonitors(ctx, p.OrgID, created.ID, entries); err != nil {
		return nil, err
	} else if resp != nil {
		return apigen.CreateStatusPage422JSONResponse{ValidationFailedJSONResponse: *resp}, nil
	}
	full, savedEntries, err := s.loadStatusPage(ctx, p.OrgID, created.ID)
	if err != nil {
		return nil, err
	}
	s.log.InfoContext(ctx, fmt.Sprintf("status page created: %d \"%s\" (/%s)", created.ID, created.Name, created.Slug),
		"status_page", created.ID, "slug", created.Slug, "org", p.OrgID, "user", p.UserID)
	return apigen.CreateStatusPage201JSONResponse(statusPageDTO(full, savedEntries)), nil
}

// UpdateStatusPage validates and overwrites a status page and its displayed monitors
// (PRD-004 2, 3). Same gate and validation as create; the page must exist in the org
// (404 otherwise). Publishing is a separate action (PublishStatusPage), so update
// keeps the existing published state.
func (s *Server) UpdateStatusPage(ctx context.Context, req apigen.UpdateStatusPageRequestObject) (apigen.UpdateStatusPageResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.UpdateStatusPage401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canStatusPage(p, statusPageManage); !d.Allowed {
		return apigen.UpdateStatusPage403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	if req.Body == nil {
		return apigen.UpdateStatusPage422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	id, ok := parseMonitorID(string(req.Id))
	if !ok {
		return apigen.UpdateStatusPage404JSONResponse{NotFoundJSONResponse: notFound("status page not found")}, nil
	}
	existing, err := s.store.GetStatusPage(ctx, p.OrgID, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.UpdateStatusPage404JSONResponse{NotFoundJSONResponse: notFound("status page not found")}, nil
		}
		return nil, err
	}
	in := apigen.StatusPageInput(*req.Body)
	sp, entries, fieldErrs := statusPageFromInput(p.OrgID, id, in)
	if len(fieldErrs) > 0 {
		return apigen.UpdateStatusPage422JSONResponse{ValidationFailedJSONResponse: fieldValidationFailed(fieldErrs)}, nil
	}
	// Update is content-only; the published state flips through PublishStatusPage.
	sp.Published = existing.Published

	if _, err := s.store.UpdateStatusPage(ctx, sp); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.UpdateStatusPage404JSONResponse{NotFoundJSONResponse: notFound("status page not found")}, nil
		}
		if errors.Is(err, store.ErrSlugTaken) {
			return apigen.UpdateStatusPage422JSONResponse{ValidationFailedJSONResponse: fieldValidationFailed(map[string]string{"slug": "slug is already taken"})}, nil
		}
		return nil, err
	}
	if resp, err := s.applyMonitors(ctx, p.OrgID, id, entries); err != nil {
		return nil, err
	} else if resp != nil {
		return apigen.UpdateStatusPage422JSONResponse{ValidationFailedJSONResponse: *resp}, nil
	}
	full, savedEntries, err := s.loadStatusPage(ctx, p.OrgID, id)
	if err != nil {
		return nil, err
	}
	return apigen.UpdateStatusPage200JSONResponse(statusPageDTO(full, savedEntries)), nil
}

// PublishStatusPage flips a page between draft and published (PRD-004 6). Member+ may
// publish. Publishing makes the public URL resolve; unpublishing returns it to draft.
func (s *Server) PublishStatusPage(ctx context.Context, req apigen.PublishStatusPageRequestObject) (apigen.PublishStatusPageResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.PublishStatusPage401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canStatusPage(p, statusPageManage); !d.Allowed {
		return apigen.PublishStatusPage403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	if req.Body == nil {
		return apigen.PublishStatusPage422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	id, ok := parseMonitorID(string(req.Id))
	if !ok {
		return apigen.PublishStatusPage404JSONResponse{NotFoundJSONResponse: notFound("status page not found")}, nil
	}
	sp, err := s.store.GetStatusPage(ctx, p.OrgID, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.PublishStatusPage404JSONResponse{NotFoundJSONResponse: notFound("status page not found")}, nil
		}
		return nil, err
	}
	sp.Published = req.Body.Published
	if _, err := s.store.UpdateStatusPage(ctx, sp); err != nil {
		return nil, err
	}
	full, entries, err := s.loadStatusPage(ctx, p.OrgID, id)
	if err != nil {
		return nil, err
	}
	return apigen.PublishStatusPage200JSONResponse(statusPageDTO(full, entries)), nil
}

// DeleteStatusPage removes a status page (and its displayed-monitor join, cascade).
// Member+ may delete.
func (s *Server) DeleteStatusPage(ctx context.Context, req apigen.DeleteStatusPageRequestObject) (apigen.DeleteStatusPageResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.DeleteStatusPage401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canStatusPage(p, statusPageManage); !d.Allowed {
		return apigen.DeleteStatusPage403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	id, ok := parseMonitorID(string(req.Id))
	if !ok {
		// An unknown id reads as a no-op success (delete is idempotent).
		return apigen.DeleteStatusPage204Response{}, nil
	}
	if _, err := s.store.DeleteStatusPage(ctx, p.OrgID, id); err != nil {
		return nil, err
	}
	return apigen.DeleteStatusPage204Response{}, nil
}

// --- handler (public) ---

// GetPublicStatusPage serves the public-safe projection of a PUBLISHED page by slug
// (PRD-004 3.6, 8). No auth, no org context: the store reads it on the public-page
// capability and returns only the left-column public fields. A draft or unknown slug
// is a 404 (PRD-004 6: a draft's existence is not leaked). A short cache header keeps
// it cacheable so the page stays up during a traffic spike (PRD-004 8).
func (s *Server) GetPublicStatusPage(ctx context.Context, req apigen.GetPublicStatusPageRequestObject) (apigen.GetPublicStatusPageResponseObject, error) {
	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		return apigen.GetPublicStatusPage404JSONResponse{NotFoundJSONResponse: notFound("status page not found")}, nil
	}
	page, err := s.store.GetPublicStatusPage(ctx, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.GetPublicStatusPage404JSONResponse{NotFoundJSONResponse: notFound("status page not found")}, nil
		}
		return nil, err
	}
	return publicStatusPageResponse{page: publicStatusPageDTO(page)}, nil
}

// publicStatusPageResponse wraps the 200 so a Cache-Control header rides on the public
// read (PRD-004 8: read-mostly and cacheable). It satisfies the generated response
// interface by delegating to the typed 200 after setting the header.
type publicStatusPageResponse struct {
	page apigen.PublicStatusPage
}

func (r publicStatusPageResponse) VisitGetPublicStatusPageResponse(w http.ResponseWriter) error {
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", publicPageCacheSeconds))
	return apigen.GetPublicStatusPage200JSONResponse(r.page).VisitGetPublicStatusPageResponse(w)
}

// --- authz helper ---

type statusPageAction int

const (
	statusPageView   statusPageAction = iota // view in app = any member (ActionViewMonitoring)
	statusPageManage                         // create/edit/publish/select = member+ (ActionManageStatusPages)
)

// canStatusPage runs the role gate through authz.Can (never reimplemented here): view
// maps to ActionViewMonitoring (any member), manage maps to ActionManageStatusPages
// (member+), per the PRD-004 10 / PRD-001 7.2 matrix.
func (s *Server) canStatusPage(p authn.Principal, action statusPageAction) authz.Decision {
	a := authz.ActionViewMonitoring
	if action == statusPageManage {
		a = authz.ActionManageStatusPages
	}
	return authz.Can(p.Actor(), a, authz.Resource{OrgID: p.OrgID})
}

// --- validation + mapping ---

// statusPageFromInput validates a StatusPageInput and returns the domain.StatusPage,
// the displayed-monitor entries, and a per-field error map (empty = valid). It does
// not touch the DB; the cap check and the monitor-in-org check run in the handler/store.
func statusPageFromInput(orgID, id int64, in apigen.StatusPageInput) (*domain.StatusPage, []*domain.StatusPageMonitor, map[string]string) {
	errs := map[string]string{}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		errs["name"] = "name is required"
	} else if len(name) > maxStatusPageNameLen {
		errs["name"] = fmt.Sprintf("name must be at most %d characters", maxStatusPageNameLen)
	}

	slug := strings.ToLower(strings.TrimSpace(in.Slug))
	if slug == "" {
		errs["slug"] = "slug is required"
	} else if len(slug) > maxStatusPageSlugLen {
		errs["slug"] = fmt.Sprintf("slug must be at most %d characters", maxStatusPageSlugLen)
	} else if !slugPattern.MatchString(slug) {
		errs["slug"] = "slug must be lowercase letters, digits, and hyphens"
	}

	theme := domain.StatusPageTheme(in.Theme)
	if in.Theme == "" {
		theme = domain.ThemeLight
	}
	if theme != domain.ThemeLight && theme != domain.ThemeDark {
		errs["theme"] = "theme must be light or dark"
	}

	if len(in.AccentColor) > maxAccentColorLen {
		errs["accent_color"] = "accent_color is too long"
	}
	if len(in.LogoUrl) > maxLogoURLLen {
		errs["logo_url"] = "logo_url is too long"
	}

	entries := make([]*domain.StatusPageMonitor, 0, len(in.DisplayMonitors))
	seen := map[int64]bool{}
	for i, e := range in.DisplayMonitors {
		dn := strings.TrimSpace(e.DisplayName)
		if dn == "" {
			errs[fmt.Sprintf("display_monitors.%d.display_name", i)] = "display_name is required"
			continue
		}
		if len(dn) > maxDisplayNameLen {
			errs[fmt.Sprintf("display_monitors.%d.display_name", i)] = "display_name is too long"
			continue
		}
		mid, err := strconv.ParseInt(e.MonitorId, 10, 64)
		if err != nil || mid <= 0 {
			errs[fmt.Sprintf("display_monitors.%d.monitor_id", i)] = "monitor_id must be a monitor id"
			continue
		}
		if seen[mid] {
			errs[fmt.Sprintf("display_monitors.%d.monitor_id", i)] = "a monitor can appear once on a page"
			continue
		}
		seen[mid] = true
		entries = append(entries, &domain.StatusPageMonitor{
			OrgID:       orgID,
			MonitorID:   mid,
			DisplayName: dn,
			SortOrder:   i,
		})
	}

	var customDomain *string
	if in.CustomDomain != nil && strings.TrimSpace(*in.CustomDomain) != "" {
		cd := strings.TrimSpace(*in.CustomDomain)
		customDomain = &cd
	}

	sp := &domain.StatusPage{
		ID:           id,
		OrgID:        orgID,
		Name:         name,
		Slug:         slug,
		LogoURL:      in.LogoUrl,
		AccentColor:  in.AccentColor,
		Theme:        theme,
		CustomDomain: customDomain,
	}
	return sp, entries, errs
}

// applyMonitors sets the page's displayed monitors and turns a not-in-org monitor into
// the per-field validation envelope (returned non-nil) rather than a 500. A nil
// response with a nil error means the monitors were set.
func (s *Server) applyMonitors(ctx context.Context, orgID, pageID int64, entries []*domain.StatusPageMonitor) (*apigen.ValidationFailedJSONResponse, error) {
	err := s.store.SetStatusPageMonitors(ctx, orgID, pageID, entries)
	switch {
	case err == nil:
		return nil, nil
	case errors.Is(err, store.ErrMonitorNotInOrg):
		resp := fieldValidationFailed(map[string]string{"display_monitors": "a monitor is not in this org"})
		return &resp, nil
	case errors.Is(err, store.ErrStatusPageNotFound):
		// The page was just created/loaded in the same request, so this is unexpected;
		// surface it as a generic validation failure rather than a 500.
		resp := validationFailed("status page not found")
		return &resp, nil
	default:
		return nil, err
	}
}

// loadStatusPage reads a page and its displayed-monitor entries together.
func (s *Server) loadStatusPage(ctx context.Context, orgID, id int64) (*domain.StatusPage, []*domain.StatusPageMonitor, error) {
	sp, err := s.store.GetStatusPage(ctx, orgID, id)
	if err != nil {
		return nil, nil, err
	}
	entries, err := s.store.ListStatusPageMonitors(ctx, orgID, id)
	if err != nil {
		return nil, nil, err
	}
	return sp, entries, nil
}

// statusPageCap resolves the org's status-page count cap. The plan is free until the
// billing catalog lands; the cap comes from the resolver (PRD-006), not a literal.
func (s *Server) statusPageCap(ctx context.Context, orgID int64) int {
	return s.statusPages.StatusPageCap(orgID, s.orgPlan(ctx, orgID))
}

// --- DTO mapping ---

// statusPageDTO maps a domain.StatusPage plus its displayed-monitor entries to the API
// shape the editor sees.
func statusPageDTO(sp *domain.StatusPage, entries []*domain.StatusPageMonitor) apigen.StatusPage {
	monitors := make([]apigen.StatusPageMonitorEntry, 0, len(entries))
	for _, e := range entries {
		monitors = append(monitors, apigen.StatusPageMonitorEntry{
			MonitorId:   strconv.FormatInt(e.MonitorID, 10),
			DisplayName: e.DisplayName,
			Order:       e.SortOrder,
		})
	}
	return apigen.StatusPage{
		Id:              strconv.FormatInt(sp.ID, 10),
		OrgId:           strconv.FormatInt(sp.OrgID, 10),
		Name:            sp.Name,
		Slug:            sp.Slug,
		LogoUrl:         sp.LogoURL,
		AccentColor:     sp.AccentColor,
		Theme:           apigen.StatusPageTheme(sp.Theme),
		State:           apigen.StatusPageState(sp.State()),
		CustomDomain:    sp.CustomDomain,
		DisplayMonitors: monitors,
		CreatedAt:       sp.CreatedAt,
		UpdatedAt:       sp.UpdatedAt,
	}
}

// publicStatusPageDTO maps the store's public projection to the API public shape. It
// carries only the public-safe fields (PRD-004 3.6); there is no path for an internal
// monitor field to appear, because the projection never holds one.
func publicStatusPageDTO(page *domain.PublicStatusPage) apigen.PublicStatusPage {
	monitors := make([]apigen.PublicDisplayedMonitor, 0, len(page.Monitors))
	for _, m := range page.Monitors {
		history := make([]apigen.PublicHistoryPoint, 0, len(m.History))
		for _, h := range m.History {
			history = append(history, apigen.PublicHistoryPoint{At: h.At, Up: h.Up})
		}
		monitors = append(monitors, apigen.PublicDisplayedMonitor{
			DisplayName: m.DisplayName,
			Status:      apigen.PublicMonitorStatus(m.Status),
			Uptime: apigen.PublicUptime{
				Uptime24h: float32(m.Uptime.Uptime24h),
				Uptime7d:  float32(m.Uptime.Uptime7d),
				Uptime90d: float32(m.Uptime.Uptime90d),
				Has24h:    m.Uptime.Has24h,
				Has7d:     m.Uptime.Has7d,
				Has90d:    m.Uptime.Has90d,
			},
			History: history,
		})
	}
	incidents := make([]apigen.PublicIncident, 0, len(page.Incidents))
	for _, inc := range page.Incidents {
		incidents = append(incidents, apigen.PublicIncident{
			DisplayName:     inc.DisplayName,
			StartedAt:       inc.StartedAt,
			EndedAt:         inc.EndedAt,
			DurationSeconds: inc.DurationSeconds,
			Resolved:        inc.Resolved,
		})
	}
	return apigen.PublicStatusPage{
		Name:            page.Name,
		Slug:            page.Slug,
		LogoUrl:         page.LogoURL,
		AccentColor:     page.AccentColor,
		Theme:           apigen.StatusPageTheme(page.Theme),
		Banner:          apigen.PublicBanner(page.Banner),
		Monitors:        monitors,
		Incidents:       incidents,
		UptimeMaxWindow: page.UptimeMaxWindow,
	}
}

// --- localizable error envelope ---

// statusPageLimitReached is the localizable upsell envelope when an org is at its
// status-page cap (PRD-004 2.3: code status_page_limit_reached). The cap rides in
// fields so the FE can interpolate the message (RFC-014).
func statusPageLimitReached(cap int) apigen.ErrorResponse {
	fields := map[string]string{"limit": strconv.Itoa(cap)}
	return apigen.ErrorResponse{Error: apigen.Error{
		Code:    "status_page_limit_reached",
		Message: fmt.Sprintf("your plan allows %d status pages; delete one or upgrade to add more", cap),
		Fields:  &fields,
	}}
}
