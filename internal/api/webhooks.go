package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"pulse/internal/apigen"
	"pulse/internal/authz"
	"pulse/internal/domain"
)

// This file implements the org-level outbound-webhook management slice (PRD-005 7,
// RFC-007 7): register/list/get/update/rotate-secret/delete per-org webhooks for the
// org event stream. This is DISTINCT from the per-monitor generic-webhook channel
// (PRD-003). The role gate is authz.ActionManageWebhooks (owner/admin only, like API
// keys). The signing secret is returned exactly once on create and on rotate, and is
// never present on list/get (it is stored encrypted at rest). Every error is the
// localizable {code, message} envelope (RFC-012 / RFC-014).

// knownWebhookEvents is the set of org event types a webhook may subscribe to
// (PRD-005 7.1). An empty subscription list means "all types".
var knownWebhookEvents = map[domain.OrgWebhookEvent]bool{
	domain.OrgEventMonitorDown:    true,
	domain.OrgEventMonitorRecover: true,
	domain.OrgEventIncidentOpened: true,
	domain.OrgEventIncidentClosed: true,
}

// ListWebhooks returns the org's outbound webhooks, metadata only (the signing secret
// is never returned). Owner/admin only.
func (s *Server) ListWebhooks(ctx context.Context, _ apigen.ListWebhooksRequestObject) (apigen.ListWebhooksResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.ListWebhooks401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageWebhooks, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.ListWebhooks403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	hooks, err := s.store.ListWebhooks(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	out := make([]apigen.OutboundWebhook, 0, len(hooks))
	for _, w := range hooks {
		out = append(out, webhookDTO(w))
	}
	return apigen.ListWebhooks200JSONResponse(out), nil
}

// GetWebhook returns one webhook in the org (secret redacted). An unknown id (or one
// in another org, hidden by RLS) is a 404. Owner/admin only.
func (s *Server) GetWebhook(ctx context.Context, req apigen.GetWebhookRequestObject) (apigen.GetWebhookResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.GetWebhook401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageWebhooks, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.GetWebhook403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	id, ok := parseMonitorID(string(req.Id))
	if !ok {
		return apigen.GetWebhook404JSONResponse{NotFoundJSONResponse: notFound("webhook not found")}, nil
	}
	w, err := s.store.GetWebhook(ctx, p.OrgID, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.GetWebhook404JSONResponse{NotFoundJSONResponse: notFound("webhook not found")}, nil
		}
		return nil, err
	}
	return apigen.GetWebhook200JSONResponse(webhookDTO(w)), nil
}

// CreateWebhook registers an outbound webhook (PRD-005 7.4). It validates the url
// (absolute https) and the subscribed event types, mints a signing secret, stores it
// encrypted, and returns the FULL secret exactly once plus the webhook metadata.
// Owner/admin only.
func (s *Server) CreateWebhook(ctx context.Context, req apigen.CreateWebhookRequestObject) (apigen.CreateWebhookResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.CreateWebhook401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageWebhooks, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.CreateWebhook403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	if req.Body == nil {
		return apigen.CreateWebhook422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	in := apigen.OutboundWebhookInput(*req.Body)
	url, events, fieldErrs := webhookFromInput(in)
	if len(fieldErrs) > 0 {
		return apigen.CreateWebhook422JSONResponse{ValidationFailedJSONResponse: fieldValidationFailed(fieldErrs)}, nil
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}

	secret, err := newWebhookSecret()
	if err != nil {
		return nil, err
	}
	creator := p.UserID
	w := &domain.OrgWebhook{
		OrgID:         p.OrgID,
		URL:           url,
		SigningSecret: secret,
		Enabled:       enabled,
		Events:        events,
		CreatedBy:     &creator,
	}
	if _, err := s.store.CreateWebhook(ctx, w); err != nil {
		return nil, err
	}
	// Re-read so created_at/updated_at (DB defaults) are populated for the DTO.
	stored, err := s.store.GetWebhook(ctx, p.OrgID, w.ID)
	if err != nil {
		return nil, err
	}
	return apigen.CreateWebhook201JSONResponse(apigen.OutboundWebhookCreated{
		Webhook: webhookDTO(stored),
		Secret:  secret,
	}), nil
}

// UpdateWebhook overwrites a webhook's url, enabled flag, and subscribed events. The
// signing secret is not touched (RotateWebhookSecret owns that). The webhook must
// exist in the org (404 otherwise). Owner/admin only.
func (s *Server) UpdateWebhook(ctx context.Context, req apigen.UpdateWebhookRequestObject) (apigen.UpdateWebhookResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.UpdateWebhook401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageWebhooks, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.UpdateWebhook403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	if req.Body == nil {
		return apigen.UpdateWebhook422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	id, ok := parseMonitorID(string(req.Id))
	if !ok {
		return apigen.UpdateWebhook404JSONResponse{NotFoundJSONResponse: notFound("webhook not found")}, nil
	}
	existing, err := s.store.GetWebhook(ctx, p.OrgID, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.UpdateWebhook404JSONResponse{NotFoundJSONResponse: notFound("webhook not found")}, nil
		}
		return nil, err
	}
	in := apigen.OutboundWebhookInput(*req.Body)
	url, events, fieldErrs := webhookFromInput(in)
	if len(fieldErrs) > 0 {
		return apigen.UpdateWebhook422JSONResponse{ValidationFailedJSONResponse: fieldValidationFailed(fieldErrs)}, nil
	}
	enabled := existing.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	existing.URL = url
	existing.Enabled = enabled
	existing.Events = events
	if err := s.store.UpdateWebhook(ctx, existing); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.UpdateWebhook404JSONResponse{NotFoundJSONResponse: notFound("webhook not found")}, nil
		}
		return nil, err
	}
	stored, err := s.store.GetWebhook(ctx, p.OrgID, id)
	if err != nil {
		return nil, err
	}
	return apigen.UpdateWebhook200JSONResponse(webhookDTO(stored)), nil
}

