# Pulse - Product Requirements Document (v2.1, Multi-Tenant SaaS)

Status: Draft for architecture and go-to-market
Owner: Product (Principal PM)
Audience: Distributed-systems architecture team and go-to-market team
Supersedes: the v1 single-binary monolith PRD (single-admin scope is OBSOLETE)

Note on reuse: the monitoring mechanics in this document are lifted forward, restated, from the proven v1 PRD. Specifically: check execution and failure reasons and assertion priority (v1 4.2), monitor status values (v1 12.1), per-field validation rules (v1 12.4), the alerting state-machine acceptance table (v1 12.5), and the exact notification payloads (v1 12.7). These are proven and stay consistent. The single-tenant, single-admin, SQLite, env-var-credential assumptions are dropped.

What v2.1 adds over v2.0: multi-region checking is now a first-class, designed-in capability (per-region check attribution, a per-monitor down policy, and probe-fleet health so our own region going down never pages a customer) rather than a phase-3 footnote; entitlement enforcement is stated as a cross-cutting product behavior with concrete tier anchors (Free at a 15-minute interval and 10 monitors, top tier at a 1-minute interval, faster on Custom); and the developer-first documentation surfaces are made explicit (the OpenAPI spec is the single source of truth served as interactive Swagger UI, plus a public docs site and pricing page on GitHub Pages kept in sync by CI).

---

## 1. Vision and positioning

### What Pulse is

Pulse is a commercial, multi-tenant SaaS for uptime and health monitoring. A team signs in with Google or GitHub, creates monitors for their HTTP endpoints, and Pulse checks them on schedule from a horizontally scaled worker fleet, decides healthy or not, opens and closes incidents, and notifies the team through the channels they picked (Slack, Discord, generic webhook, email). Pulse also publishes a public status page so a team can show customers that they are up.

The core loop is unchanged from v1 and is the heart of the product: register endpoint -> scheduled check at scale -> detect issue -> open incident -> notify -> recover. What changed is everything around the loop: it is now hosted by us, owned by organizations, shared by teams with roles, automatable through a public API, and billed.

### Who it is for

Developers and small-to-midsize engineering and ops teams who want reliable uptime monitoring without standing up Prometheus + Alertmanager + Grafana, and without paying enterprise pricing for features they do not use. The buyer ranges from a solo developer on a free plan to an SRE at a scaling startup who needs roles, a status page, and an API.

### How Pulse differs (the wedge)

| Competitor | Their strength | Where they leave a gap | Pulse wedge |
|------------|----------------|------------------------|-------------|
| UptimeRobot | Cheap, simple, huge free tier | Weak team/role model, dated UX, shallow API, limited assertions | Same simplicity, but real org/role model, modern API, richer assertions, status pages included |
| Pingdom | Mature, trusted brand | Expensive, per-check pricing punishes growth, heavy | Transparent seat + monitor pricing, fast modern SPA, developer-first |
| Better Uptime / Better Stack | Good status pages + on-call | On-call/incident.io scope creep, pricing climbs fast, broad surface | Focused on the monitoring loop done well, predictable pricing, clean API |
| Datadog Synthetics | Deep, part of a full platform | Enterprise complexity and cost, overkill for "is my API up", lock-in | Standalone, no platform tax, minutes to value |

The wedge: **a developer-first, API-first uptime product with a real team model and an included status page, priced predictably on seats + monitors rather than per-check.** We win on time-to-value (sign in with Google, first monitor in under two minutes), on a clean public API that competitors treat as an afterthought, and on a pricing model that does not punish adding monitors as you grow.

Explicit non-goals (so we do not chase the whole observability market): we are not building full on-call scheduling and escalation rotations (we integrate with PagerDuty/Opsgenie instead, phased), not building APM/tracing/logs, not building a general dashboards builder. Pulse is uptime and health monitoring, done well, hosted.

---

## 2. Target users and personas

### Persona A: Solo developer ("Dev")

- Runs a few side projects or a small SaaS. One or two people total.
- Jobs to be done: "Tell me within a minute or two when my API or site goes down, in Slack, without me babysitting it." "Give me a public status page I can link from my marketing site." "Let me script monitor creation from CI."
- Plan: Free or lowest paid tier. Highly price-sensitive. Converts when they hit monitor count or check-frequency limits, or want a custom-domain status page.

### Persona B: Startup team ("Team")

- 3 to 25 engineers. One shared workspace. Multiple people need access; not everyone should be able to change billing or delete monitors.
- Jobs to be done: "Give the whole team visibility, but let only leads edit monitors and own billing." "One Slack channel per environment." "A status page customers trust." "Invite a teammate by email and have them productive in minutes."
- Plan: Paid team tier. Cares about seats, roles, retention, and status pages. This is our primary revenue persona.

### Persona C: Ops / SRE at a scaling company ("SRE")

- 25 to 200+ engineers, multiple teams, real change-management discipline.
- Jobs to be done: "Manage hundreds of monitors as code through the API." "Route alerts to PagerDuty, not just Slack." "RBAC that maps to my org, audit logs for who changed what, SSO/SCIM." "Data export for compliance." "Confidence in scheduling accuracy and delivery latency under load."
- Plan: Top paid tier, later Enterprise. Drives the SSO/SCIM, audit-log, API-rate, and multi-region requirements (phased).

Across all three the unifying job is: "Know before my customers do, with the least operational overhead, shared correctly with my team."

---

## 3. Tenancy and account model

This is the core change from v1. Pulse is multi-tenant. The unit of isolation and ownership is the **organization**.

### Entities

- **User**: a person. Identified by a verified email from an OAuth provider (Google or GitHub). A user is global to Pulse (one user row regardless of how many orgs they are in). A user can link both a Google and a GitHub identity to the same account (see section 5).
- **Organization (org)**: the tenant. Owns all resources: monitors, channels, incidents, status pages, API keys, billing. Also called workspace or account in the UI; we standardize on "organization" in the API and "workspace" in some UI copy where it reads better. Every resource belongs to exactly one org. Cross-org access is never possible.
- **Membership**: the link between a user and an org, carrying that user's **role** in that org (section 4). A user can belong to many orgs and switch between them. A membership is created either at signup (personal org) or by accepting an invitation.
- **Seat**: a paid capacity unit on an org's plan. An accepted membership occupies one seat. Pending invitations may or may not hold a seat (decision: pending invitations reserve a seat so a team cannot over-invite past their plan; see section 16). Seat count is a billing meter (section 11).
- **Invitation**: a pending offer to join an org, addressed to an email, with a target role, in state `pending` / `accepted` / `revoked` / `expired`.

### Relationships

- User *..* Organization, via Membership (each membership carries one role).
- Organization 1..* Monitor, Channel, Incident, StatusPage, ApiKey, Invitation.
- Organization 1..1 Plan/subscription and billing customer record.

### Signup creates a personal org

On first sign-in with Google or GitHub, Pulse:

1. Creates the User from the verified provider profile (email, name, avatar).
2. Creates a **personal organization** for them, named from their name or email (e.g. "Dev's workspace"), on the Free plan.
3. Creates a Membership making them **owner** of that org, occupying seat 1.
4. Drops them into onboarding (section 10) inside that org.

Rationale: every user always has at least one org, so there is never a "you have no workspace" dead end, and the path from sign-in to first monitor never blocks on org setup. A user who is later invited to a company org keeps their personal org and switches between them. We rejected "no org until you create one" because it adds friction to activation; we rejected "global personal monitors with no org" because it breaks the single isolation model and complicates billing.

