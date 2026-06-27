# RFC-014 - Internationalization and Localization (i18n/l10n)

Status: DONE
Author: Principal Architecture
Audience: every api/service author who returns a user-facing string, the SPA (RFC-013), the notifier (RFC-007), status-page serving (PRD-004), docs/CI authors
Parent: `docs/rfc/RFC-000-architecture-overview.md` (the conventions, section 5 eventing, section 9 observability)
Depends on: RFC-012 (the error envelope `{code,message,fields}` this RFC extends), RFC-001 (the columns this RFC asks for), RFC-003 (user/org context and key auth), RFC-007 (notification bodies that localize), RFC-013 (the SPA does client-side localization)
Product source of truth: PRD-003 (notification bodies + the locked webhook envelope), PRD-004 (public status pages), PRD-001 (user/org preferences)

House style: no em-dashes. Tables and JSON examples over prose. Every load-bearing choice states the decision, the reasoning, and the rejected alternatives.

This RFC is the same designed-in-then-phased stance Pulse takes for multi-region (RFC-000 section 4): the whole contract is in place from day one, v1 ships English only, and adding a locale later is catalog files plus translation, never an API or code change.

---

## 1. Overview, scope, principles

### 1.1 What this RFC fixes

i18n is cross-cutting. Every user-facing string the platform produces (API errors, validation messages, notification bodies, transactional emails, status-page text, channel-catalog labels) is in scope, plus l10n formatting (dates, times, numbers, currency, durations, relative time, timezone). This RFC fixes one API-wide contract for how a localizable string travels on the wire, how a locale is picked per request, where strings are rendered (client vs server), the code namespace and its governance, and the tech choices for the message format and the FE/server libraries.

### 1.2 The machine-string vs human-string split

| String kind | Example | Localized? |
|-------------|---------|------------|
| Machine string (an enum, a key, an id, a code value) | `validation_failed`, `mon_123`, `down_policy: "quorum"`, the generic-webhook JSON envelope | No. Stays a stable English/ASCII token. Clients branch on it. |
| Human string, system-generated | an error `message`, a validation field message, a notification body, an email subject, a status-page banner, a channel field label | Yes. This is what this RFC localizes. |
| Human string, user-entered DATA | a monitor name, an org name, a status-page `display_name`, an incident annotation, a custom email subject the user typed | No. We never translate the customer's own words. We echo them verbatim. |
| Brand name | `Slack`, `Discord`, `PagerDuty`, `GitHub`, `Google` | No. Proper nouns are not translated. They appear verbatim inside a localized sentence via interpolation. |

The line that matters: we localize strings the platform authored, never strings the customer authored, and never a machine token a client keys on. A monitor named "down_policy" stays "down_policy"; the word "monitor" around it localizes.

### 1.3 Principles

| # | Principle |
|---|-----------|
| P1 | Designed in from day one. The localizable shape, negotiation, code namespace, server `en` catalogs, and the data-model fields all ship in v1. |
| P2 | English default. `en` is the source language and the always-present fallback. A missing catalog key falls back to `en`, never to a blank or the raw key. |
| P3 | Locales are added by catalogs only. Adding `de` or `ja` is new catalog files plus translation. No handler, no API field, no schema change. |
| P4 | No hardcoded user-facing strings in handlers or services. Every such string has a code in the namespace and an English source string in a catalog. A CI lint plus a pseudo-locale run catches violations. |
| P5 | The wire is locale-neutral where machines read it. UTC timestamps, machine codes, and the generic-webhook envelope do not localize. Locale formatting happens at the rendering edge. |
| P6 | The API always returns the localizable object. It MAY additionally server-render `message` into the resolved locale when a server catalog exists; the object is never dropped. |

---

## 2. The localizable-string contract (the core API-wide convention)

### 2.1 The shape

Every user-facing string the API returns uses one shape:

```json
{
  "code": "error.validation.interval_below_floor",
  "params": { "floor_seconds": 300, "plan": "starter" },
  "message": "Interval must be at least 300s on the Starter plan."
}
```

