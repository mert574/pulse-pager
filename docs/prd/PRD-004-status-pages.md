# PRD-004: Status Pages

Status: Draft for architecture and go-to-market
Owner: Product (Principal PM)
Parent: `docs/PRD.md` (master v2.1), primarily section 8, with section 12 (availability), section 16 decisions 3 and 7, section 6.7 (multi-region), section 11 (entitlements), and the section 4 RBAC matrix.
Related sub-PRDs: PRD-002 Monitoring Engine (data source), PRD-007 Multi-Region (per-region verdict), PRD-006 Billing (custom domain and page-count gating).

This sub-PRD expands master section 8 into an implementable spec. Where the master locks a decision, this document follows it and does not reopen it. Anything genuinely open is listed in section 13 with a recommended default.

---

## 1. Overview, goals, non-goals

### 1.1 Overview

A status page is a public, shareable web page, scoped to one org, that shows whether the org's selected services are up. It is a presentation layer over the same check and incident data the monitoring engine already produces. There is no separate probing for status pages (master 8 decision, restated): the page reads the monitoring truth, so it is cheap to run and can never disagree with what the alerting machine decided.

Status pages serve two business jobs that the master calls out (master 8, section 14):

- **Adoption driver.** A public page puts Pulse in front of the customer's own customers. Master section 14 tracks "% of orgs with a published status page" as an engagement metric.
- **Conversion lever.** Custom domain is a paid feature (master 8 phased, master 11 plan table), and page count is plan-gated (Free 1, Hobby 3, Professional 10, Custom unlimited). Hitting the page cap or wanting a branded domain is a stated upgrade trigger (master 14 conversion row).

### 1.2 Goals

- Let an org publish a trustworthy public page in minutes, reusing existing monitor data with zero extra configuration of probes.
- Show accurate, public-safe status without ever leaking internal check detail (URLs, headers, body, assertions). This is privacy-critical (master 8, master 13 data-classification "only friendly names exposed on public status pages").
- Stay up during the customer's own incident. The page is what customers point at when things break, so its availability cannot depend on the write paths that may be degrading (master 12 availability note).
- Drive the two business jobs above (adoption, conversion).

### 1.3 Non-goals (v1)

- No separate probing or synthetic checks owned by the status page. It only reads engine data (master 8).
- No full theming or page builder. Branding is name, logo, light/dark, accent only (master 8, master 10 "no theming beyond status-page branding").
- No custom domain in v1 (phased, master 8, master 15 Phase 3).
- No subscriber notifications in v1 (phased, master 8, master 15 Phase 3).
- No scheduled maintenance windows in v1 (phased, master 8, master 15 Phase 3).
- No per-region public breakdown in v1 (recommended hidden, section 7 below and section 13).
- No separate "degraded" monitor health status. Master 16 decision 7 keeps the four monitor statuses (disabled/pending/down/up); "degraded" exists only as a page-level rendering, never as a monitor state.

---

## 2. StatusPage entity (per-org)

Every status page belongs to exactly one org (master 3 isolation invariant). Fields:

| Field | Type | Notes |
|-------|------|-------|
| `id` | string | server-generated |
| `org_id` | string | owning org; cross-org access never possible (master 13) |
| `name` | string | internal label for the page, shown in the editor, not necessarily public |
| `slug` | string | URL path segment, unique within the org, lowercase, URL-safe |
| `display_monitors` | list | ordered list of displayed-monitor entries (section 3) |
| `branding` | object | org name, logo, theme (light/dark), accent color (section 2.2) |
| `state` | enum | `draft` or `published` (section 6) |
| `created_at` / `updated_at` | timestamp | RFC3339 UTC |

### 2.1 URL shape

Per master 16 decision 3 (locked recommended default), the shape is the per-org subdomain with pages as paths:

- Single page served at the org subdomain root: `{org-slug}.pulsepager.com`.
- Additional pages served as paths under it: `{org-slug}.pulsepager.com/{page-slug}`.

