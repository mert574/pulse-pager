package entitlements

import "testing"

// TestChannelTypesByTier locks the channel-type availability the pricing promises:
// bring-your-own SMTP on every tier, the Team email channel (our platform mailer)
// from tier2 up (RFC-019).
func TestChannelTypesByTier(t *testing.T) {
	has := func(plan Plan, typ string) bool {
		for _, got := range ChannelTypesAllowed(plan) {
			if got == typ {
				return true
			}
		}
		return false
	}

	all := []Plan{PlanTier1, PlanTier2, PlanTier3, PlanTierCustom}

	// Discord, bring-your-own SMTP, and Telegram are on every tier (including free).
	for _, typ := range []string{"discord", "smtp", "telegram"} {
		for _, p := range all {
			if !has(p, typ) {
				t.Errorf("%q should be available on %s", typ, p)
			}
		}
	}

	// The Team email channel (platform mailer) and Slack start at tier2.
	for _, typ := range []string{"email", "slack"} {
		if has(PlanTier1, typ) {
			t.Errorf("%q should not be available on tier1", typ)
		}
		for _, p := range []Plan{PlanTier2, PlanTier3, PlanTierCustom} {
			if !has(p, typ) {
				t.Errorf("%q should be available on %s", typ, p)
			}
		}
	}

	// The generic webhook starts at tier3.
	for _, p := range []Plan{PlanTier1, PlanTier2} {
		if has(p, "webhook") {
			t.Errorf("webhook should not be available on %s", p)
		}
	}
	for _, p := range []Plan{PlanTier3, PlanTierCustom} {
		if !has(p, "webhook") {
			t.Errorf("webhook should be available on %s", p)
		}
	}

	// An unknown plan falls back to the free tier, which must not unlock slack/webhook/email.
	for _, typ := range []string{"slack", "webhook", "email"} {
		if has(Plan("bogus"), typ) {
			t.Errorf("an unknown plan must not unlock %q", typ)
		}
	}
}
