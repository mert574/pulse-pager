package notify

import (
	"context"
	"fmt"
	"strconv"

	"pulse/internal/domain"
)

// platformEmailProvider is the "Team email" channel (RFC-007a): it sends alert mail
// through the platform mailer (the same transactional mailer the invite flow uses)
// to a multi-select of the org's active members. The config holds only member user
// ids under "members"; the real addresses are resolved at send time from the org's
// memberships, so it is impossible to email anyone outside the channel's org. This
// is distinct from smtpProvider, which is a bring-your-own SMTP server with
// free-typed recipient addresses (kept unchanged).
//
// The mailer and the member-email resolver are injected by the Manager (mailerAware
// / resolverAware), the same way the http client is injected into clientAware
// providers, so the provider itself stays small and the Manager owns the wiring.
type platformEmailProvider struct {
	mailer   Mailer
	resolver MemberEmailResolver
}

func (p *platformEmailProvider) setMailer(m Mailer)                { p.mailer = m }
func (p *platformEmailProvider) setResolver(r MemberEmailResolver) { p.resolver = r }

// Send resolves the configured member ids to active-member addresses scoped to the
// event's org and sends one plain-text mail per recipient through the platform
// mailer. An id that is not an active member of the org is dropped by the resolver,
// so a tampered config or a since-removed member never gets mail. Zero resolved
// recipients is a no-op success (the members may all have left); the save-time guard
// is what rejects an empty list when the channel is created.
func (p *platformEmailProvider) Send(ctx context.Context, cfg map[string]any, ev Event) error {
	if p.mailer == nil {
		return fmt.Errorf("email: no mailer configured")
	}
	if p.resolver == nil {
		return fmt.Errorf("email: no member resolver configured")
	}
	ids := memberIDs(cfg)
	if len(ids) == 0 {
		return fmt.Errorf("email: no members selected")
	}

	to, err := p.resolver.ActiveMemberEmails(ctx, ev.OrgID, ids)
	if err != nil {
		return fmt.Errorf("email: resolve members: %w", err)
	}
	if len(to) == 0 {
		// Every selected member has left the org or been deactivated since the channel
		// was saved. Nothing to send; not an error, so the Manager does not retry.
		return nil
	}

	subject, body, html := emailContent(ev)
	for _, addr := range to {
		if err := p.mailer.Send(ctx, Mail{To: addr, Subject: subject, Body: body, HTML: html}); err != nil {
			return fmt.Errorf("email: send to %s: %w", addr, err)
		}
	}
	return nil
}

// Validate rejects a config with no selected members. The schema check in the
// registry already confirms "members" is a list; this confirms it is non-empty. The
// org-membership check (each id is an active member) is org-scoped and runs in the
// api layer at save time.
func (p *platformEmailProvider) Validate(cfg map[string]any) error {
	if len(memberIDs(cfg)) == 0 {
		return fmt.Errorf("email: select at least one member")
	}
	return nil
}

// emailContent renders the subject, text, and html for an event, reusing the same
// AlertEmail/TestEmail builders the SMTP channel uses so the Team email and the
// SMTP channel read identically.
func emailContent(ev Event) (subject, text, html string) {
	if ev.Test {
		return TestEmail(ev.ChannelName, "the Team email channel works", ev.OrgID)
	}
	return AlertEmail(ev)
}

// memberIDs reads the "members" config as a list of member user ids. It accepts the
// JSON shapes a config map can hold (a []any of numbers/strings, a []string, or a
// single value), skipping anything that does not parse to an id.
func memberIDs(cfg map[string]any) []int64 {
	raw, ok := cfg["members"]
	if !ok || raw == nil {
		return nil
	}
	var out []int64
	add := func(v any) {
		if id, ok := toMemberID(v); ok {
			out = append(out, id)
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
	case []int64:
		out = append(out, t...)
	default:
		add(raw)
	}
	return out
}

// toMemberID coerces one config value to a member user id. Member ids come over the
// wire as strings (the API ids are strings, RFC-012) but a JSON number decodes to
// float64, so both are accepted.
func toMemberID(v any) (int64, bool) {
	switch t := v.(type) {
	case string:
		id, err := strconv.ParseInt(t, 10, 64)
		return id, err == nil
	case float64:
		return int64(t), true
	case int:
		return int64(t), true
	case int64:
		return t, true
	default:
		return 0, false
	}
}

func init() {
	Register(Descriptor{
		Type:        domain.ChannelEmail,
		DisplayName: "Team email",
		Capability:  "channel.email",
		ConfigFields: []ConfigField{
			{Key: "members", Label: "Members", Type: FieldMemberList, Required: true, Help: "the org members to email; addresses are looked up at send time"},
		},
		Factory: func() Provider { return &platformEmailProvider{} },
	})
}