The `{org-slug}` comes from the org (master 10 org settings has an org slug). The `{page-slug}` is the StatusPage `slug`. This shape is cleaner and more brandable than a shared `status.pulsepager.com/{org}/{page}` path and sets up custom domains naturally (master 16.3 rationale). The org slug renaming case is handled in section 12.5.

### 2.2 Branding (v1 scope)

- Org name (display string on the page; may differ from org legal name).
- Logo (uploaded image, shown in the page header).
- Theme: light or dark.
- Accent color (single color used for primary UI accents).

No fonts, no custom CSS, no layout control in v1 (master 8 "No full theming").

### 2.3 Page count limit

The number of status pages an org can create is plan-gated and enforced on write at the api, per master 11 entitlement enforcement (Free 1, Hobby 3, Professional 10, Custom unlimited). Creating a page past the plan cap is rejected with the standard per-field error envelope and an upsell (master 11). See PRD-006 for the gating source of truth.

---

## 3. Displayed monitor model

A status page does not display monitors directly; it displays a curated, public-safe projection of selected monitors. Each entry in `display_monitors` references one monitor in the same org and carries a friendly display name.

### 3.1 Displayed-monitor entry

| Field | Type | Notes |
|-------|------|-------|
| `monitor_id` | string | references a monitor in the same org |
| `display_name` | string | required friendly name shown publicly; NOT the raw URL |
| `order` | integer | display order on the page |

The `display_name` is mandatory and is the only label the public sees. The raw monitor `url` is never derived or shown. This is privacy-critical: internal URLs (e.g. `https://internal-api.example.com/health`) must not leak through a status page (master 8, master 13).

### 3.2 Public status mapping

Each displayed monitor renders one of three public states. These are derived from the monitor's internal status (master 6.5: disabled/pending/down/up) and reduced for public display:

| Monitor internal status (master 6.5) | Public displayed status |
|--------------------------------------|-------------------------|
| `up` | Operational |
| `down` | Down |
| `pending` (enabled, no results yet) | Operational (treated as no known problem; see 3.5) |
| `disabled` | Hidden from the page (see 3.5) |

"Degraded" at the displayed-monitor level is not a monitor state in v1 (master 16.7). The page-level banner can still read "Partial outage" (section 4); that is computed from the mix of displayed monitor statuses, not from any single monitor being degraded.

### 3.3 Uptime summary windows

For each displayed monitor the page shows an uptime percentage over three windows: **24h, 7d, 90d** (master 8). Uptime is computed from the same check-result and incident data the engine stores, backed by rollups at scale (master 12 data-retention note: rollups persist for uptime math on status pages). The 90d window is bounded by the org's retention tier; if the plan retains fewer than 90 days of raw results (Free is 7d, master 11), the page shows uptime over the available window and labels it accordingly rather than implying data it does not have (edge case, section 12).

### 3.4 Recent history bar

A recent history bar per displayed monitor shows a compact per-period up/down strip (the same data behind the monitors-list sparkline, master 10 screen 4), so a visitor can see recent incidents at a glance. The bar shows up/down per period only; it never shows latency numbers, failure reasons, or status codes publicly.

### 3.5 Pending and disabled handling

- A `pending` monitor (enabled, zero results) has no known problem, so it renders as Operational. Rationale: showing "Down" or an error for a brand-new monitor that has simply not run yet would misrepresent reality.
- A `disabled` monitor is hidden from the public page entirely. A disabled monitor is not being checked, so claiming any public status for it would be misleading. The editor still lists it so the owner can see it is on the page but currently hidden.

### 3.6 What is and is NOT exposed publicly

This table is the privacy contract. The left column is the only thing a public visitor can ever see for a displayed monitor; everything in the right column is never reachable through the status page.

