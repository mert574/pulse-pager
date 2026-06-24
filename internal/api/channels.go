package api

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"pulse/internal/apigen"
	"pulse/internal/authz"
	"pulse/internal/domain"
	"pulse/internal/entitlements"
	"pulse/internal/notify"
)

// This file implements the per-org notification-channel slice (PRD-003, RFC-007a):
// the channel-type catalog plus channel CRUD and a test-send. A channel is a place
// we deliver uptime events to (Slack, Discord, webhook, email); a monitor attaches
// channels by id (monitors.notification_channel_ids). The whole slice is descriptor-
// driven: the notify registry is the one source of which types exist, which config
// fields each has, which fields are secret, and how to validate a config, so adding
// a channel type needs no change here.
//
// The role gate is authz.ActionManageChannels (owner/admin/member; viewer denied),
// matching "Channels: create/edit/delete, send test (member+)" in the RBAC matrix.
// Plan-gating (which types a plan includes) is a per-field 422 on type, mirroring the
// monitor plan gate. Secret config values are encrypted at rest and redacted on read;
// an update that leaves a secret field blank keeps the stored value (so the editor
// does not have to re-enter secrets). Every error is the localizable {code, message}
// envelope (RFC-012 / RFC-014).

// channelTester sends a one-off test message to a single channel. *notify.Manager
// satisfies it; a test can stub it.
type channelTester interface {
	Test(ctx context.Context, ch *domain.Channel) error
}

// GetChannelTypes returns the channel-type catalog for the org: every registered
// type, its config-field schema, and whether the org's plan includes it (RFC-007a,
// PRD-006 3). It drives the channel picker and config forms with no hardcoding.
func (s *Server) GetChannelTypes(ctx context.Context, _ apigen.GetChannelTypesRequestObject) (apigen.GetChannelTypesResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.GetChannelTypes401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageChannels, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.GetChannelTypes403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	allowed := s.allowedChannelTypes(ctx, p.OrgID)
	entries := s.registry.Catalog(allowed)
	out := apigen.ChannelTypeCatalog{ChannelTypes: make([]apigen.ChannelTypeCatalogEntry, 0, len(entries))}
	for _, e := range entries {
		out.ChannelTypes = append(out.ChannelTypes, catalogEntryDTO(e))
	}
	return apigen.GetChannelTypes200JSONResponse(out), nil
}

// ListChannels returns the org's channels (enabled and disabled), secret config
// values redacted. Member+ only.
func (s *Server) ListChannels(ctx context.Context, _ apigen.ListChannelsRequestObject) (apigen.ListChannelsResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.ListChannels401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageChannels, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.ListChannels403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	channels, err := s.store.ListChannels(ctx, p.OrgID, s.registry.SecretKeys)
	if err != nil {
		return nil, err
	}
	out := make([]apigen.Channel, 0, len(channels))
	for _, ch := range channels {
		out = append(out, s.channelDTO(ch))
	}
	return apigen.ListChannels200JSONResponse(out), nil
}