### Inviting members (invitation flow)

1. An owner or admin opens Members, enters an email and a target role, sends the invite.
2. Pulse creates an Invitation in `pending`, reserves a seat (subject to plan limit; blocked with an upsell if no seat is available), and emails the invitee a tokenized accept link. Invitations expire after 7 days.
3. The invitee clicks the link:
   - If they have a Pulse account (matching the invited email or after signing in), they see "Join {Org} as {role}?" and on accept a Membership is created and the invitation moves to `accepted`. The seat is now occupied by the membership.
   - If they have no account, the link routes them through Google/GitHub sign-in first, then the accept step. We require the signed-in verified email to match the invited email (decision in section 16).
4. Owners/admins can revoke a pending invitation (frees the reserved seat) or resend it.

### Switching orgs

The app has an **org switcher** in the top navigation. The active org is part of the session/UI state. Every API call is scoped to one org via the path or an org context (section 9). A user's permissions are always evaluated against the active org's membership role.

---

## 4. Roles and permissions (RBAC)

Four roles, ordered by power: **owner > admin > member > viewer**. Roles are per-org (a user can be owner in their personal org and viewer in a company org). At least one owner must always exist on an org (the last owner cannot be removed or demoted; ownership must be transferred first).

Why four and not more: owner/admin/member/viewer covers the real separation of duties (who pays, who configures, who operates, who only watches) without the cognitive cost of a custom-permission system. We rejected a fully custom permission system for v1 as over-engineering for the target personas; custom roles are a phase-3 enterprise consideration.

### Permission matrix

Legend: Y = allowed, N = not allowed, Own = only resources they created (none in v1; all resource access is role-based, not creator-based), Self = applies to their own membership only.

| Capability | Owner | Admin | Member | Viewer |
|-----------|:-----:|:-----:|:------:|:------:|
| View monitors, incidents, check history, status | Y | Y | Y | Y |
| Create / edit / delete monitors | Y | Y | Y | N |
| Run "check now" | Y | Y | Y | N |
| View notification channels (redacted) | Y | Y | Y | Y |
| Create / edit / delete channels, send test | Y | Y | Y | N |
| Acknowledge / annotate incidents | Y | Y | Y | N |
| Manually close an incident | Y | Y | N | N |
| View status pages | Y | Y | Y | Y |
| Create / edit / publish status pages | Y | Y | Y | N |
| Configure custom domain for status page | Y | Y | N | N |
| Invite members / set invited role | Y | Y | N | N |
| Change a member's role | Y | Y (not to/from owner) | N | N |
| Remove a member | Y | Y (not an owner) | N | N |
| Transfer ownership | Y | N | N | N |
| Leave the org | Y (if not last owner) | Y | Y | Y |
| View billing and usage | Y | Y | N | N |
| Manage billing (plan, payment, invoices) | Y | N | N | N |
| Create / revoke API keys | Y | Y | N | N |
| View API keys list (metadata, not secret) | Y | Y | N | N |
| View audit log | Y | Y | N | N |
| Edit org settings (name, defaults) | Y | Y | N | N |
| Delete the organization | Y | N | N | N |

Design notes baked into the matrix:

- **Members are the operators.** They do the day-to-day monitoring work (monitors, channels, incidents) but cannot touch people, money, or keys. This matches the Team persona's "let engineers configure, keep billing and access with leads."
- **Admins run the org except money.** They manage people and keys and settings but cannot change the plan, payment method, transfer ownership, or delete the org. Only the owner controls billing and existential actions.
- **Viewers are read-only**, including on channels (redacted, so no secret leakage). Good for stakeholders and dashboards.
- **Manual incident close** is restricted to owner/admin because it overrides the alerting machine; members open/edit monitors that drive incidents automatically but should not hand-close a confirmed outage record.
- API keys inherit a role too (section 5); a key cannot exceed the permissions of its role.

---

## 5. Authentication and authorization (product contract)

Crypto and token internals are the architecture team's call. This section is the user-facing and product contract.

### Social login (primary, and the only login in v1)

- **Sign in with Google** and **Sign in with GitHub** via OAuth2/OIDC. There is no email+password login in v1. This removes password storage, reset flows, and a class of breaches, and it fits the developer audience (everyone has Google or GitHub).
- We use the provider's **verified email** as the identity anchor. If two providers return the same verified email, they map to the same Pulse user (account linking, see below).
- First sign-in creates the user and a personal org (section 3).

### Account linking

- A user can link both Google and GitHub to one Pulse account so either button signs them in. Linking happens automatically when the verified emails match, or manually from Account settings ("Connect GitHub"). We never link on an unverified email.
- Trade-off: provider-reported email changes are rare but possible; if a provider returns a new verified email not matching any user, it creates a new user. Account merge is a manual support action in v1 (noted in section 16).

### Sessions (JWT model at the product level)

- After OAuth, Pulse issues a session. The product behavior we commit to:
  - **Sessions persist** across browser restarts (you are not logged out every visit).
  - **Multiple devices** work independently; signing in on a phone does not end the laptop session.
  - **Log out works** and ends the current device's session promptly. "Log out of all devices" is available in Account settings.
  - Token lifetime should *feel* like "stay signed in for the workday/week without surprise logouts," implemented as short-lived access tokens plus a longer refresh, exact durations chosen by architecture. Revocation (logout, role change, removal from org) takes effect quickly, within the access-token refresh window at worst.
- Authorization is always evaluated against the **active org membership** at request time. If a user is removed from an org or demoted, their next refreshed request reflects it.

### API keys (programmatic access)

The product contract for API keys:

- **Per-org.** A key belongs to one org and acts within that org only. No key spans orgs.
- **Role-scoped.** Each key is created with a role (member or admin; owner-equivalent keys are not issued so billing/ownership cannot be automated away). The key can do exactly what that role can do via the API. We rejected per-endpoint scopes for v1 as too granular for the audience; role-scoping is the simple, understandable model. Fine-grained scopes are a phase-3 consideration.
- **Shown once.** The secret is displayed exactly once at creation, then only a prefix is ever shown. Pulse stores only a hash.
- **Revocable.** Any key can be revoked immediately; revoked keys fail all requests.
- **Last-used visible.** The keys list shows name, role, created date, created-by, prefix, and last-used timestamp so stale keys are easy to spot and rotate.
- Keys authenticate the public REST API (section 9). They do not grant UI session access.

---

## 6. Core monitoring product

This section reuses the proven v1 mechanics. Where it says "reused from v1," the behavior is identical to the cited v1 section; only the runtime (distributed) and ownership (per-org) change.

### 6.1 Monitor types and phasing

- **GA: HTTP / HTTPS monitors.** This is the core and covers the large majority of "is my service up."
- **GA: TLS / SSL certificate expiry monitors.** Shipped (warns before a cert expires; `cert_expires_at` in the API).
- **Phased later** (section 15), in roughly this order: TCP port, DNS record, keyword/browser (real-page render + keyword), ICMP ping, cron/heartbeat. The monitor data model carries a `type` field from day one so adding types is additive, never a migration of meaning.

### 6.2 Monitor configuration (reused from v1 4.1, scoped to an org)

Each monitor belongs to an org. Fields (identical semantics to v1):

