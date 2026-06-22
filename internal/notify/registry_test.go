package notify

import (
	"context"
	"testing"

	"pulse/internal/domain"
)

// stubProvider is a no-op provider for registry tests.
type stubProvider struct{}

func (stubProvider) Send(context.Context, map[string]any, Event) error { return nil }
func (stubProvider) Validate(map[string]any) error                     { return nil }

func testDescriptor() Descriptor {
	return Descriptor{
		Type:        domain.ChannelType("stub"),
		DisplayName: "Stub",
		Capability:  "channel.stub",
		ConfigFields: []ConfigField{
			{Key: "token", Type: FieldString, Required: true, Secret: true},
			{Key: "name", Type: FieldString, Required: true},
			{Key: "mode", Type: FieldEnum, Required: true, Enum: []string{"a", "b"}},
			{Key: "port", Type: FieldInt, Required: false},
		},
		Factory: func() Provider { return stubProvider{} },
	}
}

func TestRegistryRegisterGetList(t *testing.T) {
	r := NewRegistry()
	d := testDescriptor()
	r.Register(d)

	got, ok := r.Get(d.Type)
	if !ok {
		t.Fatal("Get returned not found")
	}
	if got.DisplayName != "Stub" {
		t.Errorf("DisplayName = %q", got.DisplayName)
	}
	if _, ok := r.Get(domain.ChannelType("missing")); ok {
		t.Error("Get on missing type should be false")
	}

	list := r.List()
	if len(list) != 1 || list[0].Type != d.Type {
		t.Errorf("List = %v", list)
	}
}

func TestRegistrySecretKeys(t *testing.T) {
	r := NewRegistry()
	r.Register(testDescriptor())
	keys := r.SecretKeys(domain.ChannelType("stub"))
	if len(keys) != 1 || keys[0] != "token" {
		t.Errorf("SecretKeys = %v, want [token]", keys)
	}
	if r.SecretKeys(domain.ChannelType("missing")) != nil {
		t.Error("SecretKeys on missing type should be nil")
	}
}

func TestDefaultRegistrySecretKeys(t *testing.T) {
	r := Default()
	cases := map[domain.ChannelType][]string{
		domain.ChannelSlack:     {"webhook_url"},
		domain.ChannelDiscord:   {"webhook_url"},
		domain.ChannelWebhook:   {"url", "custom_headers"},
		domain.ChannelSMTP:      {"password"},
		domain.ChannelPagerDuty: {"routing_key"},
		domain.ChannelOpsgenie:  {"api_key"},
		domain.ChannelTelegram:  {"bot_token"},
		domain.ChannelTeams:     {"webhook_url"},
		domain.ChannelTwilio:    {"auth_token"},
	}
	for typ, want := range cases {
		got := r.SecretKeys(typ)
		if len(got) != len(want) {
			t.Errorf("%s SecretKeys = %v, want %v", typ, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s SecretKeys = %v, want %v", typ, got, want)
				break
			}
		}
	}
}

func TestRegistryValidateConfig(t *testing.T) {
	r := NewRegistry()
	r.Register(testDescriptor())
	typ := domain.ChannelType("stub")

	// missing required field
	if err := r.ValidateConfig(typ, map[string]any{"name": "x", "mode": "a"}); err == nil {
		t.Error("expected error for missing required token")
	}
	// bad enum
	if err := r.ValidateConfig(typ, map[string]any{"token": "t", "name": "x", "mode": "z"}); err == nil {
		t.Error("expected error for bad enum mode")
	}
	// bad int
	if err := r.ValidateConfig(typ, map[string]any{"token": "t", "name": "x", "mode": "a", "port": "notanint"}); err == nil {
		t.Error("expected error for non-int port")
	}
	// valid
	if err := r.ValidateConfig(typ, map[string]any{"token": "t", "name": "x", "mode": "b", "port": 25}); err != nil {
		t.Errorf("valid config errored: %v", err)
	}
	// unknown type
	if err := r.ValidateConfig(domain.ChannelType("nope"), map[string]any{}); err == nil {
		t.Error("expected error for unknown type")
	}
}

func TestRegistryValidateConfigCallsProvider(t *testing.T) {
	// smtp Validate should fire after schema checks pass (host/from/to present in
	// schema as required, but Validate adds the recipient parse check).
	r := Default()
	cfg := map[string]any{"host": "h", "port": 587, "from": "f@x", "to": []any{"t@x"}}
	if err := r.ValidateConfig(domain.ChannelSMTP, cfg); err != nil {
		t.Errorf("valid smtp config errored: %v", err)
	}
}

func TestAvailableForAndCheckAllowed(t *testing.T) {
	r := Default()
	allowed := map[domain.ChannelType]bool{
		domain.ChannelSlack:   true,
		domain.ChannelWebhook: true,
	}
	avail := r.AvailableFor(allowed)
	if len(avail) != 2 {
		t.Fatalf("AvailableFor = %d descriptors, want 2", len(avail))
	}
	types := map[domain.ChannelType]bool{}
	for _, d := range avail {
		types[d.Type] = true
	}
	if !types[domain.ChannelSlack] || !types[domain.ChannelWebhook] {
		t.Errorf("AvailableFor missing expected types: %v", types)
	}
	if types[domain.ChannelPagerDuty] {
		t.Error("AvailableFor should omit a disallowed type")
	}

	if err := CheckAllowed(domain.ChannelSlack, allowed); err != nil {
		t.Errorf("slack should be allowed: %v", err)
	}
	if err := CheckAllowed(domain.ChannelPagerDuty, allowed); err == nil {
		t.Error("pagerduty should be rejected by CheckAllowed")
	}
}