// CreateChannel adds a channel after validating its name, type (a known type the
// plan includes), and type-specific config against the descriptor. Secret config
// values are encrypted at rest by the store. Member+ only.
func (s *Server) CreateChannel(ctx context.Context, req apigen.CreateChannelRequestObject) (apigen.CreateChannelResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.CreateChannel401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageChannels, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.CreateChannel403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	if req.Body == nil {
		return apigen.CreateChannel422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	in := apigen.ChannelInput(*req.Body)
	name, typ, cfg, fieldErrs := s.channelFromInput(ctx, p.OrgID, in)
	if len(fieldErrs) > 0 {
		return apigen.CreateChannel422JSONResponse{ValidationFailedJSONResponse: fieldValidationFailed(fieldErrs)}, nil
	}
	ch := &domain.Channel{
		OrgID:   p.OrgID,
		Name:    name,
		Type:    typ,
		Config:  cfg,
		Enabled: in.Enabled,
	}
	if err := s.store.CreateChannel(ctx, ch, s.registry.SecretKeys); err != nil {
		return nil, err
	}
	return apigen.CreateChannel201JSONResponse(s.channelDTO(ch)), nil
}

// UpdateChannel overwrites a channel's name, type, enabled flag, and config. A blank
// secret field keeps the stored value when the type is unchanged, so the editor need
// not re-enter secrets. The channel must exist in the org (404 otherwise). Member+ only.
func (s *Server) UpdateChannel(ctx context.Context, req apigen.UpdateChannelRequestObject) (apigen.UpdateChannelResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.UpdateChannel401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageChannels, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.UpdateChannel403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	if req.Body == nil {
		return apigen.UpdateChannel422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	id, ok := parseMonitorID(string(req.Id))
	if !ok {
		return apigen.UpdateChannel404JSONResponse{NotFoundJSONResponse: notFound("channel not found")}, nil
	}
	existing, err := s.store.GetChannel(ctx, p.OrgID, id, s.registry.SecretKeys)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.UpdateChannel404JSONResponse{NotFoundJSONResponse: notFound("channel not found")}, nil
		}
		return nil, err
	}
	in := apigen.ChannelInput(*req.Body)
	name, typ, cfg, fieldErrs := s.channelFromInput(ctx, p.OrgID, in)
	if len(fieldErrs) > 0 {
		// Merge kept secrets before re-validating so a blank-but-required secret that
		// the user left untouched does not read as missing.
		if typ == existing.Type {
			cfg = mergeKeptSecrets(cfg, existing.Config, s.registry.SecretKeys(typ))
			if revalErrs := s.validateChannelConfig(typ, cfg); len(revalErrs) == 0 {
				fieldErrs = stripConfigErrors(fieldErrs)
			}
		}
		if len(fieldErrs) > 0 {
			return apigen.UpdateChannel422JSONResponse{ValidationFailedJSONResponse: fieldValidationFailed(fieldErrs)}, nil
		}
	} else if typ == existing.Type {
		cfg = mergeKeptSecrets(cfg, existing.Config, s.registry.SecretKeys(typ))
	}
	existing.Name = name
	existing.Type = typ
	existing.Config = cfg
	existing.Enabled = in.Enabled
	if err := s.store.UpdateChannel(ctx, existing, s.registry.SecretKeys); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.UpdateChannel404JSONResponse{NotFoundJSONResponse: notFound("channel not found")}, nil
		}
		return nil, err
	}
	return apigen.UpdateChannel200JSONResponse(s.channelDTO(existing)), nil
}

// DeleteChannel removes a channel (idempotent: an unknown id is a no-op success). A
// monitor still referencing the deleted id simply skips it on dispatch. Member+ only.
func (s *Server) DeleteChannel(ctx context.Context, req apigen.DeleteChannelRequestObject) (apigen.DeleteChannelResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.DeleteChannel401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageChannels, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.DeleteChannel403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	id, ok := parseMonitorID(string(req.Id))
	if !ok {
		return apigen.DeleteChannel204Response{}, nil
	}
	if _, err := s.store.DeleteChannel(ctx, p.OrgID, id); err != nil {
		return nil, err
	}
	return apigen.DeleteChannel204Response{}, nil
}

// TestChannel sends a one-off test message to the channel so the user can confirm the
// config works before attaching it (RFC-007a). The channel must exist in the org (404
// otherwise). A delivery failure is a 422 with the provider error. Member+ only.
func (s *Server) TestChannel(ctx context.Context, req apigen.TestChannelRequestObject) (apigen.TestChannelResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.TestChannel401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageChannels, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.TestChannel403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	id, ok := parseMonitorID(string(req.Id))
	if !ok {
		return apigen.TestChannel404JSONResponse{NotFoundJSONResponse: notFound("channel not found")}, nil
	}
	ch, err := s.store.GetChannel(ctx, p.OrgID, id, s.registry.SecretKeys)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.TestChannel404JSONResponse{NotFoundJSONResponse: notFound("channel not found")}, nil
		}
		return nil, err
	}
	if err := s.tester.Test(ctx, ch); err != nil {
		return apigen.TestChannel422JSONResponse{ValidationFailedJSONResponse: validationFailed("could not deliver test message: " + err.Error())}, nil
	}
	return apigen.TestChannel204Response{}, nil
}

// --- validation + mapping ---