| Exposed publicly | Never exposed publicly |
|------------------|------------------------|
| Friendly `display_name` | Raw monitor `url` |
| Public status: Operational / Down (and page-level Partial/Major) | HTTP method |
| Uptime % over 24h / 7d / 90d (or available window) | Request headers (including any flagged `secret`) |
| Recent history bar (up/down per period) | Request body / `body_contains` string |
| Public incident: start time and, on recovery, duration (section 5) | Assertions: `expected_status_codes`, `max_latency_ms` |
| Human incident updates posted by owner/admin/member (section 5) | Failure reason, HTTP status code, latency ms, error text |
| Page branding (org name, logo, accent, theme) | `interval_seconds`, `timeout_seconds`, `failure_threshold` |
| Overall page banner | Notification channels, channel config, API keys |
| | Region / per-region detail (recommended hidden, section 7) |
| | Internal monitor id and any other org resource |

The public page is served from a projection that contains only the left-column fields. Internal fields are not present in the public response at all, not merely hidden in the UI.

---

## 4. Overall page banner derivation

The page shows one overall banner derived from the public statuses of its displayed monitors (master 8). Only monitors that are visible on the page count (disabled monitors are excluded per 3.5; pending counts as Operational per 3.5).

Definitions for the rule below, over the set of visible displayed monitors:

- `N` = number of visible displayed monitors.
- `D` = number of those currently Down.

| Condition | Banner | Meaning |
|-----------|--------|---------|
| `D = 0` | All systems operational | Nothing visible is down |
| `0 < D < N` | Partial outage | Some but not all visible services are down |
| `D = N` and `N > 0` | Major outage | Every visible service is down |
| `N = 0` | All systems operational (with empty-state note) | No monitors selected or all hidden; see edge case 12.2 |

Notes:

- "Partial outage" is the page-level rendering of a mixed state. It does not require any monitor to be "degraded"; it comes purely from the count of Down vs total visible (consistent with master 16.7).
- The banner uses the same monitor verdicts the alerting machine produced, so the banner can never contradict an open or closed incident (master 8 "always consistent with the monitoring truth").
- Multi-region does not change this. The monitor's single up/down verdict (already reduced by `down_policy`, master 6.7) is the input; the banner never sees per-region detail (section 7).

---

## 5. Incident display

Incidents shown on a status page are the same incident records the alerting machine opens and closes (master 6.4). The page filters to incidents on its displayed monitors only.

### 5.1 What shows publicly

- **Open incidents** on a displayed monitor appear as a public incident with the affected displayed monitor's friendly name and the incident **start time** (`started_at`, master 6.4: first failing check of the run that opened it).
- **Closed incidents** show start time and **duration** (`ended_at - started_at`, master 6.4 recovery-duration rule). Recently closed incidents remain visible for a window so visitors see that a recent problem is resolved.
- Incidents are shown by friendly display name only. The failure reason, status code, latency, and raw URL are never shown (section 3.6).

### 5.2 Human incident updates (annotations)

Owner, admin, or member can post a short human-readable update on an incident that appears publicly on the page (master 8; master 4 matrix "Acknowledge / annotate incidents" is Y for owner/admin/member, N for viewer). These are the same incident annotations from the authenticated app (master 10 screen 8), surfaced publicly.

- Each public update shows the text and a timestamp (UTC with `UTC` suffix for human display, consistent with master 7 and master appendix B timestamp conventions).
- Updates are author-attributed internally (audit) but the public page shows the org's posture, not the individual author's name, to avoid leaking staff identities. (Recommended default; see section 13 if the org wants named updates.)
- Viewers cannot post updates (master 4 matrix).

### 5.3 Consistency

Because incidents come straight from the engine, an incident that auto-closes on recovery (master 6.4 step 8) flips to "resolved" on the page without any manual action, and the duration shown equals the engine's recorded duration. A manually closed incident (owner/admin only, master 16.8) shows as resolved the same way.

---

## 6. Publish / unpublish

A page has two states: `draft` and `published` (master 8).