- `name` (required) - human label.
- `url` (required) - full http(s) URL.
- `method` (required, enum GET/POST/PUT/PATCH/DELETE/HEAD, default GET).
- `headers` (optional list of {key, value, secret?}).
- `body` (optional, only for POST/PUT/PATCH).
- `expected_status_codes` (required, list of explicit codes and/or `2xx/3xx/4xx/5xx` shorthand, default `200`).
- `timeout_seconds` (required, 1..60, default 10).
- `interval_seconds` (required, integer, **minimum 30 hard floor**, must be >= timeout, default 60). Plan tier may raise the effective minimum (section 11).
- `enabled` (required bool, default true).
- `max_latency_ms` (optional positive integer).
- `body_contains` (optional string, body read up to a 64 KB cap to test it).
- `failure_threshold` (required integer >= 1, default 1).
- `notification_channel_ids` (optional list; empty = tracked silently).
- `regions` (required list of region codes the monitor is checked from, default a single plan-default region). The selectable region set and count are plan-gated (section 11) and enforced on write (section 11 enforcement). Each scheduled check runs once per selected region.
- `down_policy` (required enum `any` / `quorum` / `all`, default `quorum`). Decides how many of the selected regions must see the target unhealthy before the monitor is declared down. See 6.7.

Per-field validation rules are reused verbatim from v1 12.4 (server authoritative, UI mirrors), extended by the `regions` and `down_policy` rules in appendix A. See appendix A.

### 6.3 Check execution semantics (reused from v1 4.2)

For each scheduled check a worker makes the configured request with the configured timeout and records the outcome. A check is **healthy** only if ALL configured assertions pass, in this order:

1. Request completed without connection error and without timing out, AND
2. Status code matches `expected_status_codes`, AND
3. If `max_latency_ms` set, measured time is at or under it, AND
4. If `body_contains` set, the response body contains the substring.

Otherwise the check is **unhealthy**, with one primary `failure_reason` recorded in this priority order (reused from v1):

`blocked_target` (request never sent, SSRF block, see section 13) -> `connection_error` -> `timeout` -> `status_mismatch` -> `latency_exceeded` -> `body_assertion_failed`.

Each check result stores: monitor id, org id, **region** (the region the check ran from), timestamp, healthy bool, failure_reason (nullable), http status code (nullable), latency ms (nullable), short truncated error text (nullable). The `region` field is present in the check-job and check-result schema from day one (Phase 0), so every result is attributed to where it ran and cross-region aggregation (6.7) is additive, never a migration. Bodies are read only up to 64 KB and only when `body_contains` is set; full bodies are never stored. A match string that would only appear past the 64 KB cap fails the body assertion (documented in UI help).

**Check now**: a manual trigger produces a normal check result and feeds the alerting machine exactly like a scheduled check, does not shift the scheduled cadence, and is serialized per monitor (one check per monitor at a time; a concurrent "check now" returns the in-flight/just-finished result or 409). In the distributed runtime this per-monitor exclusion is coordinated via Redis (section 6.6).

### 6.4 Alerting state machine (reused from v1 12.5)

State per monitor: a running count of consecutive unhealthy checks and whether an incident is open. `T` = `failure_threshold`. Table uses `T = 3`; `H` healthy, `F` unhealthy.

| Step | Check | Fails before | Action | Fails after | Incident | Status | Notification |
|------|-------|--------------|--------|-------------|----------|--------|--------------|
| 1 | H | 0 | none | 0 | none | up | none |
| 2 | F | 0 | count++ | 1 | none | up | none |
| 3 | H | 1 | reset (blip absorbed) | 0 | none | up | none |
| 4 | F | 0 | count++ | 1 | none | up | none |
| 5 | F | 1 | count++ | 2 | none | up | none |
| 6 | F | 2 | count++ reaches T, open incident | 3 | open (started_at = step 4 time) | down | ONE down alert per attached channel |
| 7 | F | 3 | stay down, no re-notify | 4 | open | down | none |
| 8 | H | 4 | close incident, reset count | 0 | closed (ended_at = step 8 time, close_reason recovered) | up | ONE recovery alert per attached channel |

With `T = 1`, a single `F` opens the incident immediately; the next `H` closes it. Extra rules carried from v1:

- `started_at` is the FIRST failing check in the run that opened the incident (step 4), not the threshold-crossing check (step 6). Recovery duration = `ended_at - started_at`.
- Any `H` while count > 0 and no incident open resets count to 0, no notification (step 3).
- Disabling a down monitor closes the incident with `ended_at` = disable time, `close_reason = disabled`, **no** recovery alert; status becomes `disabled`.
- Editing a down monitor's config does **not** auto-close the open incident; the next check with the new config drives the transition normally.
- A monitor with zero channels still opens/closes incidents and changes status; it just sends nothing.
- **Contract: one down alert, one up alert, per incident.** No re-notify-while-down in v1 (re-notify is a phased option, default off, section 15).
- **Multi-region feeds in before the machine runs.** When a monitor has more than one region, the alerting service first reduces the per-region results into a single monitor-level healthy/unhealthy verdict using `down_policy` and probe-fleet health (6.7), then drives this exact state machine with that verdict. The state machine itself is unchanged: the counts, threshold, incident open/close, and one-down/one-up dedup all govern the resulting up/down transition as written above.

### 6.5 Monitor status values (reused from v1 12.1)

Exactly four derived values, evaluated top to bottom: **disabled -> pending -> down -> up**.

- `disabled`: `enabled` is false (priority over all).
- `pending`: enabled, zero check results yet.
- `down`: enabled, has results, an incident is open.
- `up`: enabled, has results, no open incident.

A single failing check before `T` is reached does NOT make status `down`; status stays `up` until the incident opens. "Last check time" = timestamp of most recent result (null when pending). "Last latency" = latency of most recent result (null when pending or when the last check had no latency, e.g. connection error or blocked target). Status is derived, never user-editable.

### 6.6 Where this runs distributed (the SaaS promise)

The single-process scheduler of v1 is replaced by a distributed loop. Product promise: **checks run on their schedule, accurately, at SaaS scale, with no single point that stalls everyone.**

- **scheduler service**: owns the schedule for all orgs' monitors. Decides which checks are due and enqueues check jobs (onto the event bus, Kafka or Redis Streams via `PULSE_BUS`) keyed so a given monitor's checks are ordered and not duplicated. Rebuilds schedule from PostgreSQL on start; in-flight incident state is derived from stored data, never held only in memory.
- **worker fleet**: horizontally scaled stateless workers consume check jobs from the bus, execute the HTTP request with timeout, apply SSRF policy, and emit a check-result event; the platform persists the result. Workers scale out with monitor count and check frequency. Per-monitor exclusion (for "check now" vs scheduled) is enforced with a short Redis lock.
- **alerting service**: consumes check-result events, runs the per-monitor state machine (counts, threshold, incident open/close, dedup of one-down/one-up), persists incidents, and emits notification events. Dedup and ordering per monitor are the correctness-critical part and must be exactly-once in effect even if a check event is redelivered.
- **notifier service**: consumes notification events and delivers to channels (Slack/Discord/webhook/SMTP) with retry/backoff. A failed delivery after retries is recorded visibly (incident timeline + audit/log). Outbound delivery is at-least-once; payloads carry an idempotency-friendly identity so a duplicate delivery is recognizable downstream.
- **api service**: the SPA backend and public REST API. Authn (OAuth/JWT/API key), authz (org + role), CRUD, reads of status/history/incidents.

**Control plane vs regional data plane.** The topology splits in two. The **control plane** holds api, scheduler, alerting, notifier, PostgreSQL, and the central event bus (Kafka or Redis Streams via `PULSE_BUS`), in one home region. The **regional data planes** are the worker fleets that actually reach customer endpoints, one fleet per region we operate (6.7). The scheduler enqueues a check job tagged with its target region; the worker fleet in that region executes it and writes the result, tagged with the region, back to the central store. All cross-region aggregation (down policy, uptime, status pages) happens against that central store, so there is one source of truth for a monitor's state even though checks fan out across regions.

