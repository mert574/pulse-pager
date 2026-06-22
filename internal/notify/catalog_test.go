package notify

import (
	"testing"

	"pulse/internal/domain"
)

// findEntry returns the catalog entry for a type, failing the test if absent.
func findEntry(t *testing.T, cat []CatalogEntry, typ domain.ChannelType) CatalogEntry {
	t.Helper()
	for _, e := range cat {
		if e.Type == string(typ) {
			return e
		}
	}
	t.Fatalf("catalog missing type %q", typ)
	return CatalogEntry{}
}

// findField returns a field by key from an entry, failing if absent.
func findField(t *testing.T, e CatalogEntry, key string) CatalogField {
	t.Helper()
	for _, f := range e.ConfigFields {
		if f.Key == key {
			return f
		}
	}
	t.Fatalf("entry %q missing field %q", e.Type, key)
	return CatalogField{}
}

func TestCatalogCoversAllTypes(t *testing.T) {
	cat := Default().Catalog(nil)
	want := []domain.ChannelType{
		domain.ChannelSlack, domain.ChannelDiscord, domain.ChannelWebhook,
		domain.ChannelSMTP, domain.ChannelPagerDuty, domain.ChannelOpsgenie,
		domain.ChannelTelegram, domain.ChannelTeams, domain.ChannelTwilio,
	}
	if len(cat) != len(want) {
		t.Fatalf("catalog has %d entries, want %d", len(cat), len(want))
	}
	for _, typ := range want {
		_ = findEntry(t, cat, typ)
	}
}

func TestCatalogDerivedCodesAndFallback(t *testing.T) {
	cat := Default().Catalog(nil)
	slack := findEntry(t, cat, domain.ChannelSlack)

	// brand name carried through, not localized
	if slack.DisplayName != "Slack" {
		t.Errorf("slack display_name = %q, want Slack", slack.DisplayName)
	}

	f := findField(t, slack, "webhook_url")
	if f.Label.Code != "channel.slack.config.webhook_url.label" {
		t.Errorf("label code = %q", f.Label.Code)
	}
	if f.Label.Message != "Webhook URL" {
		t.Errorf("label message = %q, want English fallback 'Webhook URL'", f.Label.Message)
	}
	if f.Help.Code != "channel.slack.config.webhook_url.help" {
		t.Errorf("help code = %q", f.Help.Code)
	}
	if f.Help.Message == "" {
		t.Error("help message fallback should be present")
	}
}

func TestCatalogCarriesSchemaBits(t *testing.T) {
	cat := Default().Catalog(nil)

	// slack webhook_url is required + secret
	slack := findEntry(t, cat, domain.ChannelSlack)
	wu := findField(t, slack, "webhook_url")
	if !wu.Required || !wu.Secret {
		t.Errorf("slack webhook_url required=%v secret=%v, want both true", wu.Required, wu.Secret)
	}
	if wu.Type != "string" {
		t.Errorf("slack webhook_url type = %q, want string", wu.Type)
	}

	// smtp tls is an enum with a default
	smtp := findEntry(t, cat, domain.ChannelSMTP)
	tls := findField(t, smtp, "tls")
	if tls.Type != "enum" {
		t.Errorf("smtp tls type = %q, want enum", tls.Type)
	}
	wantEnum := []string{"starttls", "implicit", "none"}
	if len(tls.Enum) != len(wantEnum) {
		t.Fatalf("smtp tls enum = %v, want %v", tls.Enum, wantEnum)
	}
	for i := range wantEnum {
		if tls.Enum[i] != wantEnum[i] {
			t.Errorf("smtp tls enum = %v, want %v", tls.Enum, wantEnum)
			break
		}
	}
	if tls.Default != "starttls" {
		t.Errorf("smtp tls default = %q, want starttls", tls.Default)
	}

	// smtp password is secret, host is not
	if !findField(t, smtp, "password").Secret {
		t.Error("smtp password should be secret")
	}
	if findField(t, smtp, "host").Secret {
		t.Error("smtp host should not be secret")
	}
}

func TestCatalogAvailableReflectsAllowedSet(t *testing.T) {
	allowed := map[domain.ChannelType]bool{
		domain.ChannelSlack:   true,
		domain.ChannelWebhook: true,
	}
	cat := Default().Catalog(allowed)

	if !findEntry(t, cat, domain.ChannelSlack).Available {
		t.Error("slack should be available")
	}
	if !findEntry(t, cat, domain.ChannelWebhook).Available {
		t.Error("webhook should be available")
	}
	if findEntry(t, cat, domain.ChannelPagerDuty).Available {
		t.Error("pagerduty should not be available")
	}

	// nil allowed set means nothing is available
	none := Default().Catalog(nil)
	for _, e := range none {
		if e.Available {
			t.Errorf("type %q available with nil allowed set", e.Type)
		}
	}
}

func TestCatalogNoHelpWhenDescriptorHasNone(t *testing.T) {
	r := NewRegistry()
	r.Register(Descriptor{
		Type:        domain.ChannelType("nohelp"),
		DisplayName: "No Help",
		ConfigFields: []ConfigField{
			{Key: "token", Label: "Token", Type: FieldString, Required: true},
		},
		Factory: func() Provider { return stubProvider{} },
	})
	cat := r.Catalog(nil)
	f := findField(t, findEntry(t, cat, domain.ChannelType("nohelp")), "token")
	if f.Help.Code != "" || f.Help.Message != "" {
		t.Errorf("help should be empty when descriptor has no Help, got %+v", f.Help)
	}
}
