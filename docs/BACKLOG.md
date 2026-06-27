# Pulse Backlog (captured ideas, not yet planned)

Lightweight list of ideas to turn into PRDs/RFCs later. Capturing only; not designed yet.

## Configurable login methods (enable/disable per method)

Make each authentication method (Google, GitHub, dev-login, future SSO) individually enable/disable-able, so an operator can turn one off without a deploy. Motivation: when a provider has an outage or a problem (e.g. GitHub login is failing), disable that method so users do not click a button that then fails to log them in; the FE should hide or disable the button for any method that is turned off.

Notes to consider when this is planned:
- The available login methods should be discoverable by the FE (e.g. an unauthenticated `GET /auth/methods` returning the enabled set) so the login screen renders only enabled buttons, instead of the FE hard-coding Google/GitHub.
- Scope of the toggle: platform-wide for v1; possibly per-org/enterprise later (an enterprise org may enforce SSO only).
- Should compose with RFC-016 (enterprise SSO) and RFC-003 (the auth providers).
- The toggle source: config/env at first (simplest), or a runtime admin setting later so it flips without a restart.
- Captured 2026-06-22 at the operator's request.

## More check types: SSL-expiry and cron/heartbeat

Add two check types beyond the current HTTP/TCP: SSL/TLS certificate expiry (warn some days before a cert expires) and cron/heartbeat monitoring (a scheduled job pings a URL; alert if the ping is late or missing). Motivation: these are table stakes in the uptime market. Every major competitor ships them (UptimeRobot and Better Stack include SSL even on free tiers; Healthchecks.io and Cronitor are whole products built around heartbeats), and their absence is our clearest competitive gap. See `competitive-pricing-analysis.md`.

Notes to consider when this is planned:
- SSL-expiry is cheap relative to browser/synthetic and should come first; DNS checks are a sensible fast follow.
- Heartbeat is a different shape from active checks: the monitor waits for an inbound ping instead of dialing out, so it needs its own ingest endpoint, an expected-period plus grace window, and "late" vs "missing" states.
- Both should fit the existing checker/alerting/incident pipeline (RFC-005, RFC-006) rather than being bolted on.
- The cloud plan prices were set partly on the promise of closing this gap, so it has real revenue weight, not just parity.
- Captured 2026-06-22 from the competitor pricing analysis.

## Channels documentation (per-integration setup how-tos)

Write detailed documentation for every notification channel type, one setup how-to per integration, and link to it from the channel setup section in the app. Each how-to walks the user through connecting that channel end to end: what config fields the form asks for, where to get each value from the provider, and how to confirm it works with a test send. Motivation: the app now offers nine channel types (slack, discord, webhook, smtp, telegram, pagerduty, opsgenie, teams, twilio) but the only in-app help is the field labels. A user who wants Opsgenie or Twilio has to leave and guess. Good docs cut setup friction and support load, and they are expected for the on-call integrations that higher plans unlock.

Notes to consider when this is planned:
- One page (or one anchored section per type) on the docs site, so the in-app setup form can deep-link to the right place. The form should pass the channel type so the link lands on that integration's section, not the top of the page.
- Each how-to should cover: where to create the credential on the provider side (Slack incoming webhook URL, Discord webhook URL, Telegram bot token plus chat id, PagerDuty integration/routing key, Opsgenie API key, Microsoft Teams webhook, Twilio SID/auth token/from number, SMTP host/port/from/recipients), which fields are secret, and the test-send step to verify it.
- The field list per type already lives in the channel-type catalog (`GetChannelTypes` returns each type's config-field schema). The doc should match that schema so the two cannot drift; consider generating the field tables from the descriptors rather than hand-writing them.
- Keep the writing developer-first and plain, matching the docs-site voice. Screenshots help for the provider-side steps but should be kept light so they do not go stale.
- Pairs with the existing `docs-site/guides/` structure (authentication.html lives there already) and with the channel types now enabled per plan.
- Captured 2026-06-22 at the user's request.

## Platform admin panel (operator console)

A console for the platform operator (us), not org admins, that gives visibility and control across all tenants. Right now whoever runs Pulse Pager flies blind: there is no cross-org view, no system-health view, and operator actions like changing a plan are done by hand in SQL. Motivation: as soon as it is running for real you need to answer "is the system healthy, who is using it, and is anything broken" without ssh + psql, and you need safe in-product operator actions instead of raw database edits.

Notes to consider when this is planned:
- This is a platform-superadmin surface, separate from the per-org RBAC (owner/admin/member/viewer). It needs its own superadmin identity, because it crosses tenant boundaries on purpose, which the normal `WithOrg` RLS scoping forbids.
- It is the single highest-risk surface in the product (it reads across all tenants). So: strong, separate auth (consider its own host like `admin.pulsepager.com` and/or IP allowlist), and every operator action written to the existing `audit.events` so there is a trail.
- Likely contents: org/tenant list with plan, usage (monitors, seats, status pages) and last activity; drill into one org; system health (services up, bus depth / consumer lag, check throughput, error rate, recent incidents across all orgs); operator actions (set or change a plan, suspend or disable an org, expire or resend invites, a read-only support view of an org).
- Plan management belongs here. After the entitlements work, plans are operator-set but only via direct SQL (PRD-006 says operator-set until Stripe). The panel turns that into a UI action.
- Cross-tenant reads need a deliberate, audited superadmin data path (a controlled RLS bypass), not a loosening of the normal org-scoped queries.
- Distinguish from infra observability: per-service Prometheus `/metrics` already exists, and Grafana/SLO dashboards are a separate (unbuilt) infra concern. This panel is tenant and business operations, not raw infra graphs; decide where the overlap sits.
- Captured 2026-06-22 from the operator's request while running the single-node prod (no operator visibility today).

## Per-org link previews / SEO for public status pages

Server-render the head metadata (title, description, Open Graph / Twitter card) for public status pages so a shared `slug.pulsepager.com` link shows the customer's name and status, not the generic SPA shell. Right now the status page is a client-rendered bundle: the static `<title>` is "Status" and there are no per-org meta or OG tags; the real title is set at runtime by JS (`document.title = data.name`). So when someone pastes a status-page link into Slack/WhatsApp or a crawler hits it, the preview and search result are generic, which undercuts the customer's brand and the page's findability.

Notes to consider when this is planned:
- The slug already comes from the subdomain (`resolveSlug` in `web/src/status/public-client.ts` is generic), so the server knows which org is being requested. Resolve it to the public status projection (name, accent, maybe logo) and template the `<head>` before sending the HTML.
- Cleanest home is probably the Go api serving `status.html` with a per-slug templated head (it already owns the public status endpoint), rather than a separate edge worker or a prerender step.
- Keep the customer-brand-first rule (RFC-017 2.7): the title and preview should be the customer's name (for example "Acme status"), with the "Powered by Pulse Pager" credit staying small. Our own marketing card is for the app/landing, not their status page.
- og:image: start with a generic "<org> status" card or omit; per-org generated OG images (dynamic image rendering) are a further step.
- Decide indexing intent: status pages probably should be indexable (so "<org> status" finds them), which pairs with a per-org canonical plus these tags.
- Captured 2026-06-22 (follow-up from the app/docs meta work; static status.html can't do per-org previews).