Scheduling accuracy, throughput, and delivery-latency targets are committed in section 12.

### 6.7 Multi-region check execution

Pulse owns probe machines in several regions and checks customer endpoints from more than one of them, so it can tell a real outage from a regional network problem and detect downtime that only shows in one part of the world. This is a first-class capability designed in from day one (section 12), with the region set delivered in phases (section 15).

**Per-region execution and attribution.** A monitor carries a `regions` list (6.2). For each scheduled tick the scheduler enqueues one check job per selected region; the worker fleet in each region runs the request independently and writes a result tagged with its `region` (6.3). Every result is attributed to where it ran, so per-region history and "which region saw it fail" are first-class, not inferred.

**Down policy (the single monitor verdict).** The per-region results are reduced to one monitor-level healthy/unhealthy verdict by the monitor's `down_policy`, then handed to the existing per-monitor state machine (6.4):

| `down_policy` | Monitor counts as unhealthy when | Use for |
|---------------|----------------------------------|---------|
| `any` | at least one selected region sees it unhealthy | strict; catches regional outages fast, more sensitive to a single bad path |
| `quorum` (default) | a majority of the selected regions see it unhealthy | balanced; a blip in one location does not page |
| `all` | every selected region sees it unhealthy | conservative; only pages on a clearly global outage |

The default is `quorum` so a single regional blip does not page when the customer is checking from several regions. The state machine still owns the up/down transition: the verdict is just the healthy/unhealthy input to step-by-step counting, threshold, and incident open/close.

**Probe-fleet health (the false-positive problem we will not ship).** Our own regions can be slow, partitioned, or fully down, and a region that is down produces no result. That absence must never be read as the customer being down. The platform tracks the health of our own regions (per-region heartbeats and liveness) and distinguishes two things that look similar from the outside:

| Situation | What it means | What the platform does |
|-----------|---------------|------------------------|
| Region returns an unhealthy result | the target is down from that region | counts toward `down_policy` |
| Region returns no result and the region is unhealthy | our probe region is degraded or unavailable, says nothing about the target | excluded from `down_policy`, never counted as the target being down |

When too few healthy regions remain to satisfy a monitor's `down_policy`, the platform does not declare the monitor down on missing data. Instead it surfaces a **coverage-degraded** state on the monitor (visible in the UI and API) and, plan permitting, fails the affected region's checks over to another healthy region so coverage is restored. The regional workers run least-privilege with their own egress controls (section 13).

**Product guarantee: we never page you because our own probe region went down.** A missing result from a region we run is our problem to handle, not a reason to open your incident. Customer incidents come only from regions that actually saw the target unhealthy, aggregated by the down policy.

---

## 7. Notification channels (reused payloads from v1 12.7)

Channels are reusable destinations configured **per org** and attached many-to-many to monitors. v1 types: **Slack, Discord, generic webhook, email (SMTP)**. Each channel has id, org id, name, type, type-specific config (secrets encrypted at rest, redacted on read), enabled. Every channel supports **send test message** from the UI (member+ per the matrix). Secret config (webhook URLs, SMTP password) is write-only over the API and never returned.

Payloads are identical to v1 12.7 and reproduced in appendix B: the generic-webhook JSON envelope (with `event`, `monitor`, `incident`, `check`, `sent_at`, and `duration_seconds` on recovery), the Slack `text` payload, the Discord `content` payload, and the SMTP subject/body format. Human-readable timestamps are UTC with the `UTC` suffix; API/webhook fields are RFC3339.

Phased additions (section 15): PagerDuty, Opsgenie, SMS, Telegram, Microsoft Teams. These slot in as new channel types behind the same attach model.

---

## 8. Status pages