- **Draft pages are not publicly reachable.** A request to a draft page's public URL returns the same not-found response as a slug that does not exist, so a draft's existence is not leaked. Draft content is visible only inside the authenticated editor.
- **Publish flow.** An owner/admin/member (master 4 matrix "Create / edit / publish status pages") publishes the page; it becomes reachable at its URL (section 2.1). Publishing a page is an audited action (master 13 audit log lists "status page published").
- **Unpublish** returns the page to `draft` and makes the public URL stop resolving.
- **Public-link preview.** The editor offers a preview of exactly what the public sees, including while the page is still a draft, so the owner can verify display names and branding before publishing (master 10 screen 9 "public-link preview"). The preview renders the same public projection (section 3.6), so it cannot accidentally show internal fields.

---

## 7. Multi-region behavior on the public page

A displayed monitor may be checked from several regions (master 6.7, PRD-007). The public page shows the monitor's single overall verdict, already reduced across regions by the monitor's `down_policy` (master 6.7: `any` / `quorum` / `all`). The page never runs its own aggregation; it reads the verdict the engine produced.

**Per-region public detail: recommended hidden for v1.** The public page shows one status per displayed monitor, not a per-region breakdown.

Reasons:

- Per-region detail exposes Pulse's probe topology and which regions a customer pays for, which is internal and a competitive detail, not something to publish.
- A "region X sees you down" line invites confusion for visitors who do not know which region is closest to them.
- The coverage-degraded state (master 6.7: too few healthy probe regions) is an internal operational signal about Pulse's own fleet, not the customer's uptime, so it must not surface publicly. If coverage is degraded the page keeps showing the last known monitor verdict and never invents a public "down".

The authenticated app still shows per-region status and coverage-degraded state to the org (master 10 screens 5 and 6); only the public page hides it. See PRD-007 for the verdict reduction and probe-fleet health rules, and section 13 for revisiting this if customers ask for per-region public detail.

---

## 8. Performance and resilience

The product requirement: **a status page stays up during the customer's own incident.** It is exactly what customers point at when their service is broken, so its availability cannot depend on the write paths (scheduler, worker, alerting, notifier, billing writes) that may be the thing degrading (master 12 availability note: "Status-page serving should be especially resilient ... read-mostly and can be cached/served independently so it stays up even if write paths degrade").

Product requirements (architecture owns the how):

- **Read-mostly and cacheable.** The public projection (section 3.6) is a read of already-computed status, uptime rollups, and incident records. It must be cacheable so traffic spikes during a high-profile outage do not overload the system.
- **Serves under degraded writes.** If write paths are degraded or down, the page must still serve the last known good status. Stale-but-served beats unavailable for a status page. The page may show data that is slightly behind, but it must not go dark because the alerting or billing write path is unhealthy.
- **Independent serving path.** Status-page serving should be separable from the authenticated app and the write pipeline so an incident in one does not take the other down (master 12).
- The control-plane SLA target of 99.9% (master 12) explicitly includes status-page serving; this sub-PRD treats resilient public serving as a first-class requirement, not a nice-to-have.

---

## 9. Phased features

All three are out of v1 (master 8 phased, master 15 Phase 3). Stated here so the v1 data model and URL shape do not block them.

### 9.1 Custom domain (paid)

- `status.customer.com` via CNAME plus managed TLS (master 8). The per-org subdomain URL shape (section 2.1) sets this up naturally (master 16.3).
- Paid feature and a conversion lever; available on Professional and Custom plans (master 11 plan table "Custom domain status page" Y for Professional/Custom).
- Owner/admin only (master 4 matrix "Configure custom domain for status page" Y for owner/admin, N for member/viewer).
- Entitlement and TLS provisioning details belong to PRD-006 (billing/entitlement) and the architecture team. This PRD only fixes the product behavior and RBAC.

### 9.2 Subscriber notifications

- End users subscribe by email first, then webhook and RSS later, to be notified when a displayed monitor goes down or recovers (master 8).
- Adds a subscriber list (subscriber emails are PII, encrypted at rest, master 13 data classification) and an unsubscribe flow. Every notification must carry a working unsubscribe link.
- Reuses the same incident events the engine already emits; no new probing.

### 9.3 Scheduled maintenance windows