| Field | Required | Meaning |
|-------|----------|---------|
| `code` | yes | A stable hierarchical key the client localizes against its own catalog (section 4). The contract; never a free-text branch target. |
| `params` | no | Machine values for interpolation. Present only when the message has variables. Values are machine-shaped (numbers, ids, enum tokens, RFC3339 timestamps), so the client can both interpolate and itself localize an embedded token (a plan name, a channel type). |
| `message` | yes | The English source string, already interpolated, safe to display as-is. The fallback for any client without a catalog for `code`. |

The rule: `code` is the contract, `message` is the fallback, `params` is the data. A client with a catalog renders from `code` + `params` and ignores `message`. A client without one shows `message`. Both always work.

### 2.2 Why params carry machine tokens, not pre-localized text

`params` values are the raw machine form so the client can localize embedded names itself. `"plan": "starter"` (not `"Starter plan"`) lets the SPA render the plan name in its own locale via a nested lookup (`plan.starter.name`). `"channel_type": "slack"` stays the brand token (never translated). A timestamp param is RFC3339 UTC so the client formats it to the user's locale and timezone (section 8). If we pre-localized params into English text, the client could not re-localize them.

### 2.3 ICU MessageFormat as the interpolation standard

Decision: interpolation, pluralization, and select/gender use ICU MessageFormat. The English source string for a code is an ICU pattern; `params` are the ICU arguments.

```
# code: error.validation.interval_below_floor
# en source pattern:
Interval must be at least {floor_seconds, number}s on the {plan} plan.

# code: notify.down.summary
# en source pattern (plural + select):
{monitor} is down in {region_count, plural, one {# region} other {# regions}}.
```

ICU is the cross-stack standard for plurals, gender/select, and nesting; it is supported on both the Go and the JS sides (section 10), so one source pattern compiles in both the server catalog and the FE catalog. Section 10 records the rejected alternatives (Go `text/template`, printf-style).

### 2.4 Concrete examples across surfaces

An error envelope entry (top-level message):

```json
{ "code": "error.entitlement.monitor_limit_reached",
  "params": { "limit": 10, "plan": "team" },
  "message": "You have reached your plan limit of 10 monitors." }
```

A per-field validation message (inside `fields`, section 3):

```json
{ "code": "error.validation.interval_below_floor",
  "params": { "floor_seconds": 300, "plan": "starter" },
  "message": "Interval must be at least 300s on the Starter plan." }
```

The channel-catalog `unavailable_reason` (already a localizable object, section 12):

```json
{ "code": "channel.unavailable.plan_upgrade",
  "params": { "channel_type": "pagerduty", "required_plan": "business" },
  "message": "PagerDuty channels require the Business plan." }
```

A config-field label and help (channel form rendering, deterministic codes, section 4.2):

```json
{ "label":  { "code": "channel.slack.config.webhook_url.label",
              "message": "Slack webhook URL" },
  "help":   { "code": "channel.slack.config.webhook_url.help",
              "message": "Paste the incoming-webhook URL from your Slack app." } }
```

---

## 3. Extend the error envelope (amends RFC-012)

The RFC-012 error envelope `message` and each per-field validation message become localizable. The existing `code` enum on the envelope stays exactly as RFC-012 defines it (`validation_failed`, `entitlement_exceeded`, ...); this change is purely additive to the message-carrying fields. RFC-012 section 2.4 is edited now (section 13 lists it as applied).

### 3.1 Before and after

Before (RFC-012 section 2.4, `fields` is a map of bare strings):

```json
{
  "error": {
    "code": "validation_failed",
    "message": "Human-readable, safe to display.",
    "fields": {
      "interval_seconds": "must be >= the plan floor of 300",
      "regions": "region eu-west is not in your plan"
    }
  }
}
```

After (the top-level `message` carries an i18n `code` + `params`; each `fields` entry becomes a localizable object):

```json
{
  "error": {
    "code": "validation_failed",
    "message": "One or more fields are invalid.",
    "i18n": { "code": "error.validation_failed", "params": {} },
    "fields": {
      "interval_seconds": {
        "code": "error.validation.interval_below_floor",
        "params": { "floor_seconds": 300, "plan": "starter" },
        "message": "Interval must be at least 300s on the Starter plan."
      },
      "regions": {
        "code": "error.entitlement.region_not_in_plan",
        "params": { "region": "eu-west", "plan": "starter" },
        "message": "Region eu-west is not in your plan."
      }
    }
  }
}
```