A public, shareable status page per org is a real adoption driver (it gets Pulse in front of the customer's customers) and a conversion lever (custom domain is a paid feature).

### v1 of status pages

- An org can create one or more **status pages**. Each has a name, a public slug (`status.pulsepager.com/{org-slug}/{page-slug}` or a single page at `{org-slug}.pulsepager.com`, decision in section 16), and a list of selected monitors to display.
- For each displayed monitor the public page shows: a **friendly display name** (not the raw URL, so internal URLs are not leaked), current status (up/down/degraded mapping from monitor status), and an **uptime summary** over a window (e.g. last 24h / 7d / 90d) plus a recent history bar. The raw URL, headers, body, assertions, and check internals are never exposed publicly.
- Overall page status banner: "All systems operational" / "Partial outage" / "Major outage" derived from the displayed monitors.
- Open incidents on displayed monitors appear as public incidents with start time and (on recovery) duration. Owner/admin/member can post a short human update on an incident that shows on the page (incident annotations).
- Page is published/unpublished (draft until published). Unpublished pages are not publicly reachable.
- Branding v1: org name and logo, light/dark, accent color. No full theming.

### Phased (section 15)

- **Custom domain** (`status.customer.com` via CNAME + managed TLS). Paid feature, a conversion lever. Owner/admin only.
- **Subscriber notifications**: end users subscribe by email (and later webhook/RSS) to get notified when a displayed monitor goes down/recovers. Adds a subscriber list and an unsubscribe flow.
- Scheduled maintenance windows shown on the page.

Decision: status pages read from the same check/incident data, no separate probing. They are a presentation layer, which keeps them cheap and always consistent with the monitoring truth.

---

## 9. Public API and webhooks

A first-class, documented REST API is core to the wedge (developer-first). It is authenticated by **API keys** (section 5), scoped to one org by the key.

### Contract expectations

- **Versioned**: base path `/api/v1/...`. Breaking changes go to `/api/v2`; v1 is supported with a deprecation window. Additive changes do not bump the version.
- **Org-scoped**: the key determines the org; resources are addressed under it (e.g. `GET /api/v1/monitors`). No cross-org access.
- **Rate-limited**: per-key rate limits, returned via standard headers (`X-RateLimit-Limit`, `-Remaining`, `-Reset`), `429` with `Retry-After` on exceed. Limits scale by plan tier (section 11).
- **Paginated**: cursor-based for list endpoints (results, incidents, and any large list), `{ "items": [...], "next_cursor": "..."|null }`, `limit` default 100 / max 500. Conventions reused from v1 12.3 (RFC3339 UTC timestamps, the standard error envelope with `code`/`message`/`fields`, redaction of secrets).
- **Documented**: the **OpenAPI 3 specification is the single source of truth** for the public REST API. The api service serves interactive **Swagger UI at `/api/docs`** so a developer can explore the API and try calls against their own org with a key. The spec is versioned with the API (it lives at `/api/v1`, a future `/api/v2` ships its own spec), and the static documentation site and pricing page (section 10) regenerate their API reference from this same spec on each release (section 15) so the docs never drift from the API. This is a selling point, not an afterthought.

### Surface (illustrative, not exhaustive)

- Monitors: list, create, get, update (including `regions` and `down_policy`, validated against the org's region entitlement), delete, check-now, results (`range` = 24h/7d/30d, filterable by region, paginated), incidents per monitor. A monitor read also exposes per-region status and any coverage-degraded state (6.7).
- Regions: list the regions available to the org (its plan entitlement) so clients can pick valid region codes.
- Channels: list (redacted), create, update (secrets write-only), delete, test.
- Incidents: list (filter by status), get, annotate, manual close (admin+ role on the key).
- Status pages: list, create, update, publish.
- Members / invitations / API keys / billing: read and management endpoints gated by the key's role per the matrix (e.g. billing-management endpoints require an owner, which keys are not issued for, so billing changes stay UI-only by design).

### Outbound webhooks (events)

Beyond the per-monitor generic-webhook channel, an org can register **org-level outbound webhooks** to ingest events into their own systems: monitor down/recovery, incident opened/closed, and (phased) monitor created/updated/deleted and member changes. Delivery is at-least-once with retry/backoff and a per-event id for dedup, signed with a per-webhook secret so the receiver can verify authenticity. This lets SREs wire Pulse into their own pipelines without polling.

---

## 10. Dashboard / UX surfaces

The frontend is a Lit SPA served by nginx, talking to the api service over JSON. Screens:

1. **Signup / login**: "Sign in with Google" and "Sign in with GitHub." No password fields. Handles invitation-accept entry.
2. **Onboarding (first run in a new org)**: a guided "create your first monitor" + "connect a channel" + "see your first result" flow. Time-to-first-monitor is a North Star input (section 14), so this is deliberately short.
3. **Org switcher**: in the top nav; lists the user's orgs, shows the active one, "create organization," and quick role indicator.
4. **Monitors list (home)**: every monitor in the active org with name, status (up/down/disabled/pending), last check time, last latency, recent-history sparkline, quick enable toggle, clear red for down. Derivation per section 6.5.
5. **Monitor detail**: full config, current status, per-region status (and any coverage-degraded indicator, 6.7), recent check history table filterable by region, latency-over-time chart, and this monitor's incidents.
6. **Monitor create/edit form**: all fields from 6.2 with validation (valid URL, sane interval/timeout, plan-tier interval floor, region selection limited to the plan's allowed regions, `down_policy` picker) and channel selection.
7. **Channels**: list, CRUD, per-channel "send test." Secrets shown as "configured," never the value.
8. **Incidents**: org-wide list (open + recently closed), each with monitor, start, end, duration, cause, annotations; acknowledge/annotate, and manual close for admin+.
9. **Status page editor**: create/manage pages, pick monitors, set display names, branding, publish toggle, public-link preview, (phased) custom domain.
10. **Members and roles**: member list with roles, invite by email + role, change role, remove, transfer ownership (owner), pending invitations with resend/revoke.
11. **API keys**: list (name, role, prefix, created-by, created, last-used), create (secret shown once), revoke.
12. **Billing / usage**: current plan, seats used/available, monitor count vs limit, check-frequency tier, retention, usage meters and overage state, plan change, payment method, invoices (owner manages; admin views).
13. **Org settings**: org name, slug, default check settings, SSRF policy display, audit log access, delete organization (owner).
14. **Account settings**: profile (from provider), linked providers (connect/disconnect Google/GitHub), sessions ("log out of all devices"), the orgs they belong to.

Beyond the authenticated SPA, two public surfaces support the developer-first wedge:

- **Interactive API explorer** (Swagger UI) served by the api service at `/api/docs`, generated from the OpenAPI spec (section 9), where a developer can read and try the API with a key.
- **Public documentation site and pricing page** hosted on **GitHub Pages**: a static site with guides, the generated API reference, and a pricing page. A CI job regenerates the API reference from the OpenAPI spec on each release (section 15) so the public docs never drift from the live API.

UX principles carried from v1: clean, fast, opinionated layout. No dashboard builder, no theming beyond status-page branding.

---

## 11. Billing and plans

Billing shipped on Paddle (Paddle is the Merchant of Record, so it handles payment, plan management, taxes, and invoices). The plan/seat/usage model shapes the data model, limits enforcement, and the upgrade prompts throughout the UI.

### Billing axes and metering dimensions

Pricing is **per-seat and per-API** (per monitored endpoint, that is per monitor) layered on top of the tier packages, plus **region availability**. The tier sets the included allowances and floors; growth in seats and monitors is what scales the bill. We meter on dimensions that track value and cost without punishing normal growth:

1. **Monitors** (count of enabled monitored endpoints) - primary per-API value meter.
2. **Minimum check interval** (frequency tier) - faster checks cost more compute; tiered by plan.
3. **Seats** (accepted members + reserved pending invites) - per-seat team value meter.
4. **History retention** (days of raw check results) - storage cost meter.
5. **Regions** (which and how many regions a monitor can be checked from) - a feature entitlement and a real COGS dimension, since our own regions cost us differently (6.7, see region row below).
6. **Status pages** (count, and custom domain as a feature flag) - adoption/feature meter.
7. **API rate limit** (requests/min per key) - tiered by plan.

We deliberately do **not** meter on number of checks executed (the per-check pricing that makes Pingdom expensive). Pricing on monitors + frequency tier is more predictable for customers and still tracks our cost, and it is the pricing wedge in section 1. Trade-off: a customer with few monitors at very high frequency is cheaper for us than the model implies; the frequency tier covers that case.

**Region cost.** Region availability is both an entitlement and a cost dimension. The free tier gets one default region; higher tiers unlock more regions and premium regions. We track per-region check cost so multi-region margin stays visible, and scheduling is cost-aware: low tiers default to the cheaper home region so we are not paying premium-region cost on free traffic.

### Plan tiers (illustrative and GTM-tunable, but anchored to leadership's numbers)

The tiers and their public names live on the pricing page (`docs-site/pricing.html`), which is the source of truth; this table mirrors it. The internal plan codes stay `tier1` / `tier2` / `tier3` / `tierCustom` (RFC-017: display names change, identifiers do not), shown in parentheses. Free is capped at **10 monitors** with a **15-minute minimum check interval**; Professional reaches a **1-minute** interval and Custom is faster still (down to 30s) and contract-negotiated.

| Dimension | Free (`tier1`) | Hobby (`tier2`) | Professional (`tier3`) | Custom (`tierCustom`) |
|-----------|------|----------------|-------------|-----------------|
| Price | $0 | $7/mo | $19/mo | From $129/mo |
| Monitors | 10 | 25 | 50 | Custom |
| Min check interval | 15 min | 5 min | 1 min | Custom (down to 30s) |
| Regions per monitor | 1 (single region) | 1 (single region) | up to 4 | all + residency |
| Seats included | 1 | 3 | 10 | Unlimited |
| History retention | 7 days | 30 days | 90 days | 180 days |
| Status pages | 1 (Pulse subdomain) | 3 | 10 | Custom |
| Custom domain status page | N | N | Y | Y |
| API access | None | Read-only | Full | Full |
| Outbound webhooks | N | N | Y | Y |
| Audit log | N | N | 30 days | 1 year |
| SSO (SAML / Okta) | N | N | N | Y |
| Support | Community | Email | Priority email | Priority + SLA |

Custom (`tierCustom`) is the contract-negotiated tier (SSO, residency, SLA, custom limits); the entitlement code carries generous defaults (very large seat/monitor/status-page caps) that a contract overrides.

### Entitlement enforcement (cross-cutting)

Plan limits are an entitlement set per org, and enforcement is cross-cutting: it happens in more than one place on purpose so a downgrade cannot be worked around.

- **At the api on write.** Every write that touches a metered limit is checked against the org's entitlement: the monitor cap (creating the 3rd monitor on Free is blocked with an upgrade prompt), the interval floor (setting an interval below the plan floor is rejected with the per-field error shape), the seat cap (inviting past seats is blocked), region selection (picking a region the plan does not include is rejected), and the status-page count. These return the standard per-field error shape with an upsell.
- **At the scheduler, independently.** The scheduler respects the org's interval floor and region entitlement when it dispatches, so an existing monitor created under a higher plan cannot keep running at a faster interval or in a richer region set after a downgrade. The api enforces on write; the scheduler enforces on every dispatch. Neither trusts the other, so there is no single place to bypass.
- **Cached for the hot path.** Org entitlements are cached so the scheduler and api do not pay a database read per check or per request; cache invalidation follows plan changes.
- Downgrades that would exceed the lower plan's limits prompt the owner to bring usage under the limit first (disable monitors, remove members, drop extra regions) rather than silently deleting data.
- Payment, plan management, and invoices run through Paddle (Merchant of Record), which is shipped. Limits are enforced regardless of payment state.
- Trials: a no-card trial is offered (3 days monthly, 7 days annual), dropping safely to Free on expiry, with a 35-day re-trial deny window anchored on the subscription's `ended_at` (full model and trial mechanics live in PRD-006).

(This section states the product behavior; the architecture team owns how the cache, the write checks, and the scheduler checks are built.)

---

## 12. Non-functional requirements (committed targets)

These numbers are commitments that drive the architecture. They are defensible for the target market and intentionally not hyperscale-on-day-one.

### Scale targets

| Target | Commitment | Justification |
|--------|-----------|---------------|
| Organizations | Tens of thousands (design for 50k) | SMB SaaS scale; per-org isolation must hold at this count |
| Active monitors | Hundreds of thousands (design for 500k) | 50k orgs averaging ~10 active monitors, with headroom for large orgs |
| Sustained check throughput | ~10,000 checks/sec sustained, 2x burst | 500k monitors, blended avg interval ~60s -> ~8.3k/s steady; round up and keep burst headroom |
| Scheduling accuracy | Check dispatched within 5 s of its scheduled time at p99 under normal load | "On schedule" is the product promise; 5 s is invisible at 30s+ intervals and achievable with a distributed scheduler + queue |
| Check-result to decision latency | Alerting state updated within 5 s of the check result at p99 | Keeps detection fast; the queue path (worker -> bus -> alerting) must stay short |
| Notification delivery latency | Down/recovery notification sent within 30 s of the triggering check at p99 (excluding the third-party channel's own latency) | "Know before your customers" needs sub-minute end-to-end; our controllable budget is 30 s |
| API latency | p99 < 300 ms for reads, p99 < 500 ms for writes (excluding "check now" which does network I/O) | Snappy SPA + good API DX; standard SaaS API expectation |

### Availability

- **SLA target: 99.9%** monthly for the control plane (api service, dashboard, status-page serving) and for the **check + alert pipeline** (a due check runs and, on a state change, a notification is sent). This is the customer-facing promise: monitoring keeps working and alerts keep flowing.
- 99.9% (about 43 min/month of allowed downtime) is the right first commitment: credible, sellable, and reachable with Kubernetes redundancy and managed PostgreSQL/Redis/Kafka, without the cost of a 99.99% multi-region active-active control plane. Note this is about control-plane availability, separate from multi-region checking, which is a check-execution capability (6.7) and ships earlier. We revisit the control plane to 99.95%+ in a later phase (section 15).
- Status-page serving should be especially resilient (it is what customers show during their own incidents); it is read-mostly and can be cached/served independently so it stays up even if write paths degrade.

### Data retention tiers

- Raw check results: retained per the plan's retention tier (7 / 30 / 90 / 180 days), then deleted by a background cleanup job. Incidents and monitor config are retained for the life of the org (not subject to raw-result cleanup).
- At scale, long retention is backed by rollups (hourly aggregates) so the per-monitor history/uptime views stay fast; raw rows age out while rollups persist for uptime math on status pages. (Rollups are an architecture detail; the product commitment is fast history and correct uptime over the retention window.)

### Multi-region posture

Multi-region is designed in from day one, with the region set delivered in phases. It is not a "single region now, multi-region someday" deferral.

- **Designed in from the start.** Region is part of the check-job and check-result schema from Phase 0 (6.3), every result is attributed to the region it ran from, the monitor verdict is aggregated across regions by `down_policy` (6.7), and the platform tracks the health of our own regions so a region that is down never produces a false positive (6.7). The control-plane vs regional-data-plane split (6.6) is part of the topology, not a later retrofit.
- **Delivered in phases (honest about what ships when).** Phase 0 carries the `region` field and runs everything from the single home region (one region in the set). The multi-region capability (region selection, per-region checks, quorum down policy, probe-fleet health, coverage-degraded state, regional fail-over) lands in a defined later phase and GA may launch with a small set of regions; the region set grows over phases. Regional **data residency** for compliance-sensitive customers is the last piece, alongside enterprise (section 15).
- The data plane writes results back to the central store for cross-region aggregation, so there is one source of truth for a monitor's state regardless of how many regions check it.

---

## 13. Security, privacy, and compliance

The detailed compliance and data-governance design lives in `docs/rfc/RFC-015-compliance-and-data-governance.md`: the full data inventory and classification, the GDPR data-subject-rights mechanics (export, erasure with the 14-day grace and the backup exception), data residency, the SOC 2 / ISO 27001 controls matrix, breach-notification readiness, and the PCI boundary. RFC-015 is honest about which steps are engineering's and which need legal counsel and an audit firm.

### Tenant isolation (hard requirement)

- Every resource carries an `org_id`. Every query and every authorization check is scoped to the caller's active org. A user or key from org A can never read or affect org B's data, under any endpoint, ever. This is the top security invariant and must be enforced at the data-access layer, not just in handlers, and covered by tests.

### Regional isolation and failure handling

- Regional worker fleets (6.7) run with **least privilege** and their own network egress controls, the same SSRF posture as any worker, so a regional fleet cannot reach internal services or other regions' sensitive endpoints.
- **A region going down is a handled failure mode, not a security or correctness incident.** Probe-fleet health distinguishes "our region is degraded" from "the target is down" (6.7), so a region failure never produces a false positive and never pages a customer. Coverage-degraded state and, where the plan allows, fail-over to a healthy region keep monitoring honest while a region is unavailable.

### Data classification

| Class | Examples | Handling |
|-------|----------|----------|
| Secrets | Slack/Discord/generic-webhook URLs, SMTP password, monitor headers flagged `secret`, API key material, webhook signing secrets | Encrypted at rest (AES-256-GCM, per-value nonce, reused from v1 12.6 approach); write-only over the API; redacted on read; never logged. API key/secret stored only as a hash. |
| PII | User emails, names, avatars, invitation emails, status-page subscriber emails (phased) | Encrypted at rest (managed disk/db encryption at minimum), access-controlled by role, exportable and deletable for GDPR. |
| Operational | Check results, latencies, incident records, the last-failure response snapshot (headers + capped body, PRD-002 3.8) | Org-scoped, retained per plan, not exposed cross-org; only friendly names exposed on public status pages. The failure snapshot is treated as operational, not secret: Pulse does not assume a monitored endpoint returns anything sensitive, and what it returns is the customer's responsibility. |

### Encryption

- In transit: TLS everywhere (client to api, status pages, and service-to-service in the cluster).
- At rest: database and disk encryption for all stores; application-level AES-256-GCM for the secret class above so a DB leak does not expose channel/header secrets in plaintext.

### Audit logging

- Sensitive actions are recorded in a per-org audit log: member invited/removed/role-changed, ownership transfer, API key created/revoked, billing/plan change, channel created/deleted, monitor deleted, status page published, org settings changed, manual incident close, org deleted. Each entry: who, what, when, from where (IP/agent), and the target resource. Visible to owner/admin (matrix), retained per plan tier.

### GDPR / privacy

- **Data export**: a user can export their personal data; an owner can export an org's data (monitors, incidents, members, settings) in a machine-readable format.
- **Data delete**: a user can delete their account (which transfers or deletes personal orgs and removes memberships); an owner can delete an org, which removes all its data after a 14-day grace window (during which it can be restored). Deletions are honored within a committed window.
- Data processing agreement and subprocessor list published for paying customers.

### SOC 2 path

- Build toward SOC 2 Type II from the start: audit logging, access controls, encryption, change management, and vendor management are designed in, not retrofitted. Formal audit targeted in phase 3 alongside enterprise (SSO/SCIM), because enterprise buyers will require it. We state the path now so we do not make choices that block it.

### SSRF (first-class threat)

This is the marquee security risk and is *worse* in SaaS than in self-host: workers fetch arbitrary customer-supplied URLs from inside our infrastructure, so a malicious customer could try to reach our internal services or cloud metadata (`169.254.169.254`, internal IPs, link-local, loopback).

**Product stance (committed, stricter than v1's opt-in):**

- SSRF protection is **on by default and not customer-disableable** in the hosted product. Workers refuse any target that resolves to loopback, link-local, cloud-metadata, or private (RFC1918) ranges and record the check as `blocked_target` (request not sent), reusing the v1 failure-reason semantics.
- Protection is enforced at resolution time on the worker (resolve, validate every resolved IP, then connect to the validated IP) to defeat DNS-rebinding, with redirects re-validated on each hop. Workers run with least privilege and network egress controls so a bypass still cannot reach sensitive internal endpoints.
- Rationale and trade-off: v1 defaulted to allow because it was the operator's own box. In multi-tenant SaaS that reasoning inverts entirely; allowing private targets would let one tenant probe our infrastructure or other tenants' adjacent networks. We accept that customers cannot monitor their own private/internal endpoints directly from the public service; that need is served by a future private-location agent (phase 3), not by relaxing the default.

---

## 14. Success metrics and North Star

**North Star: number of active monitors that are healthily firing checks across paying orgs** (proxy for delivered value: real services being watched by customers who pay). It rises with activation, breadth of adoption inside an org, and retention.

Supporting metrics:

| Stage | Metric | Target intent |
|-------|--------|---------------|
| Activation | Time to first monitor (sign-in -> first check result) | Under ~2 minutes median |
| Activation | Time to first alert configured (a channel attached to a monitor) | Within first session for most activated users |
| Activation | % of new orgs that create >=1 monitor in week 1 | High (this is the core funnel step; track and optimize onboarding) |
| Engagement | % of orgs with >=2 members (team adoption) | Indicates the team model is landing |
| Engagement | % of orgs with a published status page | Status pages drive external reach |
| Retention | Org week-4 and month-3 retention | Core durability signal |
| Conversion | Free -> paid conversion rate; trigger reasons (monitor cap, seats, custom domain, frequency) | Validates the metering dimensions |
| Reliability | Alert signal-to-noise (are customers raising failure_threshold because we are noisy?) | Carried from v1; noisy alerts kill trust |
| Reliability | Self-measured pipeline SLA attainment vs the 99.9% target | We monitor ourselves |

---

## 15. Phased delivery

Each phase states explicit in/out scope.

### Phase 0 - Internal MVP (prove the distributed loop end to end)

The smallest thing that proves the distributed register -> check -> detect -> notify loop across the real services.

- In: a single org (internal), HTTP GET monitors via API/seed, the five services wired (api, scheduler, worker, alerting, notifier) on Kubernetes with PostgreSQL + Redis + the event bus (Kafka or Redis Streams via `PULSE_BUS`); scheduler enqueues, worker fleet executes with timeout + SSRF block, alerting runs the state machine with one-down/one-up dedup, notifier delivers to Slack; check results in PostgreSQL; a bare monitors-list behind Google login. The check-job and check-result schema carry the `region` field from this phase (single home region for now), so multi-region is additive later.
- Out: multi-org UI, roles, other channels, status pages, billing, public API, the non-GET fields, multi-region check execution (the field exists, the fan-out does not yet).
- Done when: a monitor goes down and recovers and exactly one down + one recovery Slack message arrive, with correct incident timing, through the distributed pipeline, and it survives a worker restart.

### Phase 1 - Private beta (core monitoring + orgs + social login + channels)

- In: full multi-tenancy (users, orgs, memberships, seats, invitations, org switcher); Google + GitHub login + account linking; full RBAC (owner/admin/member/viewer) and the permission matrix; full HTTP monitor fields and validation (reused v1); the complete alerting machine and status derivation (reused v1); all four channels with test-send and the reused payloads; full UI screens 1-8, 10, 13, 14; retention cleanup; secret encryption + redaction; SSRF on-by-default; audit log.
- Out: status pages, public API, billing, non-HTTP check types, SSO/SCIM, multi-region check execution (still single home region; the `region` field is carried), rollups (raw queries are fine at beta scale).
- Done when: external beta orgs can sign in, invite teammates with roles, create monitors, get correct alerts, all isolated per org.

### Phase 2 - GA (status pages, public API, billing)

- In: status pages v1 (section 8, Pulse subdomain, no custom domain); public REST API v1 with API keys, rate limits, pagination, OpenAPI spec as the single source of truth served as interactive Swagger UI at `/api/docs`, plus the public documentation site and pricing page on GitHub Pages with the CI job that regenerates the API reference from the spec on each release; outbound org-level webhooks; billing with Paddle (plans, seats, per-monitor and per-region metering, usage limits enforced, invoices); cross-cutting entitlement enforcement (api on write and scheduler on dispatch, with cached entitlements, section 11); plan-tier interval floors and limits including region entitlement; **multi-region check execution** (region selection, per-region checks, `down_policy` aggregation, probe-fleet health, coverage-degraded state, plan-permitting regional fail-over, section 6.7), launching with a small set of regions; rollups for fast history/uptime at retention scale; usage/billing UI (screens 9, 11, 12).
- Out: custom-domain status pages, subscriber notifications, non-HTTP types, SSO/SCIM, regional data residency, an expanded region set (GA ships a small set), PagerDuty/Opsgenie/SMS/Teams/Telegram.
- Done when: a customer can self-serve sign up, pay, manage monitors via API, and publish a status page, with limits enforced per plan.

### Phase 3 - Scale and enterprise

- In: SSO (SAML/OIDC) and SCIM provisioning; custom roles; more check types (TCP, DNS, keyword/browser, ICMP, cron/heartbeat; TLS/SSL-cert-expiry already shipped at GA, section 6.1); custom-domain status pages + subscriber notifications + maintenance windows; PagerDuty, Opsgenie, SMS, Telegram, Microsoft Teams channels; re-notify-while-down and basic escalation; **expanded region set and regional data residency** for compliance-sensitive customers (multi-region check execution itself shipped at GA, section 6.7); private-location agent (monitor internal endpoints safely); SOC 2 Type II; advanced alerting (latency/SLO percentile alerts); 99.95% control-plane SLA tier.
- Out: APM/tracing/logs, full on-call scheduling product, general dashboard builder (still non-goals).

---

## 16. Open product decisions (each with a recommended default)

Core behavior is decided in the body above. These are genuinely open, each with my recommended default and the trade-off; we ship the default unless overridden.

1. **Do pending invitations reserve a seat?** Recommended: **yes, reserve.** Prevents over-inviting past plan limits and makes billing predictable. Trade-off: a team can be temporarily "full" of unaccepted invites; mitigated by 7-day expiry and easy revoke. (Used in section 3.)

2. **Must the invited email match the signed-in provider email on accept?** Recommended: **yes, must match.** Stops invitation links from being forwarded and accepted by the wrong person. Trade-off: someone whose GitHub email differs from the invited address must use the matching identity or ask for a re-invite; acceptable for security.

3. **Status-page URL shape.** Recommended: **`{org-slug}.pulsepager.com` subdomain per org, with multiple pages as paths** (`{org-slug}.pulsepager.com/{page-slug}`), single page served at the root. Cleaner and more brandable than a shared `status.pulsepager.com/{org}/{page}` path, and it sets up custom domains naturally. Trade-off: subdomain management and wildcard TLS to handle; worth it. (Used in section 8.)

4. **Personal org vs forced team org at signup.** Recommended: **always create a personal org** (section 3). Trade-off: some users end up with an unused personal org after joining a company org; harmless, and it guarantees no empty-state dead end.

5. **API key role ceiling.** Recommended: **keys max out at admin; no owner-equivalent keys** so billing, ownership transfer, and org deletion cannot be automated by a leaked key. Trade-off: fully scripting an org's lifecycle (including billing) is not possible via API; acceptable, those are deliberate human actions. (Used in section 5.)

6. **Account merge when provider emails diverge.** Recommended: **manual support action in v1**, automated self-serve merge deferred. Trade-off: rare friction for users who change their provider email; low volume, not worth the complexity and risk of an automated merge early.

7. **Degraded state on monitors and status pages.** Recommended: **no separate "degraded" monitor status in v1** (keep the four: disabled/pending/down/up, reused from v1); status pages may render "partial outage" at the page level from the mix of monitor statuses, but an individual monitor is binary up/down. Trade-off: latency-degraded-but-up is not its own state yet; that is SLO/percentile alerting, explicitly phase 3. Note this is about a monitor's health status; it is unrelated to the **coverage-degraded** signal in 6.7, which is about our own probe regions being unavailable, not the target being slow.

8. **Manual incident close availability.** Recommended: **owner/admin only** (matrix), and a manually closed incident is recorded with a distinct close reason. Trade-off: members cannot hand-close; acceptable since the machine auto-closes on recovery and manual close is an override.

---

## Appendix A - Per-field validation rules (reused verbatim from v1 12.4)

Enforced on create and update, server-side (UI mirrors, server authoritative):

- `name`: required, non-empty after trim, max 200 chars.
- `url`: required, absolute URL with scheme `http` or `https` only (others rejected), must have a host.
- `method`: required, one of GET/POST/PUT/PATCH/DELETE/HEAD, default GET.
- `headers`: optional list of {key, value, secret?}; non-empty keys, no duplicates, max ~50; `secret` default false (when true, encrypted at rest and redacted on read).
- `body`: optional; only for POST/PUT/PATCH (rejected for GET/HEAD/DELETE), size cap ~1 MB.
- `expected_status_codes`: required, non-empty; explicit codes (100..599) and/or `2xx/3xx/4xx/5xx`; default `200`.
- `timeout_seconds`: required integer 1..60, default 10.
- `interval_seconds`: required integer, minimum 30 hard floor, must be >= timeout, default 60 (plan tier may raise the effective floor).
- `enabled`: required bool, default true.
- `max_latency_ms`: optional positive integer.
- `body_contains`: optional string, max ~1000 chars; engine reads up to the 64 KB body cap to test it.
- `failure_threshold`: required integer >= 1, default 1.
- `notification_channel_ids`: optional list; each must reference an existing channel in the same org; empty allowed (tracked, silent).
- `regions`: required non-empty list of region codes; each must be a region the org's plan includes (rejected otherwise with the per-field error), no duplicates; count limited by the plan; default the plan's home region (section 11, 6.7).
- `down_policy`: required enum, one of `any` / `quorum` / `all`, default `quorum` (6.7).

Errors use the standard envelope (`code`/`message`/`fields`) from v1 12.3.

## Appendix B - Notification payloads (reused verbatim from v1 12.7)

### Generic webhook (POST, application/json)

Down:

```json
{
  "event": "down",
  "monitor": { "id": "mon_123", "name": "Prod API health", "url": "https://api.example.com/health", "method": "GET" },
  "incident": { "id": "inc_456", "started_at": "2026-06-21T14:00:00Z", "ended_at": null },
  "check": { "checked_at": "2026-06-21T14:00:30Z", "healthy": false, "failure_reason": "status_mismatch", "status_code": 503, "latency_ms": 120, "error": null },
  "sent_at": "2026-06-21T14:00:31Z"
}
```

Recovery (adds top-level `duration_seconds`, `event` = `recovery`, `incident.ended_at` set, `check.healthy` true, `failure_reason` null):

```json
{
  "event": "recovery",
  "monitor": { "id": "mon_123", "name": "Prod API health", "url": "https://api.example.com/health", "method": "GET" },
  "incident": { "id": "inc_456", "started_at": "2026-06-21T14:00:00Z", "ended_at": "2026-06-21T14:10:00Z" },
  "check": { "checked_at": "2026-06-21T14:10:00Z", "healthy": true, "failure_reason": null, "status_code": 200, "latency_ms": 95, "error": null },
  "duration_seconds": 600,
  "sent_at": "2026-06-21T14:10:01Z"
}
```

Field types: `event` string (`down`/`recovery`); monitor fields strings; `incident.id` string, `started_at` RFC3339, `ended_at` RFC3339 on recovery else null; `check.checked_at` RFC3339, `healthy` bool, `failure_reason` string-or-null (one of the section 6.3 reasons; null on recovery), `status_code` integer-or-null, `latency_ms` integer-or-null, `error` short-string-or-null; `sent_at` RFC3339. Generic-webhook custom headers configured on the channel are sent with the request.

### Slack (incoming webhook)

Down:

```json
{ "text": ":red_circle: *DOWN* Prod API health\nhttps://api.example.com/health\nReason: status_mismatch (HTTP 503)\nDown since 2026-06-21 14:00:00 UTC" }
```

Recovery:

```json
{ "text": ":large_green_circle: *RECOVERED* Prod API health\nhttps://api.example.com/health\nWas down for 10m 0s (since 2026-06-21 14:00:00 UTC)" }
```

### Discord (incoming webhook)

Down:

```json
{ "content": "**DOWN** Prod API health\nhttps://api.example.com/health\nReason: status_mismatch (HTTP 503)\nDown since 2026-06-21 14:00:00 UTC" }
```

Recovery uses `**RECOVERED**` and the down-duration line.

### Email (SMTP)

Subject: `[Pulse Pager] DOWN: Prod API health` or `[Pulse Pager] RECOVERED: Prod API health`. Plain-text body (HTML optional) with the same facts: monitor name, URL, reason + status/latency, when it went down, and on recovery the duration. Sent from the configured `from` to the configured recipients over the configured host/port with TLS per channel config.

Note (branding, RFC-017): the subject prefix changed from `[Pulse]` to `[Pulse Pager]` as part of the product rename. This is an intentional, additive branding update. The payload structure and field keys are unchanged; only the human-readable prefix text differs. The same prefix change applies to the SMS subject text. Slack and Discord payloads above carry no product prefix and are unchanged.

All human-readable timestamps are UTC with the `UTC` suffix; API/webhook fields use RFC3339 per the section 9 conventions.
