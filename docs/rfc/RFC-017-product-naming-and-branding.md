# RFC-017 - Product Naming and Branding

Status: DONE
Author: Principal PM / PO
Audience: frontend engineers (RFC-013), notifier engineers (RFC-007 / PRD-003), API authors (RFC-012), docs-site maintainers, GTM
Parent: `docs/PRD.md` section 1 (vision and positioning, the wedge)
Touches: `docs/PRD.md` appendix B (notification payloads), the SPA (`web/`), the notifier (`internal/notify/`), the OpenAPI spec (`api/openapi/v1.yaml`), the docs site (`docs-site/`), READMEs.

House style: no em-dashes. Plain professional English. Every decision is stated, not left open. Tables for the usage guidelines and the rename checklist.

---

## 1. The name

The product is renamed from **Pulse** to **Pulse Pager**.

Two words, both capitalized: **Pulse Pager**.

### Why rename

"Pulse" on its own is a mood, not a job. It tells a buyer the product has something to do with a heartbeat or activity, but not what it does for them. The category is crowded (UptimeRobot, Pingdom, Better Stack, Datadog Synthetics, plus dozens of "Pulse"-named analytics and survey tools), so a bare "Pulse" is both generic and easy to confuse with unrelated products. It also does not survive a trademark or domain search well.

"Pulse Pager" fixes the clarity problem in one word. It says: this watches your service (pulse) and it pages you when something breaks (pager). That is the whole product loop in the master PRD section 1: register endpoint, check at scale, detect issue, open incident, **notify**, recover. The "pager" half points straight at the moment the product earns its money, the alert. It also lines up with our north-star promise to the user (PRD section 2): "know before your customers do." A pager is how you know first.

We keep "Pulse" as the lead word so we do not throw away the existing recognition and the heartbeat icon idea, and so the wordmark still reads naturally. We add "Pager" as the clarifier rather than replacing the name outright.

### Brand voice

Developer-first, plain, confident, no hype. Short sentences. We talk like an engineer who has been paged at 3am and built the tool they wished they had. We do not say "leverage", "synergy", or "observability platform". We say "we tell you when your API is down, fast, in the channel you already watch."

### Tagline

Primary tagline (use this almost everywhere):

> **Know before your customers do.**

This is lifted from the PRD's own framing of the unifying job (section 2) and it carries the urgency and the benefit in five words. It pairs with the name without repeating it.

Secondary / supporting line (optional, for hero copy on the docs site where a second sentence helps):

> **Uptime monitoring that pages you the moment something breaks.**

PO decision: the primary tagline is the default. The secondary line is allowed only as supporting hero copy, never as a standalone slogan, so we keep one memorable line, not two competing ones.

---

## 2. Usage guidelines (how to display the name)

These are rules, not suggestions. The implementer should be able to copy the exact strings from here.

### 2.1 Name forms

| Form | Exact text | When to use | Notes |
|------|-----------|-------------|-------|
| Full product name | `Pulse Pager` | First mention on any surface, all titles and headings, legal text, footer, the SPA header wordmark, email From name, status-page credit, docs-site brand | This is the default everywhere. When in doubt, use the full name. |
| Short form (running text) | `Pulse Pager` preferred; `Pulse` allowed only in running prose after the full name has already appeared on the same page | Body copy where repeating "Pulse Pager" reads heavy, for example a paragraph that already said "Pulse Pager" once | Never as the first mention, never in a title, never in the footer or legal line. |
| Abbreviation | none | never | PO decision: there is **no** abbreviation. Do not use "PP". It is unprofessional and ambiguous. |
| One-word wordmark variant | none | never | PO decision: there is **no** `PulsePager`, `Pulsepager`, or `pulsepager` display form. It is always two words. (The one-word lowercase form survives only as an internal identifier and a future domain, see section 4, never as display text.) |

Why allow "Pulse" as a short form at all: in long-form prose, repeating "Pulse Pager" every sentence reads like ad copy. After the full name has anchored the page, "Pulse" alone is unambiguous in context and reads cleaner. This is the same pattern most products use (first mention full, later mentions short). It is a writer's convenience, not a second brand, so it is barred from every fixed slot (titles, footer, legal, email From, header wordmark) where the full name must always appear.