| Change | Detail |
|--------|--------|
| `error.code` | unchanged. Still the RFC-012 closed enum the client branches on. |
| `error.message` | unchanged shape (an English string), still safe to display. The English render of `error.i18n`. |
| `error.i18n` | new optional object `{code, params?}` that localizes the top-level message. The `code` here is an i18n key (hierarchical), distinct from `error.code` (the machine enum). |
| `fields[name]` | was a bare string; now a localizable object `{code, params?, message}`. `message` keeps the old human string so an old client reading `fields[name].message` still works after a one-line change, and a code-less old client path is migrated to read `.message`. |

The choice to keep `error.code` (the machine enum) separate from `error.i18n.code` (the i18n key) is deliberate: clients already branch on `error.code` per RFC-012, and that must not change. The i18n key is a second, additive axis for rendering, not for branching.

### 3.2 Additive and backward-safe

Adding `error.i18n` and turning `fields[name]` from string to object is the one shape change. It is introduced in the v1 spec from the start, so there is no in-flight migration: the SPA (the largest client, RFC-013) reads `fields[name].message` for display and `fields[name].code` for localization from day one. External clients that only read `error.code` are unaffected.

---

## 4. Code namespace and governance

### 4.1 The namespace

Codes are a stable hierarchical dotted key namespace. The top segment names the surface, then the area, then the specific string.

| Prefix | Surface | Example codes |
|--------|---------|---------------|
| `error.validation.*` | per-field validation | `error.validation.interval_below_floor`, `error.validation.url_shape`, `error.validation.name_too_long` |
| `error.entitlement.*` | entitlement rejections | `error.entitlement.monitor_limit_reached`, `error.entitlement.region_not_in_plan`, `error.entitlement.seat_limit_reached` |
| `error.<top_code>` | the top-level envelope message | `error.validation_failed`, `error.rate_limited`, `error.not_found`, `error.internal` |
| `channel.<type>.config.<key>.label` / `.help` | channel form field labels and help | `channel.slack.config.webhook_url.label`, `channel.smtp.config.password.help` |
| `channel.unavailable.<reason>` | channel-catalog unavailable reason | `channel.unavailable.plan_upgrade`, `channel.unavailable.region` |
| `notify.<event>.<part>` | notification bodies (server-rendered) | `notify.down.subject`, `notify.down.body`, `notify.recovery.subject` |
| `email.<flow>.<part>` | transactional emails | `email.invite.subject`, `email.invite.body`, `email.billing.payment_failed.subject` |
| `statuspage.<area>.<key>` | public status-page text | `statuspage.banner.all_operational`, `statuspage.banner.partial_outage`, `statuspage.banner.major_outage`, `statuspage.uptime.window_clamped` |
| `plan.<id>.name` / `region.<code>.name` | localizable embedded names referenced by `params` | `plan.starter.name`, `region.eu-west.name` |

### 4.2 Deterministic codes (so adding a thing needs no new API code)

Where a code is a deterministic function of existing machine data, no new API-level code is invented when that data grows. The clearest case is channel config fields:

```
channel field label code = channel.<type>.config.<key>.label
channel field help  code = channel.<type>.config.<key>.help
```

Adding a new channel type (say `pagerduty` with a `routing_key` field) needs no new code in the API contract. The API derives `channel.pagerduty.config.routing_key.label` mechanically from the channel type and field key it already knows. What is needed is the catalog entry for that derived code on the FE and (if server-rendered) the server side. This is exactly the "added a channel needs only catalog entries, not API code" property item 4 asks for. The same holds for `plan.<id>.name` and `region.<code>.name`: a new plan or region brings a catalog entry, not a code change.

### 4.3 Governance rules

| Rule | Detail |
|------|--------|
| Stable and additive-only | A code, once shipped, never changes meaning and is never renamed or removed (same discipline as the RFC-012 `code` enum). A reworded English string keeps the same code. A genuinely new string gets a new code. |
| English source lives with the code | The `en` catalog is the source of truth for every code's pattern. It is committed in the repo and reviewed in PRs. A code with no `en` entry fails CI. |
| No hardcoded user-facing strings | Handlers and services never embed a user-facing literal. They reference a code and let the localizable object or the server renderer produce the text. |
| CI lint | A linter flags user-facing string literals returned from handlers/renderers that are not routed through the code path (a small allowlist covers logs and machine tokens). |
| Pseudo-locale check | A pseudo-locale (section 10.4, 11) renders every string with accented/elongated text in non-prod. A screen showing raw English is a hardcoded string; a screen showing the raw key is a missing catalog entry. Both are caught before a real locale is added. |

