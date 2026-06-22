package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/apigen"
	"pulse/internal/authz"
	"pulse/internal/domain"
	"pulse/internal/store"
)

// This file implements the incidents slice (PRD-002 4): the global org incident list,
// the incident detail with its annotation timeline, adding an annotation, and the
// manual close. The role gate is authz.Can: view/list/annotate = any member
// (ActionViewMonitoring), manual close = owner/admin (ActionCloseIncident, PRD-001
// 7.2). A manual close is an operator override, not a recovery: it just closes the row
// with a distinct manual close_reason and emits NO recovery notification (the
// alerting/notify path is left untouched). Every error is the localizable
// {code, message} envelope (RFC-012 / RFC-014).

const maxAnnotationLen = 5000

// ListIncidents returns the org's incidents across every monitor, newest first, paged
// by the cursor. status=open filters to currently open incidents; any other value (or
// none) returns all. Any member may view (ActionViewMonitoring).
func (s *Server) ListIncidents(ctx context.Context, req apigen.ListIncidentsRequestObject) (apigen.ListIncidentsResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.ListIncidents401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canMonitor(p, authzView); !d.Allowed {
		return apigen.ListIncidents403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	openOnly := req.Params.Status != nil && *req.Params.Status == apigen.ListIncidentsParamsStatusOpen
	before := parseTimeCursor(req.Params.Cursor)
	incidents, err := s.store.ListOrgIncidents(ctx, p.OrgID, openOnly, before, defaultResultsLimit)
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
	return apigen.ListIncidents200JSONResponse(page), nil
}

// GetIncident returns one incident with its annotation timeline (PRD-002 4). Any
// member may view; an unknown incident (or one in another org, hidden by RLS) is 404.
func (s *Server) GetIncident(ctx context.Context, req apigen.GetIncidentRequestObject) (apigen.GetIncidentResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.GetIncident401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canMonitor(p, authzView); !d.Allowed {
		return apigen.GetIncident403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	id, ok := parseMonitorID(req.Id)
	if !ok {
		return apigen.GetIncident404JSONResponse{NotFoundJSONResponse: notFound("incident not found")}, nil
	}
	inc, err := s.store.GetIncident(ctx, p.OrgID, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.GetIncident404JSONResponse{NotFoundJSONResponse: notFound("incident not found")}, nil
		}
		return nil, err
	}
	annotations, err := s.store.ListIncidentAnnotations(ctx, p.OrgID, id)
	if err != nil {
		return nil, err
	}
	return apigen.GetIncident200JSONResponse(incidentDetailDTO(inc, annotations)), nil
}

// AddIncidentAnnotation adds a note to the incident timeline (PRD-002 4). Member+ may
// annotate. A blank note is a 422; an unknown incident is a 404.
func (s *Server) AddIncidentAnnotation(ctx context.Context, req apigen.AddIncidentAnnotationRequestObject) (apigen.AddIncidentAnnotationResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.AddIncidentAnnotation401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := s.canMonitor(p, authzView); !d.Allowed {
		return apigen.AddIncidentAnnotation403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	if req.Body == nil {
		return apigen.AddIncidentAnnotation422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	note := strings.TrimSpace(req.Body.Note)
	if note == "" {
		return apigen.AddIncidentAnnotation422JSONResponse{ValidationFailedJSONResponse: fieldValidationFailed(map[string]string{"note": "note is required"})}, nil
	}
	if len(note) > maxAnnotationLen {
		return apigen.AddIncidentAnnotation422JSONResponse{ValidationFailedJSONResponse: fieldValidationFailed(map[string]string{"note": "note is too long"})}, nil
	}
	id, ok := parseMonitorID(req.Id)
	if !ok {
		return apigen.AddIncidentAnnotation404JSONResponse{NotFoundJSONResponse: notFound("incident not found")}, nil
	}
	a, err := s.store.AddIncidentAnnotation(ctx, p.OrgID, id, p.UserID, note)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.AddIncidentAnnotation404JSONResponse{NotFoundJSONResponse: notFound("incident not found")}, nil
		}
		return nil, err
	}
	return apigen.AddIncidentAnnotation201JSONResponse(annotationDTO(a)), nil
}

// CloseIncident manually closes an open incident as an operator override (PRD-002 4).
// Owner or admin only (ActionCloseIncident). It sets a distinct manual close_reason
// and emits NO recovery notification: this is an operator action, not a recovery, so
// the alerting/notify path is not touched. An unknown incident is a 404; one that is
// already closed is a 409.
func (s *Server) CloseIncident(ctx context.Context, req apigen.CloseIncidentRequestObject) (apigen.CloseIncidentResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.CloseIncident401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionCloseIncident, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.CloseIncident403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	id, ok := parseMonitorID(req.Id)
	if !ok {
		return apigen.CloseIncident404JSONResponse{NotFoundJSONResponse: notFound("incident not found")}, nil
	}
	now := time.Now().UTC()
	inc, err := s.store.ManualCloseIncident(ctx, p.OrgID, id, p.UserID, now)
	if err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return apigen.CloseIncident404JSONResponse{NotFoundJSONResponse: notFound("incident not found")}, nil
		case errors.Is(err, store.ErrIncidentNotOpen):
			return apigen.CloseIncident409JSONResponse{ConflictJSONResponse: conflict("incident is already closed")}, nil
		default:
			return nil, err
		}
	}
	// Re-read the annotations so the detail returned after a close still carries the
	// timeline. The close itself adds no annotation.
	annotations, err := s.store.ListIncidentAnnotations(ctx, p.OrgID, id)
	if err != nil {
		return nil, err
	}
	return apigen.CloseIncident200JSONResponse(incidentDetailDTO(inc, annotations)), nil
}

// --- DTO mapping ---

// incidentDetailDTO maps an incident plus its annotations to the API IncidentDetail
// shape. It reuses incidentDTO for the shared fields so the duration/close-reason
// logic stays in one place.
func incidentDetailDTO(inc *domain.Incident, annotations []*domain.IncidentAnnotation) apigen.IncidentDetail {
	base := incidentDTO(inc)
	items := make([]apigen.IncidentAnnotation, 0, len(annotations))
	for _, a := range annotations {
		items = append(items, annotationDTO(a))
	}
	return apigen.IncidentDetail{
		Id:              base.Id,
		MonitorId:       base.MonitorId,
		StartedAt:       base.StartedAt,
		EndedAt:         base.EndedAt,
		CauseReason:     base.CauseReason,
		CloseReason:     base.CloseReason,
		DurationSeconds: base.DurationSeconds,
		Annotations:     items,
	}
}

// annotationDTO maps a domain.IncidentAnnotation to the API shape. A nil author (the
// writer was removed) is rendered as an empty string id.
func annotationDTO(a *domain.IncidentAnnotation) apigen.IncidentAnnotation {
	author := ""
	if a.AuthorUserID != nil {
		author = strconv.FormatInt(*a.AuthorUserID, 10)
	}
	return apigen.IncidentAnnotation{
		Id:           strconv.FormatInt(a.ID, 10),
		IncidentId:   strconv.FormatInt(a.IncidentID, 10),
		AuthorUserId: author,
		Note:         a.Note,
		CreatedAt:    a.CreatedAt,
	}
}
