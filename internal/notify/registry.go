package notify

import (
	"fmt"
	"sort"
	"strconv"

	"pulse/internal/domain"
)

// Registry holds the channel descriptors keyed by type. It is the one place the
// system looks up what a channel type is, which of its fields are secret, how to
// validate its config, and whether a plan allows it.
type Registry struct {
	byType map[domain.ChannelType]Descriptor
}

// NewRegistry returns an empty registry. Use Register to add descriptors, or
// Default() for one populated with all built-in providers.
func NewRegistry() *Registry {
	return &Registry{byType: map[domain.ChannelType]Descriptor{}}
}

// Register adds a descriptor. A later Register for the same type replaces the
// earlier one, which keeps tests and the default registry simple.
func (r *Registry) Register(d Descriptor) {
	r.byType[d.Type] = d
}

// Get returns the descriptor for a type and whether it exists.
func (r *Registry) Get(t domain.ChannelType) (Descriptor, bool) {
	d, ok := r.byType[t]
	return d, ok
}

// List returns all descriptors sorted by type for a stable order.
func (r *Registry) List() []Descriptor {
	out := make([]Descriptor, 0, len(r.byType))
	for _, d := range r.byType {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// SecretKeys returns the config keys whose values are secret for a type, derived
// from the descriptor's ConfigFields. The store uses this to know which keys to
// encrypt at rest, and the API uses it to know which to redact on read. Returns
// nil for an unknown type.
func (r *Registry) SecretKeys(t domain.ChannelType) []string {
	d, ok := r.byType[t]
	if !ok {
		return nil
	}
	var keys []string
	for _, f := range d.ConfigFields {
		if f.Secret {
			keys = append(keys, f.Key)
		}
	}
	return keys
}

// ValidateConfig runs the schema checks derived from the descriptor (required,
// type, enum) and then the Provider's own semantic Validate. It returns the
// first error found. An unknown type is an error.
func (r *Registry) ValidateConfig(t domain.ChannelType, cfg map[string]any) error {
	d, ok := r.byType[t]
	if !ok {
		return fmt.Errorf("unknown channel type %q", t)
	}
	for _, f := range d.ConfigFields {
		if err := validateField(f, cfg); err != nil {
			return err
		}
	}
	return d.Factory().Validate(cfg)
}

// validateField checks one field against its declared schema.
func validateField(f ConfigField, cfg map[string]any) error {
	raw, present := cfg[f.Key]
	missing := !present || raw == nil || raw == ""
	if missing {
		if f.Required {
			return fmt.Errorf("field %q is required", f.Key)
		}
		return nil
	}

	switch f.Type {
	case FieldInt:
		if !isIntLike(raw) {
			return fmt.Errorf("field %q must be an integer", f.Key)
		}
	case FieldBool:
		switch raw.(type) {
		case bool, string:
		default:
			return fmt.Errorf("field %q must be a bool", f.Key)
		}
	case FieldEnum:
		got := cfgString(cfg, f.Key)
		if !contains(f.Enum, got) {
			return fmt.Errorf("field %q must be one of %v, got %q", f.Key, f.Enum, got)
		}
	case FieldStringList:
		if !isListLike(raw) {
			return fmt.Errorf("field %q must be a list", f.Key)
		}
	case FieldMemberList:
		// A member multi-select is a list of member ids at the schema level; the
		// org-membership check (every id is an active member of the org) is org-scoped
		// and runs in the api layer where the DB is reachable.
		if !isListLike(raw) {
			return fmt.Errorf("field %q must be a list", f.Key)
		}
	case FieldString:
		// any non-empty value is accepted; cfgString coerces on read.
	}
	return nil
}

// AvailableFor returns the descriptors whose type is in the allowed set, sorted.
// The caller derives allowed from the org's plan/entitlement; this is the UI's
// "which channel types can this org add" list. A type missing from allowed is a
// type the plan does not include.
func (r *Registry) AvailableFor(allowed map[domain.ChannelType]bool) []Descriptor {
	var out []Descriptor
	for _, d := range r.List() {
		if allowed[d.Type] {
			out = append(out, d)
		}
	}
	return out
}

// CheckAllowed reports whether a channel type is allowed by the plan's allowed
// set. The caller uses it at channel-create and at send time. A plan downgrade
// just changes the allowed set the caller passes in, so a now-disallowed type is
// blocked on create and skipped on send with no code change here.
func CheckAllowed(t domain.ChannelType, allowed map[domain.ChannelType]bool) error {
	if !allowed[t] {
		return fmt.Errorf("channel type %q is not included in your plan", t)
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func isIntLike(v any) bool {
	switch t := v.(type) {
	case int, int64, float64:
		return true
	case string:
		_, err := strconv.Atoi(t)
		return err == nil
	default:
		return false
	}
}

func isListLike(v any) bool {
	switch v.(type) {
	case []string, []any, map[string]any, map[string]string:
		return true
	default:
		return false
	}
}