---

## 5. Locale negotiation and resolution

### 5.1 Precedence

One resolved locale is computed per request and attached to the request context. The first source that yields a supported locale wins:

| # | Source | Notes |
|---|--------|-------|
| 1 | Explicit request override | `?lang=<tag>` query param or the `X-Pulse-Locale: <tag>` header. The deliberate per-call override (Swagger try-it, a preview, a test). |
| 2 | Authenticated user's stored locale | `users.locale` (section 9). The signed-in person's preference. |
| 3 | Org default locale | `organizations.default_locale` (section 9). The tenant default for members who have not set their own. |
| 4 | `Accept-Language` | Standard content negotiation for browsers and non-UI clients that send it. |
| 5 | System default `en` | The always-present fallback. |

A value is "supported" only if a catalog exists for it; an unsupported tag at any level is skipped to the next source, so `?lang=xx` for an unshipped locale falls through to the user/org/`Accept-Language`/`en` chain rather than erroring. In v1 only `en` is supported, so every path resolves to `en`, but the precedence is live so adding a locale needs no negotiation change.

### 5.2 One resolved locale per request

The resolved locale is computed once at the edge (in api) and carried on the request context, the same way `org_id` is (RFC-000 section 7). Every handler, every server-side render, and every error path reads that one value. There is no per-handler re-negotiation.

### 5.3 Non-UI and API-key clients

| Client | How it picks a locale |
|--------|-----------------------|
| API key (no user) | `Accept-Language` if sent, else the org default (`organizations.default_locale`), else `en`. A key has no `users.locale` because it is not a person. |
| Swagger / curl | `Accept-Language` or the explicit `?lang=` / `X-Pulse-Locale` override. |
| The SPA | Sends `X-Pulse-Locale` set from the user's chosen UI locale so server-rendered content (emails it triggers) matches what the user sees. |

---

## 6. Two rendering modes

Both modes coexist. The API always returns the localizable object (section 2). On top of that, the server MAY render `message` into the resolved locale when a server catalog exists for the code.

### 6.1 Client-side rendering (the SPA)

The SPA receives `{code, params, message}` and renders from its own bundles (section 10.3). It does not need the server to localize anything: it looks up `code` in its catalog, formats the ICU pattern with `params` and the user's locale/timezone, and shows the result. It ignores `message` when it has the code. This keeps the API render-free for the SPA path and lets the FE ship a new locale by shipping a bundle, with no server deploy.

### 6.2 Server-side rendering (non-UI consumers and server-generated content)

For consumers that cannot render (a plain API client showing `message`, an email body, a status page served as HTML), the server renders the resolved-locale string from its own catalogs (section 10.2) and puts the result in `message`. When no server catalog exists for the resolved locale, `message` stays English (the always-present `en` source). So:

| Situation | What `message` contains |
|-----------|-------------------------|
| Server catalog has the resolved locale | the rendered resolved-locale string |
| Server catalog lacks the resolved locale (or it is `en`) | the English source string |

The localizable object is never dropped: even when the server renders `message`, `code` and `params` ride along so a smart client can still re-render. The two modes are not either/or; the object plus an optionally-localized `message` serves both a render-capable SPA and a render-incapable API client from one response.

---

## 7. Server-generated content that MUST localize

This is the "API in general" part: content the platform authors and sends, which must localize to the recipient's locale because the recipient never sees the SPA.

