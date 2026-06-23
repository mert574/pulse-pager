package devapi

import (
	"encoding/json"
	"testing"

	"pulse/internal/apigen"
	"pulse/internal/domain"
	"pulse/internal/notify"
)

// findCatalogEntry returns the catalog entry for a type from a decoded response.
func findCatalogEntry(t *testing.T, cat apigen.ChannelTypeCatalog, typ apigen.ChannelType) apigen.ChannelTypeCatalogEntry {
	t.Helper()
	for _, e := range cat.ChannelTypes {
		if e.Type == typ {
			return e
		}
	}
	t.Fatalf("catalog missing type %q", typ)
	return apigen.ChannelTypeCatalogEntry{}
}

func TestGetChannelTypesHandlerAvailableAndGated(t *testing.T) {
	srv := testServer(t)
	resp := get(t, srv.URL, "/api/v1/orgs/org_dev/channel-types")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET channel-types = %d, want 200", resp.StatusCode)
	}

	var cat apigen.ChannelTypeCatalog
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// the dev org is on the team plan: slack (free tier) is available with no
	// unavailable_reason.
	slack := findCatalogEntry(t, cat, apigen.Slack)
	if !slack.Available {
		t.Error("slack should be available on the team plan")
	}
	if slack.UnavailableReason != nil {
		t.Errorf("available channel should have no unavailable_reason, got %+v", slack.UnavailableReason)
	}
	if slack.RequiredPlan == nil || *slack.RequiredPlan != apigen.Tier1 {
		t.Errorf("slack required_plan = %v, want free", slack.RequiredPlan)
	}

	// twilio (sms) is Business; the team plan does not include it. It comes back
	// available:false with required_plan business and a localized reason.
	twilio := findCatalogEntry(t, cat, apigen.Twilio)
	if twilio.Available {
		t.Error("twilio should not be available on the team plan")
	}
	if twilio.RequiredPlan == nil || *twilio.RequiredPlan != apigen.TierCustom {
		t.Errorf("twilio required_plan = %v, want business", twilio.RequiredPlan)
	}
	if twilio.UnavailableReason == nil {
		t.Fatal("gated channel should carry an unavailable_reason")
	}
	r := twilio.UnavailableReason
	if r.Code != "channel.unavailable.plan_upgrade" {
		t.Errorf("reason code = %q", r.Code)
	}
	if r.Params == nil {
		t.Fatal("reason should carry params")
	}
	p := *r.Params
	if p["required_plan"] != "tierCustom" {
		t.Errorf("reason params required_plan = %v, want tierCustom", p["required_plan"])
	}
	if p["channel_type"] != "twilio" {
		t.Errorf("reason params channel_type = %v, want twilio", p["channel_type"])
	}
	if r.Message != "Upgrade to Business to use SMS (Twilio)" {
		t.Errorf("reason message = %q", r.Message)
	}
}

// On a plan that does not include PagerDuty (free), the enrichment marks it
// unavailable with the team tier as required_plan and a localized reason. This
// is the gated-channel case from the spec, exercised directly so the plan input
// is explicit rather than tied to the dev org's plan.
func TestPagerDutyGatedWhenPlanTooLow(t *testing.T) {
	allowed := allowedChannelTypes(apigen.Tier1)
	if allowed[domain.ChannelPagerDuty] {
		t.Fatal("pagerduty should not be allowed on the free plan")
	}

	entries := notify.Default().Catalog(allowed)
	var pd notify.CatalogEntry
	for _, e := range entries {
		if e.Type == string(domain.ChannelPagerDuty) {
			pd = e
		}
	}
	got := channelCatalogEntry(pd)

	if got.Available {
		t.Error("pagerduty should be unavailable")
	}
	if got.RequiredPlan == nil || *got.RequiredPlan != apigen.Tier3 {
		t.Errorf("required_plan = %v, want team", got.RequiredPlan)
	}
	if got.UnavailableReason == nil {
		t.Fatal("expected unavailable_reason")
	}
	p := *got.UnavailableReason.Params
	if p["required_plan"] != "tier3" || p["channel_type"] != "pagerduty" {
		t.Errorf("reason params = %v", p)
	}
	if got.UnavailableReason.Message != "Upgrade to Team to use PagerDuty" {
		t.Errorf("reason message = %q", got.UnavailableReason.Message)
	}
}

func TestAllowedChannelTypesByPlan(t *testing.T) {
	cases := []struct {
		plan    apigen.Plan
		typ     domain.ChannelType
		allowed bool
	}{
		{apigen.Tier1, domain.ChannelSlack, true},
		{apigen.Tier1, domain.ChannelPagerDuty, false},
		{apigen.Tier1, domain.ChannelTwilio, false},
		{apigen.Tier2, domain.ChannelWebhook, true},
		{apigen.Tier2, domain.ChannelOpsgenie, false},
		{apigen.Tier3, domain.ChannelPagerDuty, true},
		{apigen.Tier3, domain.ChannelOpsgenie, true},
		{apigen.Tier3, domain.ChannelTeams, false},
		{apigen.TierCustom, domain.ChannelTelegram, true},
		{apigen.TierCustom, domain.ChannelTwilio, true},
	}
	for _, c := range cases {
		got := allowedChannelTypes(c.plan)[c.typ]
		if got != c.allowed {
			t.Errorf("plan %s type %s allowed = %v, want %v", c.plan, c.typ, got, c.allowed)
		}
	}
}