// RotateWebhookSecret writes a new signing secret and returns it exactly once. The
// old secret stops verifying immediately, so a receiver must switch to the new one.
// Owner/admin only.
func (s *Server) RotateWebhookSecret(ctx context.Context, req apigen.RotateWebhookSecretRequestObject) (apigen.RotateWebhookSecretResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.RotateWebhookSecret401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageWebhooks, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.RotateWebhookSecret403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	id, ok := parseMonitorID(string(req.Id))
	if !ok {
		return apigen.RotateWebhookSecret404JSONResponse{NotFoundJSONResponse: notFound("webhook not found")}, nil
	}
	secret, err := newWebhookSecret()
	if err != nil {
		return nil, err
	}
	if err := s.store.RotateWebhookSecret(ctx, p.OrgID, id, secret); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.RotateWebhookSecret404JSONResponse{NotFoundJSONResponse: notFound("webhook not found")}, nil
		}
		return nil, err
	}
	stored, err := s.store.GetWebhook(ctx, p.OrgID, id)
	if err != nil {
		return nil, err
	}
	return apigen.RotateWebhookSecret200JSONResponse(apigen.OutboundWebhookCreated{
		Webhook: webhookDTO(stored),
		Secret:  secret,
	}), nil
}

// DeleteWebhook removes a webhook (idempotent: an unknown id is a no-op success).
// Owner/admin only.
func (s *Server) DeleteWebhook(ctx context.Context, req apigen.DeleteWebhookRequestObject) (apigen.DeleteWebhookResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.DeleteWebhook401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageWebhooks, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.DeleteWebhook403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	id, ok := parseMonitorID(string(req.Id))
	if !ok {
		return apigen.DeleteWebhook204Response{}, nil
	}
	if _, err := s.store.DeleteWebhook(ctx, p.OrgID, id); err != nil {
		return nil, err
	}
	return apigen.DeleteWebhook204Response{}, nil
}

// --- validation + mapping ---

// webhookFromInput validates a WebhookInput and returns the normalized url, the
// subscribed event types, and a per-field error map (empty = valid). It does not
// touch the DB. url must be an absolute https URL; each event must be a known type.
func webhookFromInput(in apigen.OutboundWebhookInput) (string, []domain.OrgWebhookEvent, map[string]string) {
	errs := map[string]string{}

	raw := strings.TrimSpace(in.Url)
	if raw == "" {
		errs["url"] = "url is required"
	} else if u, err := url.Parse(raw); err != nil || u.Scheme != "https" || u.Host == "" {
		errs["url"] = "url must be an absolute https URL"
	}

	var events []domain.OrgWebhookEvent
	if in.Events != nil {
		seen := map[domain.OrgWebhookEvent]bool{}
		for i, e := range *in.Events {
			ev := domain.OrgWebhookEvent(e)
			if !knownWebhookEvents[ev] {
				errs["events."+strconv.Itoa(i)] = "unknown event type"
				continue
			}
			if seen[ev] {
				continue // ignore a harmless duplicate
			}
			seen[ev] = true
			events = append(events, ev)
		}
	}
	return raw, events, errs
}

// webhookDTO maps a stored webhook to the API metadata shape. The signing secret is
// never present (it lives encrypted and is shown only once at create/rotate).
func webhookDTO(w *domain.OrgWebhook) apigen.OutboundWebhook {
	var createdBy *string
	if w.CreatedBy != nil {
		s := strconv.FormatInt(*w.CreatedBy, 10)
		createdBy = &s
	}
	events := make([]apigen.OutboundWebhookEvent, 0, len(w.Events))
	for _, e := range w.Events {
		events = append(events, apigen.OutboundWebhookEvent(e))
	}
	var lastStatus *string
	if w.LastStatus != "" {
		ls := w.LastStatus
		lastStatus = &ls
	}
	return apigen.OutboundWebhook{
		Id:             strconv.FormatInt(w.ID, 10),
		Url:            w.URL,
		Enabled:        w.Enabled,
		Events:         events,
		CreatedBy:      createdBy,
		CreatedAt:      w.CreatedAt,
		UpdatedAt:      w.UpdatedAt,
		LastDeliveryAt: w.LastDeliveryAt,
		LastStatus:     lastStatus,
		LastError:      w.LastError,
	}
}

// newWebhookSecret mints a high-entropy signing secret. It is prefixed whsec_ so it
// is recognizable in a receiver's config, base64url over 32 random bytes. Only the
// caller sees it once; the store keeps it encrypted.
func newWebhookSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b), nil
}
