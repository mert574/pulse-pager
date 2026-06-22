package devapi

import (
	"context"
	"fmt"

	"pulse/internal/apigen"
	"pulse/internal/domain"
	"pulse/internal/notify"
)

// planRank orders the plan tiers low to high so we can ask "does this plan reach
// the tier a channel type needs". It mirrors PRD-006 section 3 (free < starter <
// team < business).
var planRank = map[apigen.Plan]int{
	apigen.Free:     0,
	apigen.Starter:  1,
	apigen.Team:     2,
	apigen.Business: 3,
}

// channelMinPlan is the lowest plan tier that includes each channel type. It
// mirrors PRD-006 section 3 (the "Channel types" row, line 104): the v1 channels
// (slack, discord, webhook, email) are on every tier; PagerDuty and Opsgenie
// arrive on Team; the remaining phased channels (telegram, teams, sms) are
// Business. This map lives in the api layer on purpose: internal/notify must not
// import entitlements/billing (no cycle), so the tier-to-channel knowledge that
// PRD-006 owns sits here, not in the registry. When RFC-009 codifies the
// entitlement set's channel_types_allowed, this is the one place to swap for it.
var channelMinPlan = map[domain.ChannelType]apigen.Plan{
	domain.ChannelSlack:     apigen.Free,
	domain.ChannelDiscord:   apigen.Free,
	domain.ChannelWebhook:   apigen.Free,
	domain.ChannelSMTP:      apigen.Free,
	domain.ChannelPagerDuty: apigen.Team,
	domain.ChannelOpsgenie:  apigen.Team,
	domain.ChannelTelegram:  apigen.Business,
	domain.ChannelTeams:     apigen.Business,
	domain.ChannelTwilio:    apigen.Business,
}

// allowedChannelTypes returns the set of channel types the given plan includes,
// derived from channelMinPlan: a type is allowed when the org's plan rank is at
// least the type's minimum tier. This is the allowed set the registry's Catalog
// takes; deriving it here keeps the plan knowledge out of internal/notify.
func allowedChannelTypes(plan apigen.Plan) map[domain.ChannelType]bool {
	have := planRank[plan]
	allowed := make(map[domain.ChannelType]bool, len(channelMinPlan))
	for typ, min := range channelMinPlan {
		allowed[typ] = have >= planRank[min]
	}
	return allowed
}

// GetChannelTypes returns the channel-type catalog for the org: every channel
// type, its config-field schema, and whether the org's plan includes it. The
// registry builds the structural catalog (schema + i18n codes), and this layer
// fills the plan/billing bits the registry leaves open: required_plan and, when a
// type is not available, a localized unavailable_reason with the upgrade prompt.
func (s *server) GetChannelTypes(_ context.Context, _ apigen.GetChannelTypesRequestObject) (apigen.GetChannelTypesResponseObject, error) {
	// The dev workspace is on the team plan (see GetMe / GetEntitlements). The real
	// api resolves the org's plan from the entitlement set (RFC-009); the catalog
	// build below is identical once that plan value is in hand.
	plan := apigen.Plan(devOrgPlan)
	allowed := allowedChannelTypes(plan)

	entries := notify.Default().Catalog(allowed)
	out := make([]apigen.ChannelTypeCatalogEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, channelCatalogEntry(e))
	}
	return apigen.GetChannelTypes200JSONResponse(apigen.ChannelTypeCatalog{ChannelTypes: out}), nil
}

// channelCatalogEntry maps one notify.CatalogEntry to the API model and enriches
// it with the plan bits the registry leaves zero: required_plan (the lowest tier
// that includes the type) and, when the type is not available, a localized
// unavailable_reason carrying the required plan and channel type for the upgrade
// prompt.
func channelCatalogEntry(e notify.CatalogEntry) apigen.ChannelTypeCatalogEntry {
	entry := apigen.ChannelTypeCatalogEntry{
		Type:         apigen.ChannelType(e.Type),
		DisplayName:  e.DisplayName,
		Available:    e.Available,
		ConfigFields: make([]apigen.CatalogField, 0, len(e.ConfigFields)),
	}
	for _, f := range e.ConfigFields {
		entry.ConfigFields = append(entry.ConfigFields, catalogField(f))
	}

	min := channelMinPlan[domain.ChannelType(e.Type)]
	reqPlan := min
	entry.RequiredPlan = &reqPlan

	if !e.Available {
		reason := unavailableReason(min, e)
		entry.UnavailableReason = &reason
	}
	return entry
}

// unavailableReason builds the localized "upgrade to use this channel" message
// for a gated channel type. It follows the platform i18n shape (RFC-014): a
// stable code, params for interpolation, and an English fallback message.
func unavailableReason(required apigen.Plan, e notify.CatalogEntry) apigen.LocalizedString {
	params := map[string]any{
		"required_plan": string(required),
		"channel_type":  e.Type,
	}
	return apigen.LocalizedString{
		Code:    "channel.unavailable.plan_upgrade",
		Params:  &params,
		Message: fmt.Sprintf("Upgrade to %s to use %s", titlePlan(required), e.DisplayName),
	}
}

// catalogField maps one notify.CatalogField to the API model, carrying the
// schema bits through and turning each LocalizedString across the gen boundary.
func catalogField(f notify.CatalogField) apigen.CatalogField {
	out := apigen.CatalogField{
		Key:      f.Key,
		Type:     f.Type,
		Required: f.Required,
		Secret:   f.Secret,
		Label:    localized(f.Label),
	}
	if len(f.Enum) > 0 {
		enum := f.Enum
		out.Enum = &enum
	}
	if f.Default != "" {
		def := f.Default
		out.Default = &def
	}
	if f.Help.Code != "" {
		help := localized(f.Help)
		out.Help = &help
	}
	return out
}

// localized maps notify.LocalizedString to the gen apigen.LocalizedString,
// carrying the optional params across the pointer-map boundary.
func localized(l notify.LocalizedString) apigen.LocalizedString {
	out := apigen.LocalizedString{Code: l.Code, Message: l.Message}
	if len(l.Params) > 0 {
		params := l.Params
		out.Params = &params
	}
	return out
}

// titlePlan renders a plan tier for the English fallback message (free ->
// "Free"). Only the message is title-cased; the params keep the lowercase tier
// id the frontend keys on.
func titlePlan(p apigen.Plan) string {
	switch p {
	case apigen.Free:
		return "Free"
	case apigen.Starter:
		return "Starter"
	case apigen.Team:
		return "Team"
	case apigen.Business:
		return "Business"
	default:
		return string(p)
	}
}