- An owner/admin/member can schedule a maintenance window for selected displayed monitors that shows on the page (master 8, master 15 Phase 3).
- During a scheduled window the page communicates planned maintenance rather than an unplanned outage, so a known maintenance does not read as a "Major outage" to visitors.

---

## 10. RBAC

Reused verbatim from the master section 4 matrix. No new roles or capabilities are introduced.

| Capability | Owner | Admin | Member | Viewer |
|------------|:-----:|:-----:|:------:|:------:|
| View status pages (in app) | Y | Y | Y | Y |
| Create / edit / publish status pages | Y | Y | Y | N |
| Post public incident update (annotate) | Y | Y | Y | N |
| Configure custom domain (phased) | Y | Y | N | N |

The public page itself needs no Pulse account to view; it is public by design once published (master 8). Authentication applies only to the editor and to posting updates.

---

## 11. SEO, branding basics, and accessibility

- **SEO/indexing.** Published pages are public and may be indexed. Draft pages are not reachable, so they cannot be indexed. The page should set a sensible title (org name plus "Status") and description so a search result and link preview read well. Custom-domain pages (phased) inherit the same.
- **Branding basics.** Org name, logo, accent color, and light/dark theme (section 2.2). The page should render the org's logo as the favicon and link-preview image where possible so a shared link is recognizably the customer's brand, not Pulse's.
- **Accessibility.** The public page must meet basic accessibility: status is never conveyed by color alone (the up/down/partial/major state carries a text label and an icon, not just red/green), sufficient contrast in both light and dark themes, the history bar and incidents are reachable and labeled for screen readers, and the page is keyboard-navigable. Color-blind-safe status indication is required because the audience is the customer's customers, not engineers.

---

## 12. User stories, acceptance criteria, edge cases

### 12.1 User stories

- As a solo developer (Dev), I link a public status page from my marketing site so customers can self-serve "is it up" without emailing me (master 2 persona A).
- As a startup team (Team), I publish a branded status page my customers trust, and I post a short human update during an incident so customers know we are on it (master 2 persona B).
- As an SRE, I want the status page to stay up while my service is down, and I want to be sure it never leaks our internal endpoint URLs (master 2 persona C, master 13).
- As an owner, I want to preview a page before publishing and control who can publish.

### 12.2 Acceptance criteria (testable)

1. **Correct status.** Given a displayed monitor whose engine status is `up`, the public page shows it Operational; when the engine opens an incident and the monitor becomes `down` (master 6.4), the public page shows it Down and the banner reflects the new mix (section 4) within the page's refresh/cache window.
2. **Correct uptime.** The 24h / 7d / 90d uptime percentages on the public page match the engine's computed uptime for that monitor over the same windows; on a plan with shorter retention the 90d figure shows the available window and is labeled (3.3).
3. **Hides internals.** The public response for a page contains none of the right-column fields in section 3.6 (no URL, method, headers, body, assertions, failure reason, status code, latency, channels, region detail, internal ids). A test asserts the public payload schema contains only the allowed left-column fields.
4. **Updates on incident.** When an incident opens on a displayed monitor, a public incident entry appears with the correct `started_at`; when it recovers, the entry shows resolved with duration equal to `ended_at - started_at` (master 6.4).
5. **Public updates appear.** An update posted by an owner/admin/member appears on the public page; a viewer cannot post one (master 4 matrix).
6. **Draft is hidden.** A draft page's public URL returns the same not-found response as a nonexistent slug; after publish it resolves; after unpublish it stops resolving.
7. **Banner mapping.** With all visible monitors up the banner is "All systems operational"; with some down it is "Partial outage"; with all down it is "Major outage" (section 4 table).
8. **Resilience.** With the write pipeline degraded, the public page still serves the last known status rather than erroring (section 8).
9. **No per-region leak.** The public page shows one status per monitor and no region names or coverage-degraded text (section 7).

### 12.3 Edge cases