// channelFromInput validates a ChannelInput and returns the trimmed name, the channel
// type, the config map, and a per-field error map (empty = valid). It checks the name
// is present, the type is a known type the plan includes, and the config passes the
// descriptor schema and the provider's own Validate. It does not touch the DB.
func (s *Server) channelFromInput(ctx context.Context, orgID int64, in apigen.ChannelInput) (string, domain.ChannelType, map[string]any, map[string]string) {
	errs := map[string]string{}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		errs["name"] = "name is required"
	}

	typ := domain.ChannelType(in.Type)
	switch {
	case !in.Type.Valid():
		errs["type"] = "unknown channel type"
	case !s.allowedChannelTypes(ctx, orgID)[typ]:
		errs["type"] = "channel type is not included in your plan"
	}

	cfg := map[string]any{}
	if in.Config != nil {
		cfg = map[string]any(in.Config)
	}
	// Only validate the config when the type is a known one; an unknown type has no
	// descriptor to validate against and already carries a type error.
	if in.Type.Valid() {
		for k, v := range s.validateChannelConfig(typ, cfg) {
			errs[k] = v
		}
		// Team email channel: the "members" config holds member ids, and every id must
		// be an active member of this org. This is the save-time half of the org-scoping
		// guard (the send-time half is the resolver join in the notifier). It runs here,
		// not in the registry, because it needs the DB and the org id. A bad id is a
		// per-field 422 on "members", matching how the descriptor validator reports.
		if typ == domain.ChannelEmail {
			if msg := s.validateEmailMembers(ctx, orgID, cfg); msg != "" {
				errs["members"] = msg
			}
		}
	}
	return name, typ, cfg, errs
}

// validateEmailMembers checks that the Team email channel's selected member ids are
// all active members of the org. It returns a per-field message on failure, "" when
// every id is a valid active member. An empty selection is left to the descriptor
// validator's "required" / the provider's Validate; this only adds the org-membership
// check when there are ids to check.
func (s *Server) validateEmailMembers(ctx context.Context, orgID int64, cfg map[string]any) string {
	ids := parseMemberIDs(cfg["members"])
	if len(ids) == 0 {
		return ""
	}
	ok, err := s.store.AreActiveMembers(ctx, orgID, ids)
	if err != nil {
		return "could not verify members"
	}
	if !ok {
		return "one or more selected members are not active members of this org"
	}
	return ""
}

// parseMemberIDs reads a config value as a list of member user ids, accepting the
// JSON shapes a config map holds: a list of numbers or strings (the API member ids
// are strings, RFC-012). Anything that does not parse to an id is skipped.
func parseMemberIDs(raw any) []int64 {
	if raw == nil {
		return nil
	}
	var out []int64
	add := func(v any) {
		switch t := v.(type) {
		case string:
			if id, err := strconv.ParseInt(t, 10, 64); err == nil {
				out = append(out, id)
			}
		case float64:
			out = append(out, int64(t))
		case int64:
			out = append(out, t)
		case int:
			out = append(out, int64(t))
		}
	}
	switch t := raw.(type) {
	case []any:
		for _, v := range t {
			add(v)
		}
	case []string:
		for _, v := range t {
			add(v)
		}
	default:
		add(raw)
	}
	return out
}

// validateChannelConfig runs the registry's schema + provider validation for a type
// and returns it as a per-field error map keyed on "config" (the registry returns a
// single combined error, not per-field). Empty map = valid.
func (s *Server) validateChannelConfig(typ domain.ChannelType, cfg map[string]any) map[string]string {
	if err := s.registry.ValidateConfig(typ, cfg); err != nil {
		return map[string]string{"config": err.Error()}
	}
	return map[string]string{}
}

// mergeKeptSecrets fills blank/missing secret fields in the incoming config with the
// stored value, so an editor that left a redacted secret untouched keeps it. It only
// applies when the type is unchanged (the secret keys match). A nested secret map
// (custom_headers) that is absent entirely is copied wholesale; per-key merging of a
// partially-edited header map is intentionally not attempted (the form sends the full
// set when it edits headers).
func mergeKeptSecrets(in, existing map[string]any, secretKeys []string) map[string]any {
	for _, key := range secretKeys {
		newVal, present := in[key]
		blank := !present || newVal == nil || newVal == ""
		if !blank {
			continue
		}
		if old, ok := existing[key]; ok && old != nil && old != "" {
			in[key] = old
		}
	}
	return in
}