### 2.2 Capitalization

| Rule | Correct | Wrong |
|------|---------|-------|
| Both words capitalized in all display | `Pulse Pager` | `pulse pager`, `Pulse pager`, `PULSE PAGER` |
| Never compressed to one word in display | `Pulse Pager` | `PulsePager`, `Pulsepager` |
| Sentence case in surrounding copy still capitalizes the name | "Set up Pulse Pager in two minutes." | "set up pulse pager..." |

All-caps `PULSE PAGER` is not a brand form. It only appears as the unrelated `PULSE_*` env-var prefix, which is an internal identifier and out of scope for display (section 4).

### 2.3 Logo / wordmark

v1 is a **text-based wordmark**, no commissioned logo asset required. This keeps the rename shippable now and avoids blocking on design.

Wordmark spec (what the SPA header renders):

- The wordmark is the two words `Pulse` and `Pager` set in the app's bold heading weight.
- Emphasis: `Pulse` in the default heading color, `Pager` in the brand accent color (the same `--brand-accent` token the status page already uses for its heading). This gives a two-tone wordmark for free, with no image. If a single color is simpler in a given slot, a single-color `Pulse Pager` in bold is an acceptable fallback.
- The two words are separated by a normal space. Do not kern them into one word.
- Icon concept (for a future real asset, documented here so design has the brief, not required for v1): a pulse / heartbeat line that resolves into a pager-or-bell shape on its right end. The heartbeat carries the "Pulse" half, the bell carries the "Pager" half. v1 ships without this icon; the header is text only, or reuses the existing small mark if one is already present.

Exact SPA header wordmark for v1: the bold link in the app shell shows `Pulse Pager` (two words), replacing the current single word `Pulse` in `web/src/components/app-root.ts` and `web/src/components/app-nav.ts`. Two-tone styling is a nice-to-have on top; the load-bearing change is the text.

### 2.4 Browser and page titles

Pattern: `{Page} - Pulse Pager`. Hyphen with spaces, not an em-dash.

| Surface | Title string |
|---------|-------------|
| SPA default / app root `<title>` | `Pulse Pager` |
| SPA per-view title | `Monitors - Pulse Pager`, `Incidents - Pulse Pager`, `Settings - Pulse Pager`, and so on |
| Login view | `Sign in - Pulse Pager` |
| Public status page `<title>` | the customer's own page name (today this is `document.title = data.name`); keep that. The product name does **not** go in the status-page browser title, because that tab belongs to the customer's brand, not ours. See section 2.7. |
| Docs site home `<title>` | `Pulse Pager - developer-first uptime monitoring` |
| Docs site sub-pages | `Pricing - Pulse Pager`, `API reference - Pulse Pager`, `Authentication - Pulse Pager` |

### 2.5 Footer

Exact footer text for the SPA and the docs site:

> `(c) 2026 Pulse Pager. Know before your customers do.`

Rules:
- Use `(c)`, the year, the full name, then the tagline. The tagline in the footer is optional on space-constrained surfaces but recommended.
- No em-dashes anywhere in the footer. Separate the copyright and the tagline with a period and a space, as above, not with a dash.
- The docs-site footer currently reads `Pulse docs ...`; it becomes `Pulse Pager docs ...` (full name), keeping its existing link list.

### 2.6 Email

| Field | Value |
|-------|-------|
| From display name | `Pulse Pager` |
| From address | unchanged (configured `from`, an internal/ops value, see section 4) |
| Subject prefix (alerts) | `[Pulse Pager]` (see section 3 for the full decision and the appendix B impact) |

### 2.7 Status page (how we credit ourselves)

The public status page belongs to the customer's brand. We add a small, quiet credit, not a banner.

- Add a footer line on the public status page: `Powered by Pulse Pager`.
- It renders small and muted (the existing `text-base-content/60` muted style), below the monitors and incidents, not in the header.
- It does **not** go in the page header, the `<h1>`, or the browser `<title>`; those stay the customer's name and accent color exactly as today.
- PO decision: "Powered by Pulse Pager" over "Status by ..." or a logo lockup, because it is the standard, expected, low-friction credit and it reads as attribution rather than co-branding. The customer's brand stays on top.

---

