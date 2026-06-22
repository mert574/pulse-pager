package notify

import (
	"context"

	"pulse/internal/domain"
)

// FieldType is the kind of a config field. The UI and the schema validator both
// read this off the descriptor so there is one source of truth per field.
type FieldType string

const (
	FieldString     FieldType = "string"
	FieldInt        FieldType = "int"
	FieldBool       FieldType = "bool"
	FieldEnum       FieldType = "enum"
	FieldStringList FieldType = "stringlist"
)

// ConfigField describes one field of a channel's config. Everything the system
// needs to know about a field (is it required, is it a secret to encrypt and
// redact, what enum values are allowed, the UI label and help) lives here, so a
// new channel type declares its fields once and the generic code does the rest.
type ConfigField struct {
	Key      string
	Label    string
	Type     FieldType
	Required bool
	// Secret means the value is encrypted at rest and redacted on read. The store
	// reads SecretKeys off the descriptor to know which keys to encrypt, and the
	// API reads the same to know which to redact. For a stringlist whose values are
	// secret (webhook custom_headers), Secret marks the whole field's values.
	Secret  bool
	Enum    []string
	Default string
	Help    string
}

// Descriptor is the static definition of a channel type. Adding a channel type
// means writing a Provider and a Descriptor, then registering it. Nothing else
// in the package hardcodes a per-type branch.
type Descriptor struct {
	Type        domain.ChannelType
	DisplayName string
	// Capability is the entitlement key for plan-gating, e.g. "channel.slack" or
	// "channel.pagerduty". The caller maps the org's plan to a set of allowed
	// channel types; this key is the stable name the billing side keys on.
	Capability   string
	ConfigFields []ConfigField
	Factory      func() Provider
}

// Provider is one channel type's delivery logic. It is a single attempt with no
// retry: the Manager owns retry and backoff. cfg is the decrypted in-memory
// config map (domain.Channel.Config).
type Provider interface {
	// Send delivers one event. It returns nil on success.
	Send(ctx context.Context, cfg map[string]any, ev Event) error
	// Validate runs semantic checks beyond schema presence (the registry already
	// checks required/type/enum from the descriptor before calling this). It may
	// be a no-op for simple types.
	Validate(cfg map[string]any) error
}
