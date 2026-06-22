package notify

import "pulse/internal/domain"

// LocalizedString is the platform i18n shape (RFC-014): a stable Code the
// frontend looks up in its message catalog, optional Params to interpolate, and
// an English Message the frontend falls back to when it has no translation for
// the code. The catalog ships the English fallback so a fresh frontend renders
// without any message bundle.
type LocalizedString struct {
	Code    string         `json:"code"`
	Params  map[string]any `json:"params,omitempty"`
	Message string         `json:"message"`
}

// CatalogField is one config field in the channel catalog: the schema bits the
// form needs (type, required, secret, enum, default) plus localizable label and
// help. It is the descriptor's ConfigField projected for the API, with the
// label/help turned into LocalizedString so the frontend can translate them.
type CatalogField struct {
	Key      string          `json:"key"`
	Type     string          `json:"type"`
	Required bool            `json:"required"`
	Secret   bool            `json:"secret"`
	Enum     []string        `json:"enum,omitempty"`
	Default  string          `json:"default,omitempty"`
	Label    LocalizedString `json:"label"`
	Help     LocalizedString `json:"help,omitempty"`
}

// CatalogEntry is one channel type in the catalog: its stable Type, its brand
// DisplayName (not localized, it is a product name like "Slack"), whether the
// requesting org's plan includes it, and its config-field schema. RequiredPlan
// and UnavailableReason are left zero here on purpose: they need plan/billing
// knowledge the api layer has and this package must not import (no cycle). The
// api layer fills them in after calling Catalog.
type CatalogEntry struct {
	Type              string           `json:"type"`
	DisplayName       string           `json:"display_name"`
	Available         bool             `json:"available"`
	ConfigFields      []CatalogField   `json:"config_fields"`
	RequiredPlan      string           `json:"required_plan,omitempty"`
	UnavailableReason *LocalizedString `json:"unavailable_reason,omitempty"`
}

// Catalog builds the structural channel catalog from the registry. For each
// registered type (in the stable List order) it projects the descriptor into a
// CatalogEntry: the schema fields carried through, and label/help turned into
// LocalizedString with i18n codes derived deterministically from the descriptor
// so adding a channel type needs no change here. The codes are:
//
//	field label: channel.<type>.config.<key>.label
//	field help:  channel.<type>.config.<key>.help
//
// and the English Message is the descriptor's existing Label/Help string (the
// fallback). Available comes from the passed-in allowed set; a type missing from
// allowed is not in the org's plan. The allowed set is passed in (not derived
// here) so this package stays free of any entitlements or billing import.
func (r *Registry) Catalog(allowed map[domain.ChannelType]bool) []CatalogEntry {
	descriptors := r.List()
	out := make([]CatalogEntry, 0, len(descriptors))
	for _, d := range descriptors {
		typ := string(d.Type)
		fields := make([]CatalogField, 0, len(d.ConfigFields))
		for _, f := range d.ConfigFields {
			cf := CatalogField{
				Key:      f.Key,
				Type:     string(f.Type),
				Required: f.Required,
				Secret:   f.Secret,
				Enum:     f.Enum,
				Default:  f.Default,
				Label: LocalizedString{
					Code:    "channel." + typ + ".config." + f.Key + ".label",
					Message: f.Label,
				},
			}
			if f.Help != "" {
				cf.Help = LocalizedString{
					Code:    "channel." + typ + ".config." + f.Key + ".help",
					Message: f.Help,
				}
			}
			fields = append(fields, cf)
		}
		out = append(out, CatalogEntry{
			Type:         typ,
			DisplayName:  d.DisplayName,
			Available:    allowed[d.Type],
			ConfigFields: fields,
		})
	}
	return out
}