## 3. Alert / notification prefix decision (touches LOCKED appendix B)

This is the one decision that changes the locked appendix B payloads, so it is called out explicitly.

### What exists today

The notifier builds alert text with a `[Pulse]` prefix:

- Email subject: `[Pulse] DOWN: <name>` / `[Pulse] RECOVERED: <name>` (built in `internal/notify/smtp.go`).
- SMS (Twilio): `[Pulse] DOWN: <name> (<url>)` (built in `internal/notify/twilio.go`).
- Test sends: `[Pulse] Test message`.
- Slack and Discord bodies use `*DOWN*` / `**DOWN**` with no product prefix (appendix B), so they are unaffected by the prefix decision.

### The decision

PO decision: the alert prefix becomes **`[Pulse Pager]`**.

Reasoning:

- The alert is the exact moment the "Pager" half of the name pays off. A recipient scanning a phone notification or an inbox should see, at a glance, that this is the thing that pages them. `[Pulse Pager]` reinforces the rename precisely where it matters most.
- The length cost is small. `[Pulse Pager]` is 13 characters versus `[Pulse]` at 7. On an SMS (a 160-character GSM segment) and on an email subject line, six extra characters do not push a real alert into a second segment or truncate the meaningful part. The keyword the recipient actually scans for (`DOWN:` / `RECOVERED:`) sits immediately after the prefix in both, so scan speed is preserved.
- Consistency beats brevity here. Having the email/SMS say `[Pulse]` while every other surface says "Pulse Pager" would look like a half-finished rename and invite "is this the same product?" confusion at the worst possible time.

Rejected alternative: keep `[Pulse]` for brevity. Rejected because the saving is six characters, it does not change segment count for normal alerts, and it leaves the product looking inconsistent on its single most important touchpoint. If we ever find a real-world case where the six characters cause SMS segmenting on long monitor names, that is a separate, measurable problem to revisit, not a reason to ship an inconsistent name now.

### Impact on appendix B (intentional, additive branding update)

This changes the human-readable prefix text in appendix B of `docs/PRD.md`. To be explicit for the implementer:

- The **field structure and keys do not change.** Webhook JSON keys, Slack `text` shape, Discord `content` shape, and the email subject/body format are all unchanged.
- The only change is the human-readable prefix string in the email and SMS subject text: `[Pulse]` becomes `[Pulse Pager]`.
- Slack and Discord payloads in appendix B are **unchanged** (they carry no product prefix).

Exact strings the implementer must use:

| Surface | Old | New |
|---------|-----|-----|
| Email subject, down | `[Pulse] DOWN: <name>` | `[Pulse Pager] DOWN: <name>` |
| Email subject, recovery | `[Pulse] RECOVERED: <name>` | `[Pulse Pager] RECOVERED: <name>` |
| SMS, down | `[Pulse] DOWN: <name> (<url>)` | `[Pulse Pager] DOWN: <name> (<url>)` |
| SMS, recovery | `[Pulse] RECOVERED: <name> (<url>)` | `[Pulse Pager] RECOVERED: <name> (<url>)` |
| Test send (email/SMS) | `[Pulse] Test message...` | `[Pulse Pager] Test message...` |

The appendix B example in `docs/PRD.md` (the email subject line) is updated to match as part of this RFC, and is annotated there as an intentional, additive branding update.

---

## 4. Scope: what changes versus what stays

The load-bearing rule:

> Change **all user-facing display text** to "Pulse Pager". Keep **all internal identifiers** exactly as they are.

### Change (display only)

Everything a human reads in the UI, emails, alerts, page titles, docs, and the OpenAPI human description. Listed file by file in section 5.

### Keep (internal identifiers, do not touch)