| Content | Locale used | Owner |
|---------|-------------|-------|
| Notification bodies (Slack / Discord / email / SMS) | the org/recipient locale (`organizations.default_locale`, or the recipient user's `locale` for a user-targeted send) | RFC-007 (renders via the server catalog) |
| Transactional emails: invitations | the invited locale carried on the invitation (`invitations.locale`, section 9) so a cold invite localizes before the user exists | api (invite send) |
| Transactional emails: alerts, billing | the recipient user's `locale`, else org default | api / notifier |
| Public status pages | per-viewer `Accept-Language`, falling back to the page-config default locale (`status_pages.default_locale`), then `en` | PRD-004 serving path |

### 7.1 The locked generic-webhook envelope stays locale-neutral English

The locked generic-webhook JSON envelope (PRD-003 appendix B, RFC-007 section 2) is machine-consumed and does NOT localize. It stays English/ASCII byte-for-byte. This keeps PRD-003 appendix B and RFC-007 intact: the human-readable channel bodies (the Slack `text`, the Discord `content`, the email subject/body) localize, the machine envelope does not.

| Channel output | Localizes? |
|----------------|------------|
| Slack `text`, Discord `content`, email subject/body (human-read) | Yes, server-rendered to the recipient locale |
| Generic-webhook JSON envelope (appendix B, machine-read) | No. Stays English/ASCII, unchanged |
| The org-level outbound-webhook event body (RFC-007 section 7, machine-read) | No. Stays English/ASCII |

The split mirrors P5: a human reads the chat/email body and gets their language; a machine reads the webhook envelope and gets a stable English contract.

---

## 8. Localization of formatting (l10n)

The "API is UTC, the client formats to locale" rule (RFC-000 section 1, RFC-012 section 2.5) is reaffirmed and extended to server-rendered content.

| Format | Wire / source | Where formatted |
|--------|---------------|-----------------|
| Dates and times | API emits RFC3339 UTC (`2026-06-21T14:00:00Z`). Unchanged. | The SPA formats to the user's locale and the user's timezone (`users.timezone`). Server-rendered content (emails, status pages) formats to the recipient's locale and timezone. |
| Numbers | machine numbers in `params` | formatted per locale at render (ICU `{n, number}` / `Intl.NumberFormat`) |
| Currency (billing, invoices) | machine minor-units + ISO currency code | formatted per locale (ICU `{amount, number, ::currency/USD}` / `Intl.NumberFormat` currency) |
| Durations | machine seconds | formatted per locale (incident duration, retry windows) |
| Relative time | the RFC3339 timestamp | formatted per locale (`Intl.RelativeTimeFormat`), client-side |

Timezone is part of l10n: a localized string with a wrong-zone time is still wrong. `users.timezone` and `organizations.default_timezone` (section 9) carry the zone; the wire stays UTC and the render applies both locale and zone. The server, when it renders an email or a status page, uses the recipient's locale plus timezone, not the server's.

---

## 9. Data model additions (for RFC-001)

These columns are needed so the negotiation chain (section 5) and the recipient-locale renders (section 7) have something to read. They are listed here as the RFC-001 changes to apply; RFC-001 owns the exact DDL. All default to `en` / `UTC` so v1 is correct with no data backfill.

| Table | Column | Type / default | Why |
|-------|--------|----------------|-----|
| `users` | `locale` | `TEXT NOT NULL DEFAULT 'en'` | the signed-in person's preference (precedence 2) |
| `users` | `timezone` | `TEXT NOT NULL DEFAULT 'UTC'` | the person's zone for date/time render (section 8) |
| `organizations` | `default_locale` | `TEXT NOT NULL DEFAULT 'en'` | tenant default (precedence 3); key locale for notifications and API-key clients |
| `organizations` | `default_timezone` | `TEXT NOT NULL DEFAULT 'UTC'` | tenant default zone for server-rendered content |
| `invitations` | `locale` | `TEXT NOT NULL DEFAULT 'en'` | so the invite email localizes before the invited user exists (section 7) |
| `status_pages` | `default_locale` | `TEXT NOT NULL DEFAULT 'en'` | the page-config fallback locale when a viewer sends no usable `Accept-Language` (PRD-004) |

Defaults make the columns inert in v1 (everything is `en` / `UTC`) and live the moment a locale catalog is added, with no migration beyond adding the columns. The locale value is a BCP-47 tag (`en`, `de`, `pt-BR`); the timezone is an IANA name (`UTC`, `Europe/Berlin`).

---

## 10. Tech choices

### 10.1 Message format standard: ICU MessageFormat

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| ICU MessageFormat (chosen) | chosen | The industry standard for translatable strings. Handles plurals (`plural`), gender/choice (`select`), nesting, and named arguments, which printf and Go templates do not express cleanly. Crucially it has mature implementations on both our stacks (Go and JS, sections 10.2/10.3), so one source pattern compiles in the server catalog and the FE catalog with identical semantics. Translators know it; translation tooling (Crowdin, Lokalise, Pontoon) speaks it natively. |
| Go `text/template` | rejected | Server-only. No pluralization or gender model, so a translator cannot express "1 region / 2 regions" without code. The FE could not share the same pattern. Splits the source of truth across stacks. |
| printf-style (`%s`, `%d`) | rejected | Positional args break under translation (languages reorder), no plural/select, no named args. The simplest and the most fragile for real localization. |

### 10.2 Server-side Go i18n library: `github.com/nicksnyder/go-i18n/v2`

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| `go-i18n/v2` (chosen) | chosen | Built around CLDR pluralization and a message catalog loaded from files, which is exactly the catalog-additions-only workflow P3 wants. It supports ICU-style plural rules and named template data, loads per-locale message files at startup, and resolves with an explicit fallback chain ending at `en`. Mature, widely used, no cgo. It fits "render `message` in the resolved locale, fall back to `en`" (section 6.2) directly. |
| `golang.org/x/text` message/catalog | rejected | Lower-level and oriented to compile-time catalogs generated by `gotext`, which couples adding a locale to a codegen + rebuild step. That fights P3 (catalogs added without a code change). Good pluralization, but the workflow is heavier than a file-loaded catalog. |
| A pure ICU binding (cgo ICU) | rejected | Full ICU correctness, but cgo breaks the `CGO_ENABLED=0` static/distroless build (RFC-000 section 3) and adds a heavy native dependency for a need `go-i18n` already meets. Revisit only if we hit an ICU feature `go-i18n` cannot express. |

### 10.3 Frontend i18n library: `@lit/localize`

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| `@lit/localize` (chosen) | chosen | First-party for Lit (the SPA stack, RFC-013 / ADR-0017). It integrates with Lit templates, supports runtime locale switching (no full reload to change language), and uses XLIFF catalogs that fit the standard translation workflow (section 10.4). It pairs with `Intl.*` (`DateTimeFormat`, `NumberFormat`, `RelativeTimeFormat`) for the formatting in section 8, which the SPA already uses for dates (RFC-013 section 9.2). It supersedes the interim flat `t(key)` map RFC-013 section 9.2 describes; that map is the placeholder this RFC replaces. |
| FormatJS / `@formatjs/intl` | rejected | Excellent ICU support and the React-world standard, but not Lit-native, so it needs glue to drive Lit re-renders on locale change. We would carry a heavier runtime to get ICU richness `@lit/localize` plus `Intl.*` already covers for our string set. |
| Hand-rolled `t(key)` map (the current interim) | rejected as the final answer | Fine as the v1 placeholder, but no plurals, no ICU, no runtime switching, no translation-tool format. It is exactly what we replace once real locales come. |

RTL (Arabic, Hebrew) is designed-in and phased for both the SPA and the status pages: `@lit/localize` carries the active locale, from which the SPA sets `dir="rtl"` on the document and relies on logical CSS properties (Tailwind logical utilities) so layout mirrors. No RTL locale ships in v1, but the direction hook and logical-property discipline are in place so an RTL locale is a catalog plus a translation, not a layout rewrite.

### 10.4 Catalog format and workflow

| Aspect | Decision |
|--------|----------|
| FE catalog format | XLIFF (what `@lit/localize` produces and consumes), generated from the source strings in the Lit templates. |
| Server catalog format | ICU-message files loaded by `go-i18n/v2` (one file per locale, e.g. `active.en.toml` / `active.de.toml`), keyed by the same dotted codes (section 4). |
| Source of truth | the `en` catalog on each side. Both sides key off the one code namespace (section 4), so a code means the same string on the FE and the server. |
| Keeping FE and server in sync | the code namespace is the contract both sides implement. A CI check asserts every code the server emits in a localizable object resolves in the FE catalog and the server catalog (or falls back to `en`), so a code cannot exist on one side only. Deterministic codes (section 4.2) are checked by their generation rule, not enumerated. |
| Pseudo-localization | a generated pseudo-locale (`en-XA`: accented, ~30% elongated, bracket-wrapped) is built in non-prod. It surfaces hardcoded strings (they stay plain English) and missing keys (they show the raw code) before any real locale is translated. Active in dev and staging (section 11). |

---

## 11. Scope and phasing

| Item | v1 (ships now) | Later (adding a locale) |
|------|----------------|--------------------------|
| Localizable shape `{code, params?, message}` | yes, every user-facing string | unchanged |
| Error-envelope extension (section 3) | yes | unchanged |
| Negotiation precedence (section 5) | yes, resolves to `en` | unchanged; a new locale just starts winning |
| Code namespace + governance (section 4) | yes | new codes are additive |
| Server `en` catalogs (`go-i18n`) | yes | add `active.<locale>.*` files |
| FE `en` bundle (`@lit/localize`) | yes | add an XLIFF translation, ship a bundle |
| Data-model fields (section 9) | yes, defaulting `en` / `UTC` | values become non-default |
| Pseudo-locale | active in dev/staging | stays, catches regressions |
| Real non-English locale | none | catalog files + translation, no code/API change |
| RTL | direction hook + logical CSS in place | an RTL catalog + translation |

Adding a locale is: translate the `en` catalog into the new XLIFF (FE) and the new `go-i18n` file (server), mark the locale supported in the negotiation allowlist, ship. No handler, no endpoint, no schema, no spec change. This is P1 + P3 made concrete.

---

## 12. The channel catalog already uses the localizable shape

The channel-catalog endpoint (the one that lists channel types, their config fields, and why a type is unavailable on the current plan) already returns localizable objects: each field `label`/`help` and each `unavailable_reason` is a `{code, params?, message}` (section 2.4). Its field-label codes are the deterministic `channel.<type>.config.<key>.label` form (section 4.2), so a new channel type needs only catalog entries. This RFC ratifies that shape; it is not a change to the catalog, it is the convention the catalog was already built against, now named API-wide.

---

## 13. Touchpoints and required edits to other docs

The RFC-012 edit (the error-envelope extension, section 3) is applied now by this change. The rest are listed precisely for a follow-up reconciliation; they are not edited here to avoid concurrent-edit conflicts.

| Doc | Edit needed | Status |
|-----|-------------|--------|
| RFC-012 (API design) | extend the error envelope: top-level `error.i18n {code,params?}`, each `fields[name]` becomes `{code,params?,message}`; add an "Internationalization" subsection pointing at RFC-014 | APPLIED now (section 3, and the edit below) |
| RFC-007 (notifier) / PRD-003 | state that human-readable channel bodies (Slack/Discord/email/SMS) render in the recipient locale via the server catalog; reaffirm the generic-webhook envelope and the org-webhook body stay locale-neutral English (appendix B unchanged) | pending |
| RFC-013 (frontend) | replace the interim flat `t(key)` map (section 9.2) with `@lit/localize`; add the RTL direction hook + logical-CSS discipline; consume `{code,params,message}` and render client-side | pending |
| RFC-003 / PRD-001 | user/org locale + timezone preference surfaces (account settings, org settings) and the locale-negotiation precedence read at the api edge | pending |
| RFC-001 (data model) | add the columns in section 9: `users.locale`, `users.timezone`, `organizations.default_locale`, `organizations.default_timezone`, `invitations.locale`, `status_pages.default_locale` (defaults `en` / `UTC`) | pending |
| PRD-004 (status pages) | status-page text localizes via per-viewer `Accept-Language` then `status_pages.default_locale` then `en`; user-entered display names are never translated | pending |
| Channel catalog endpoint (RFC-012 surface) | already uses the localizable shape (section 12); confirm the spec documents the `{code,params?,message}` field shape | confirm only |

---

## 14. Open questions

| # | Question | Lean |
|---|----------|------|
| Q1 | Should `error.i18n` be required on every envelope, or optional until a code exists? | Optional in v1 (every envelope still carries the English `message`); tighten to required once the code coverage lint is green. |
| Q2 | Per-channel SMS length under translation (some locales expand and push past one SMS segment). | Accept multi-segment for v1; flag for the SMS channel when it ships (PRD-003 phased). |
| Q3 | Which locales to launch first when we go past `en`. | Product call; the architecture is locale-agnostic, so it does not block. |
| Q4 | Whether status-page viewers get an explicit locale switcher vs `Accept-Language` only. | `Accept-Language` + page default in v1; a switcher is an additive PRD-004 follow-up. |