- **Monitor deleted while on a page.** The displayed-monitor entry referencing it must be dropped from the page automatically; the public page must not error or show a dangling "unknown service". The banner recomputes over the remaining visible monitors. The editor shows that an entry was removed because its monitor was deleted.
- **Page with zero monitors.** A published page with no visible monitors shows an empty-state ("No services configured") and the banner reads "All systems operational" rather than implying an outage (section 4, `N = 0`). It is a valid published state, not an error.
- **All monitors on the page are disabled.** Same as zero monitors for public purposes, since disabled monitors are hidden (3.5): empty-state and "All systems operational".
- **Org renamed / slug change.** Changing the org slug changes the page's URL host (`{old-slug}.pulsepager.com` to `{new-slug}.pulsepager.com`). The old URL must not silently 404 in a way that breaks customers' existing links without warning; recommended default is to redirect the old org subdomain to the new one for a grace window and warn the owner in the editor before the slug change takes effect. (Custom-domain pages, phased, are unaffected by org-slug changes since they use the customer's own domain.)
- **Pending monitor on a fresh page.** Renders Operational, not an error (3.5).
- **Plan downgrade below page count.** If a downgrade would leave more pages than the lower plan allows, the owner is prompted to unpublish/remove pages first rather than Pulse silently deleting them (master 11 downgrade behavior). See PRD-006.
- **Retention shorter than 90d.** The 90d uptime window is clamped to the retention tier and labeled (3.3).

---

## 13. Open decisions (with recommended defaults)

1. **URL shape confirmation.** Recommended default: confirm master 16.3, the `{org-slug}.pulsepager.com` per-org subdomain with pages as paths and the single page at root. This sub-PRD is written against it. Trade-off: wildcard TLS and subdomain management (master 16.3); worth it for branding and the custom-domain path. Ship the default unless overridden.

2. **Per-region public detail.** Recommended default: **hidden in v1** (section 7). Show one verdict per monitor, no region breakdown, no coverage-degraded text. Trade-off: a multi-region power user cannot show "down only in eu-west" publicly; acceptable for v1 and revisitable if customers ask. Coverage-degraded must stay internal regardless (master 6.7).

3. **Degraded mapping thresholds (page-level).** Recommended default: the banner is purely count-based (section 4): any visible monitor down with at least one still up is "Partial outage"; all down is "Major outage". No "degraded" displayed-monitor state, consistent with master 16.7. Trade-off: a latency-slow-but-up service cannot show as "degraded" on the page until SLO/percentile alerting lands (master 16.7, Phase 3). If GTM later wants a softer middle banner, the natural hook is a future "degraded" signal from percentile alerting, not a new status-page-only probe.

4. **Named vs org-attributed public incident updates.** Recommended default: public updates show the org's posture, not the staff author's name (5.2), to avoid leaking individual staff identities. Trade-off: some orgs may want named updates for a personal touch; can be made a per-page option later.

---

## 14. Dependencies

- **PRD-002 Monitoring Engine.** Source of all data the page reads: monitor records, the four monitor statuses (master 6.5), check results, uptime rollups, and incidents with `started_at` / `ended_at` and annotations (master 6.4). Status pages add no probing of their own (master 8).
- **PRD-007 Multi-Region.** Source of the single monitor verdict via `down_policy` aggregation and probe-fleet health / coverage-degraded state (master 6.7). The public page consumes the reduced verdict and hides per-region and coverage-degraded detail (section 7).
- **PRD-006 Billing.** Source of truth for the status-page count limit per plan and the custom-domain entitlement (master 11 plan table). Enforcement on write at the api with the standard per-field error and upsell (master 11). Custom domain availability and downgrade-over-cap handling live there.

---

## 15. Mapping to phased delivery (master 15)

- **Phase 2 (GA):** Status pages v1 as specified in sections 1-8, 10-12 (Pulse subdomain, no custom domain), per master 15 Phase 2 "status pages v1 (section 8, Pulse subdomain, no custom domain)". Multi-region verdict consumption (section 7) lands with the same phase since multi-region check execution ships at GA (master 15 Phase 2).
- **Phase 3:** Custom domain (9.1), subscriber notifications (9.2), and scheduled maintenance windows (9.3), per master 15 Phase 3.