| Identifier | Example | Why keep it |
|-----------|---------|-------------|
| Go module path | `module pulse` (go.mod) | Renaming the module rewrites every import path in the repo for zero user-visible benefit and risks breaking the build and any tooling. It is never shown to a user. |
| Env-var prefix | `PULSE_POSTGRES_DSN`, `PULSE_SECRET_KEY`, etc. (`.env.example`, `internal/config`) | These are operator-facing config keys baked into compose files, Helm values, secrets, and CI. Renaming them is an ops migration with real breakage risk and no product value. |
| API key prefix | `pulse_sk_` | This prefix is printed in keys our customers have already created and stored. Changing it would break or confuse every existing key and any client that pattern-matches on it. It is a stable token format, not a brand surface. |
| Kafka topic names | the existing `pulse`-named topics | Topic names are a wire contract between services. Renaming means a coordinated migration with replay/cutover. No user ever sees a topic name. |
| Webhook signature header | `X-Pulse-Signature` | This is an HTTP header integrators verify in code. Renaming it breaks every customer's webhook verification. It is a wire identifier, not display. |
| Go package names | `package notify`, etc. | Internal code organization, never shown to users. |
| Domain and status subdomain | canonical brand domain `pulsepager.com`; app host `app.pulsepager.com`; status pages `{slug}.pulsepager.com`, status subdomain `status.pulsepager.com` | The brand domain is `pulsepager.com`. The actual DNS and TLS and email-deliverability cutover (redirects, cert reissue, OAuth callback URLs, status-page subdomain wiring, SEO) is a **separate infra task**, not this rename. Any `pulse.app` / `pulse.io` strings still in code are placeholders to swap during that cutover, so they stay as code identifiers until then. |

Why this split: the rename's goal is clarity for buyers and users, which lives entirely in display text. Internal identifiers carry no brand meaning to users and carry large coordination and breakage cost. Touching them buys nothing and risks a lot, so they stay. This keeps the rename a low-risk, mostly-copy change that ships fast.

---

## 5. Rename checklist

Grouped by area. Every row is the literal change. "Display" means user-facing text (in scope, change it). Internal identifiers are listed in section 4 and are explicitly **not** in this checklist.

### SPA (`web/`)

| File | What | Change | Type |
|------|------|--------|------|
| `web/index.html` | `<title>Pulse</title>` | `<title>Pulse Pager</title>` | Display |
| `web/src/components/app-root.ts` | header brand link text `Pulse` | `Pulse Pager` (two-tone wordmark optional, section 2.3) | Display |
| `web/src/components/app-nav.ts` | nav brand text `Pulse` | `Pulse Pager` | Display |
| `web/src/components/login-view.ts` | `<h1>Pulse</h1>` | `<h1>Pulse Pager</h1>` | Display |
| `web/src/i18n.ts` (en) | `login.tagline` "Sign in to your Pulse account", `account.providersHint` "Sign in to Pulse with any of these providers.", `apiKeys.emptyHint` "...call the Pulse API..." | replace product name `Pulse` -> `Pulse Pager` in each string | Display |
| `web/src/i18n.ts` (es) | the same three keys in Spanish | replace `Pulse` -> `Pulse Pager` (keep the Spanish grammar, the name is not translated) | Display |
| `web/src/i18n.ts` (de) | the same three keys in German | replace `Pulse` -> `Pulse Pager` (name not translated) | Display |
| SPA per-view `<title>` handling | wherever the app sets the document title | apply the `{Page} - Pulse Pager` pattern (section 2.4) | Display |
| App footer | add/confirm footer | `(c) 2026 Pulse Pager. Know before your customers do.` (section 2.5) | Display |

Note: `pulse.theme`, `pulse.locale`, `pulse.last_org`, `pulse_csrf`, `pulse_sk_*`, and the `pulse.app` subdomain logic in `web/src/status/public-client.ts`, `web/src/components/status-page-url.ts`, `web/src/state/context.ts` are **internal identifiers / storage keys / domain, keep unchanged.** The `sales@pulse.example` mailto and code-comment mentions of "Pulse" are not display copy; comments may be updated opportunistically but are not required.

### Public status page (`web/src/status/`)

| File | What | Change | Type |
|------|------|--------|------|
| `web/status.html` | `<title>Status</title>` (the public status-page HTML shell) | keep `Status` as the pre-load placeholder; the Lit component overrides it with the customer's page name on load (`document.title = data.name`). Do not put the product name here. The `pulse.app` text on line 10 is a code comment, keep it. | Keep (deliberate) |
| `web/src/status/status-page.ts` | no product credit today | add a small muted footer line `Powered by Pulse Pager` below the monitors/incidents (section 2.7) | Display |
| `web/src/status/status-page.ts` | `document.title = data.name` | keep as is; do **not** add the product name to the status-page browser title | Keep (deliberate) |