// stripConfigErrors removes the config error from a field-error map (used after a
// successful re-validation of merged config), leaving any name/type errors.
func stripConfigErrors(errs map[string]string) map[string]string {
	delete(errs, "config")
	return errs
}

// allowedChannelTypes is the set of channel types the org's plan includes. orgPlan
// reads the org's current tier, and the types come from entitlements, so there is no
// hardcoding here.
func (s *Server) allowedChannelTypes(ctx context.Context, orgID int64) map[domain.ChannelType]bool {
	allowed := map[domain.ChannelType]bool{}
	for _, t := range entitlements.ChannelTypesAllowed(s.orgPlan(ctx, orgID)) {
		allowed[domain.ChannelType(t)] = true
	}
	return allowed
}

// channelDTO maps a stored channel to the API shape with secret config values redacted
// (blanked), so a secret is never returned (PRD-002 2.2 / master 13). The config keys
// stay present so the editor knows which fields are set.
func (s *Server) channelDTO(ch *domain.Channel) apigen.Channel {
	return apigen.Channel{
		Id:      strconv.FormatInt(ch.ID, 10),
		OrgId:   strconv.FormatInt(ch.OrgID, 10),
		Name:    ch.Name,
		Type:    apigen.ChannelType(ch.Type),
		Enabled: ch.Enabled,
		Config:  redactSecretConfig(ch.Config, s.registry.SecretKeys(ch.Type)),
	}
}

// redactSecretConfig returns a copy of the config with every secret field's value
// blanked. A nested secret map (custom_headers) has each of its values blanked. Non-
// secret fields are carried through unchanged.
func redactSecretConfig(cfg map[string]any, secretKeys []string) map[string]any {
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	for _, key := range secretKeys {
		v, ok := out[key]
		if !ok || v == nil {
			continue
		}
		switch val := v.(type) {
		case string:
			if val != "" {
				out[key] = ""
			}
		case map[string]any:
			hdrs := make(map[string]any, len(val))
			for hk, hv := range val {
				if s, ok := hv.(string); ok && s != "" {
					hdrs[hk] = ""
				} else {
					hdrs[hk] = hv
				}
			}
			out[key] = hdrs
		}
	}
	return out
}

// catalogEntryDTO maps one notify.CatalogEntry to the API shape. The notify package
// leaves RequiredPlan/UnavailableReason zero (it must not import billing); we fill the
// unavailable reason here for a type the plan does not include. No plan currently adds
// the phased types, so RequiredPlan stays unset (there is no upsell target yet).
func catalogEntryDTO(e notify.CatalogEntry) apigen.ChannelTypeCatalogEntry {
	fields := make([]apigen.CatalogField, 0, len(e.ConfigFields))
	for _, f := range e.ConfigFields {
		fields = append(fields, catalogFieldDTO(f))
	}
	entry := apigen.ChannelTypeCatalogEntry{
		Type:         apigen.ChannelType(e.Type),
		DisplayName:  e.DisplayName,
		Available:    e.Available,
		ConfigFields: fields,
	}
	if !e.Available {
		entry.UnavailableReason = &apigen.LocalizedString{
			Code:    "channel." + e.Type + ".unavailable",
			Message: "This channel type is not available on your plan yet.",
		}
	}
	return entry
}

// catalogFieldDTO maps one notify.CatalogField to the API CatalogField, turning empty
// optionals (default, enum, help) into nil pointers.
func catalogFieldDTO(f notify.CatalogField) apigen.CatalogField {
	cf := apigen.CatalogField{
		Key:      f.Key,
		Type:     f.Type,
		Required: f.Required,
		Secret:   f.Secret,
		Label:    localizedDTO(f.Label),
	}
	if f.Default != "" {
		d := f.Default
		cf.Default = &d
	}
	if len(f.Enum) > 0 {
		e := f.Enum
		cf.Enum = &e
	}
	if f.Help.Code != "" || f.Help.Message != "" {
		h := localizedDTO(f.Help)
		cf.Help = &h
	}
	return cf
}

// localizedDTO maps the notify package's LocalizedString to the API one (RFC-014).
func localizedDTO(l notify.LocalizedString) apigen.LocalizedString {
	out := apigen.LocalizedString{Code: l.Code, Message: l.Message}
	if len(l.Params) > 0 {
		p := l.Params
		out.Params = &p
	}
	return out
}