### Notifier (`internal/notify/`)

| File | What | Change | Type |
|------|------|--------|------|
| `internal/notify/smtp.go` | subject builders `[Pulse] DOWN: %s`, `[Pulse] RECOVERED: %s`, `[Pulse] Test message` | `[Pulse Pager] ...` (section 3) | Display |
| `internal/notify/twilio.go` | SMS text `[Pulse] DOWN/RECOVERED/Test` | `[Pulse Pager] ...` | Display |
| Email From display name | wherever the mailer sets the From display name | `Pulse Pager` (the From address stays the configured internal value) | Display |
| `internal/notify/notify_test.go`, `internal/notify/smtp_test.go` (and any test asserting `[Pulse]`) | test expectations on the old prefix | update assertions to `[Pulse Pager]` so tests match the new strings | Test (follows display) |

Note: `opsgenie.go` / `pagerduty.go` use "Pulse incident id" only in code comments (the alias/dedup_key is the incident id, not a product string); no display change needed.

### OpenAPI (`api/openapi/v1.yaml`)

| What | Change | Type |
|------|--------|------|
| `info.title: Pulse API` | `info.title: Pulse Pager API` | Display |
| `info.description` ("Pulse public REST API (v1)...") | replace the product name `Pulse` -> `Pulse Pager` in the prose; leave path examples, the `pulse_sk_` prefix, and the `X-Pulse-Signature` header name unchanged | Display (prose) / Keep (identifiers) |

The generated `web/src/api/schema.d.ts` is regenerated from the spec; do not hand-edit it. Its `pulse_sk_` mentions are the key prefix and stay.

### Docs site (`docs-site/`)

| File | What | Change | Type |
|------|------|--------|------|
| `docs-site/index.html` | `<title>`, meta description, `.nav-brand` text, body copy mentions of "Pulse" the product | `Pulse Pager` (full name on first mention and brand slots) | Display |
| `docs-site/pricing.html` | `<title>Pricing - Pulse`, meta, `.nav-brand`, body | `Pricing - Pulse Pager`, brand -> `Pulse Pager` | Display |
| `docs-site/api.html` | `<title>`, meta, `.nav-brand` | `API reference - Pulse Pager`, brand -> `Pulse Pager` | Display |
| `docs-site/guides/authentication.html` | `<title>`, meta, `.nav-brand`, body prose "Pulse API" | `Authentication - Pulse Pager`, brand -> `Pulse Pager`; keep `X-Pulse-Signature`, `pulse_sk_`, and `incident.opened`/`incident.closed` event names unchanged | Display (prose) / Keep (identifiers) |
| all docs-site footers | `Pulse docs ...` | `Pulse Pager docs ...` | Display |
| `docs-site/README.md` | product name mentions | `Pulse Pager` | Display |
| `docs-site/openapi.yaml` | this is the published copy of the spec | regenerate / mirror the `api/openapi/v1.yaml` title+description change | Display (prose) |

### READMEs

| File | What | Change | Type |
|------|------|--------|------|
| `README.md` | `# Pulse` heading and product-name mentions in prose | `# Pulse Pager`; update the product name where it reads as the brand. Leave `module pulse`, `PULSE_*`, `pulse_sk_`, `cmd/...`, topic names, and `pulse.app` references unchanged | Display (prose) / Keep (identifiers) |

### Checklist headline

Roughly two dozen display strings across six areas: SPA shell + login + i18n (en/es/de) + per-view titles + footer, the public status-page "Powered by" credit, the notifier email/SMS subject prefix and From name (the one appendix B change), the OpenAPI `info.title`/description, the docs-site pages and footers, and the top-level README. Everything else (`module pulse`, `PULSE_*`, `pulse_sk_`, `X-Pulse-Signature`, Kafka topics, package names, `pulse.app`) stays.

---

## 6. PRD and PLANNING updates made with this RFC

- `docs/PRD.md` appendix B: the email subject example updated from `[Pulse]` to `[Pulse Pager]`, annotated as an intentional additive branding update (structure/keys unchanged).
- `docs/PLANNING.md`: RFC-017 added to the RFC list as DONE, with a short note on the alert-prefix change.
