# PRD-006 - Billing & Entitlements

Status: DRAFTING
Owner: Product (Principal PM)
Parent: `PRD.md` (master), primarily section 11 (billing and plans), with section 3 (seats), section 4 (RBAC), section 12 (NFR/retention), section 16 (open decisions).
Audience: Architecture team (entitlements enforcement, scheduler, api) and go-to-market.
Phase: enforcement model lands in Phase 1; Paddle and metered billing land in Phase 2 (master section 15). The entitlement model and enforcement are needed early because they gate core behavior.

This sub-PRD fully specifies the billing and entitlements domain. It does not restate the master; it expands master section 11 into a buildable product spec and stays consistent with the locked anchors there.

---

## 1. Overview, goals, non-goals

### 1.1 Overview

Pulse is billed per organization. Each org sits on exactly one plan (master section 3: "Organization 1..1 Plan/subscription and billing customer record"). A plan grants an **entitlement set**: the allowances and floors that govern what the org can do (monitor cap, interval floor, seats, regions, retention, status pages, API rate, channels, audit-log retention, support). Pricing scales on two axes on top of the tier package: **per-seat** and **per-API** (per monitor), plus **region availability**. We deliberately do not meter per check executed (master section 11).

Two things are separable on purpose and ship at different times:

- **The entitlement model and its enforcement** are product behavior. They gate creating monitors, setting intervals, inviting members, selecting regions, and serving the API. These must work in Phase 1, before any payment integration, because they shape the data model and the upgrade prompts throughout the app (master section 11 opening note).
- **Paddle** (payment, plan management, invoices, proration, subscription-state webhooks) is Phase 2. Until Paddle is wired, plans are set internally by an operator and limits are still enforced exactly the same way (master section 11: "Until then, plans are set internally and limits are still enforced").

### 1.2 Goals

1. **Predictable pricing wedge.** A customer can read the pricing page and know what they will pay as they grow, because the bill scales on seats and monitors, not on a check counter they cannot predict (master section 1 wedge, section 11). This is the headline differentiator versus per-check competitors (Pingdom).
2. **Enforce limits everywhere they matter, with no bypass.** A limit is enforced at the api on write and independently at the scheduler on dispatch, so a downgrade cannot be worked around by a monitor created under a richer plan (master section 11 enforcement).
3. **Never silently destroy customer data on downgrade.** A downgrade that would exceed the lower plan asks the owner to bring usage under the limit first, rather than deleting monitors, members, or regions behind their back (master section 11).
4. **Make the limit visible before the user hits it.** Usage meters, at-limit states, and upgrade prompts appear in the relevant surfaces (master section 10 screen 12), so conversion happens at the moment of need, not after a confusing failure.
5. **Keep the hot path cheap.** Entitlement lookups must not cost a database read per check or per request; they are cached in Redis and invalidated on plan change (master section 11).

### 1.3 Non-goals

- **Per-check metering.** Explicitly rejected (master section 11). Out of scope for any tier, including Enterprise.
- **Usage-based overage billing in v1.** The recommendation is a hard block with an upgrade prompt at the cap, not a soft overage that silently bills more (section 11 of this doc, open decisions). Soft overage stays out of v1.
- **Custom per-customer contracts, invoicing, and net-terms.** Enterprise concern, phased (master section 11 Enterprise note, section 15 Phase 3). Self-serve card billing only in Phase 2.
- **Building a billing/payment engine.** Paddle owns money movement, plan catalog pricing, proration math, dunning, and invoices (section 8 of this doc). Pulse owns entitlement enforcement only.
- **Owner-equivalent API automation of billing.** API keys never reach owner, so billing cannot be scripted (master section 5, section 16 decision 5). Out of scope by design.
- **SSO/SCIM seat sync, regional data residency pricing, contractual SLA credits.** Phase 3 / Enterprise (master section 15).

---

## 2. Entities (conceptual)

These are conceptual product entities. The physical schema is the architecture team's call (RFC-001 data model, RFC-009 entitlements enforcement per `PLANNING.md`).

### 2.1 Plan

A named tier in the catalog: `tier1`, `tier2`, `tier3`, `tierCustom` (and `enterprise`, phased). A Plan carries the default entitlement values for that tier (the matrix in section 3) and, in Phase 2, a mapping to Paddle price IDs (per-seat add-on price, per-monitor add-on price, base package price). The Plan catalog is internal config, not per-org data.

### 2.2 Entitlement (limits)

The concrete set of allowances and floors that apply to one org. Derived from the org's Plan, but stored per org so it can be overridden (a support grandfather, a beta grant, an Enterprise custom limit) without forking the catalog. Fields mirror every metered/gated dimension in section 3: `monitors_cap`, `min_interval_seconds` (the floor), `seats_included` plus `seats_purchased`, `regions_allowed` (the set of region codes) plus `regions_per_monitor_cap`, `retention_days`, `status_pages_cap`, `custom_domain` (bool), `api_access` (none/read/full), `api_rate_per_min`, `outbound_webhooks` (bool), `channel_types_allowed`, `audit_log_retention_days`, `failure_snapshot` (bool, store the last failed check's response, PRD-002 3.8). The entitlement set is what gets cached in Redis (section 5.3).

### 2.3 Subscription

The link of an org to a Plan plus its purchased capacity and current usage snapshot. Conceptual fields:

- `org_id`, `plan` (the tier), `status` (`active` / `past_due` / `canceled` / `trialing`; in Phase 1 effectively always `active` since there is no payment).
- `seats_purchased` (paid seat count = `seats_included` plus per-seat add-ons).
- `billing_cycle` (`monthly` / `annual`, Phase 2).
- `provider_customer_id`, `provider_subscription_id`, `current_period_end` (provider-agnostic, see RFC-018).
- A usage view (computed, not authoritative billing): monitors used, seats used, status pages used. Usage is counted from the real resource tables (section 4), not stored as a running tally, so it cannot drift.

One org has exactly one Subscription (master section 3). Every resource the subscription gates (monitors, members, status pages, API keys) belongs to the same org via `org_id` (master section 13 tenant isolation).

### 2.4 Invoice (phased, Phase 2)

A billing record owned by Paddle. Pulse stores a lightweight reference (Paddle invoice id, amount, period, status, hosted PDF URL) for display in the billing UI (master section 10 screen 12). Pulse does not compute invoice line items; it reads them from Paddle. Before Phase 2 there are no invoices (Free and internally-set plans only).

### 2.5 Relationship to Organization

```
Organization 1..1 Subscription 1..1 Plan (by reference to catalog)
Organization 1..1 Entitlement (effective limits, defaulted from Plan, override-capable)
Organization 1..* Invoice (Phase 2, mirrored from Paddle)
Organization 1..* Monitor / Membership / Invitation / StatusPage / ApiKey  (the gated resources)
```

The Subscription and Entitlement are existential to the org and created at signup on the Free plan (master section 3: signup creates a personal org on Free, owner in seat 1).

---

## 3. Plan tier matrix

This is the full table of every metered or gated dimension per tier. It mirrors the public pricing page (`docs-site/pricing.html`), which is the source of truth. Public names are Free / Hobby / Professional / Custom; the internal plan codes stay `tier1` / `tier2` / `tier3` / `tierCustom` (RFC-017), shown in parentheses. Custom is the contract-negotiated tier (SSO, residency, SLA, custom limits); the entitlement code carries generous defaults that a contract overrides.

| Dimension | Free (`tier1`) | Hobby (`tier2`) | Professional (`tier3`) | Custom (`tierCustom`) |
|-----------|------|----------------|-------------|-----------------|
| **Price** | $0 | $7/mo | $19/mo | From $129/mo |
| **Monitors (cap)** | 10 | 25 | 50 | Custom (large default) |
| **Per-monitor add-on beyond cap** | N (hard cap) | Y (per-API price) | Y (per-API price) | Custom |
| **Min check interval (frequency floor)** | 15 min | 5 min | 1 min | Custom (down to 30 s) |
| **Hard interval floor (master 6.2)** | 30 s never go below | 30 s | 30 s | 30 s |
| **Regions allowed (count per monitor)** | 1 (single region) | 1 (single region) | up to 4 | all + residency |
| **Premium regions** | N | N | N | Y |
| **Seats included** | 1 | 3 | 10 | Unlimited |
| **Per-seat add-on beyond included** | N (cap at 1) | Y (per-seat price) | Y (per-seat price) | Custom |
| **History retention (raw results)** | 7 days | 30 days | 90 days | 180 days |
| **Status pages (cap)** | 1 (Pulse subdomain) | 3 | 10 | Custom (large default) |
| **Custom domain status page** | N | N | Y | Y |
| **API access** | None (no keys) | Read-only | Full | Full |
| **API rate limit (per key)** | n/a | 120 req/min | 300 req/min | 600 req/min |
| **Outbound org-level webhooks** | N | N | Y | Y |
| **Channel types** | Discord, BYO-SMTP, Telegram | + Slack, platform Email | + Webhook, PagerDuty/Opsgenie, Teams (phased) | All + SMS |
| **Audit-log retention** | N | N | 30 days | 1 year |
| **SSO (SAML / Okta)** | N | N | N | Y |
| **Failed-check response capture (PRD-002 3.8)** | Y | Y | Y | Y (on for all tiers now; gateable per plan later, GTM-tunable) |
| **Support** | Community | Email | Priority email | Priority + SLA |

Notes anchored to the master:

- Retention values match the master's data-retention tiers exactly (master section 12: 7 / 30 / 90 / 180 days). Raw results age out by background cleanup; incidents and monitor config persist for the life of the org.
- API rate-limit numbers are illustrative (master section 9 says limits scale by plan; section 11 names the tiers Read-only-low / standard / higher / highest). The req/min values above are concrete defaults to build and price against, tunable at GTM.
- Free API access is **read-only** (master section 11 table). A Free key cannot create or mutate via the API.
- Channel types are gated by tier (RFC-019): Free gets Discord, bring-your-own SMTP, and Telegram; Slack and the platform Email channel start at Hobby; the generic webhook starts at Professional; SMS (Twilio) is Custom-only. The on-call/incident tools (PagerDuty, Opsgenie, Teams) are phased and land at Professional and above.
- **Enterprise (phased, master section 11 / 15 Phase 3)** adds SSO/SCIM, custom roles, higher limits, more and premium regions with regional data residency, contractual SLA, and invoicing. Not specified in this matrix; it is contract-negotiated.

---

## 4. Metering dimensions and the pricing model

### 4.1 The model

Tier package (a base price per tier) sets the included allowances and the frequency floor. On top of that, two growth axes scale the bill: **per-seat** and **per-API (per monitor)**. **Region availability** is a tier entitlement and a cost dimension, not a separately metered counter in v1. We do not meter checks executed.

| Dimension | What it is | How it is counted | Pricing role |
|-----------|-----------|-------------------|--------------|
| **Monitors** | enabled monitored endpoints in the org | count of monitors with `enabled = true` against `monitors_cap`; the cap is the gate, add-ons (paid tiers) raise effective cap | per-API value meter (primary growth axis) |
| **Min check interval** | the frequency floor the org may set | not a counter; a floor entitlement enforced on write and on dispatch (section 5) | tier package (faster = higher tier) |
| **Seats** | paid team capacity | **accepted members + reserved pending invites** (master section 3, section 16 decision 1) against `seats_included + seats_purchased` | per-seat value meter (second growth axis) |
| **History retention** | days of raw results kept | tier entitlement; cleanup job deletes beyond `retention_days` (master section 12) | tier package (storage cost) |
| **Regions** | which/how-many regions a monitor may run from | tier entitlement: `regions_allowed` set and `regions_per_monitor_cap`; premium regions are a tier flag | tier entitlement + COGS (master section 11 region cost) |
| **Status pages** | published/draft pages in the org | count against `status_pages_cap`; custom domain is a separate feature flag | tier feature meter |
| **API rate limit** | requests/min per key | not billed per request; a per-key ceiling set from the tier (master section 9) | tier feature |

### 4.2 How seats are counted (precise)

Seats used = **accepted memberships + pending invitations that are still live** (state `pending`, not expired/revoked). This is the master's locked decision (section 3, section 16 decision 1: pending invitations reserve a seat). Effects:

- Inviting a member reserves a seat at invite time, subject to the cap (master section 3 invitation flow). Over-inviting past the cap is blocked with an upsell (section 5.1).
- Revoking or letting an invite expire frees the reserved seat (master section 3).
- Removing an accepted member frees a seat (section 10 edge case).
- The org always holds at least one seat: the owner in seat 1 (master section 3). Free is capped at 1 seat, so a Free org is single-person until it upgrades.

### 4.3 How monitors are counted

Monitors used = count of monitors with `enabled = true`. Disabled monitors do not count against the cap (this is what makes "disable to get under the limit" a valid downgrade path, section 6). The cap is on monitors, not on monitor-region combinations; region count is gated separately per monitor by `regions_per_monitor_cap`.

### 4.4 Why not per-check, and the trade-off

Per-check pricing (the model that makes Pingdom expensive, master section 1) punishes the exact growth we want to encourage: adding monitors and checking them often. It is also unpredictable for the customer, who cannot forecast a check counter. Pricing on monitors plus a frequency tier is predictable (a customer can read the pricing page and compute their bill) and still tracks our cost, because faster intervals sit in higher tiers and more monitors cost more.

The trade-off (stated honestly, master section 11): a customer with few monitors at a very high frequency is cheaper for us than the model implies, and a customer at the slow end of a tier subsidizes nobody. The frequency tier (the min-interval floor per tier) covers the high-frequency case: you cannot get 1-minute checks without being on Professional (or Custom). Region COGS is handled by tier (premium regions are Custom-only) and by cost-aware scheduling (low tiers default to the cheaper home region, master section 11 region cost), so multi-region traffic does not erode margin on low tiers.

---

## 5. Entitlement enforcement (the cross-cutting heart)

Enforcement happens in more than one place on purpose, so a downgrade cannot be worked around (master section 11). The api enforces on write. The scheduler enforces on every dispatch. Neither trusts the other.

### 5.1 At the api, on write

Every write that touches a metered limit is checked against the org's cached entitlement before it is accepted. On a violation, the api returns the standard per-field error envelope (`code` / `message` / `fields`, master section 9 / appendix A) carrying an upsell hint, so the UI can render an upgrade prompt inline.

| Write action | Entitlement checked | Behavior on violation |
|--------------|---------------------|-----------------------|
| Create monitor | `monitors_cap` (enabled count) | Reject with `code: monitor_limit_reached`, upgrade prompt. On Free (cap 10), the 11th enabled monitor is blocked. |
| Create/update monitor `interval_seconds` | `min_interval_seconds` floor for the tier | Reject as a **per-field** error on `interval_seconds` (`code: interval_below_plan_floor`), stating the tier floor. Below the 30 s hard floor is also rejected (master 6.2). |
| Create/update monitor `regions` | `regions_allowed` set and `regions_per_monitor_cap` | Reject per-field on `regions` (`code: region_not_in_plan` or `region_count_exceeded`); picking a premium region on a non-premium tier is rejected. (Master appendix A region rules.) |
| Invite member | seats: accepted + pending vs `seats_included + seats_purchased` | Block invite with `code: seat_limit_reached`, upsell. On Free (1 seat, owner holds it) any invite is blocked until upgrade. |
| Create status page | `status_pages_cap` | Reject with `code: status_page_limit_reached`, upsell. |
| Set custom domain on status page | `custom_domain` flag | Reject if not entitled; owner/admin only (master section 4). |
| Create API key / use API | `api_access` (none/read/full) and `api_rate_per_min` | Free keys are read-only; write calls return 403 with an upsell. Per-key rate set from `api_rate_per_min`; over-rate returns 429 with `Retry-After` (master section 9). |

The interval-floor and region rules extend the per-field validation already specified in master appendix A (which says "plan tier may raise the effective floor" and "each region must be a region the org's plan includes"). This PRD names the error codes; the validation point is the one in appendix A.

### 5.2 At the scheduler, on dispatch (independent)

The scheduler owns the schedule for all orgs' monitors (master section 6.6) and must independently respect the org's interval floor and region entitlement when it dispatches checks. This is the no-bypass guarantee: a monitor created under Professional at a 1-minute interval across 4 regions cannot keep running that way after the org downgrades to Free, even though the stored monitor config still says 60 seconds and 4 regions.

- **Interval floor on dispatch.** The scheduler computes the effective interval as `max(monitor.interval_seconds, entitlement.min_interval_seconds)`. After a downgrade to Free, a stored 60 s monitor is dispatched no faster than every 15 minutes. The stored value is not mutated; the floor is applied at dispatch so an upgrade restores the faster cadence without re-editing the monitor.
- **Region entitlement on dispatch.** The scheduler enqueues a check job per selected region (master 6.7), but only for regions in the current `regions_allowed` set, and no more than `regions_per_monitor_cap`. A region dropped by a downgrade is simply not dispatched. (Premium regions stop dispatching when the org leaves a premium tier.)
- This is why downgrade does not need to rewrite every monitor: the scheduler clamps to the live entitlement on every tick. The owner is still prompted to bring usage under the limit for the things that are not silently clampable (monitor count, seats), section 6.

### 5.3 Caching (the hot path)

The scheduler runs at ~10k checks/sec sustained (master section 12) and the api serves reads at p99 < 300 ms (master section 12). Neither can pay a database read per check or per request for entitlements. So:

- The org's effective entitlement set (section 2.2) is cached in **Redis**, keyed by `org_id` (master section 11; RFC-009 owns the mechanism).
- The scheduler and api read the entitlement from cache on the hot path.
- **Invalidation follows plan changes.** On any change to an org's plan or purchased capacity (upgrade, downgrade, Paddle webhook updating subscription state, support override), the cache entry is invalidated so the new limits take effect promptly. A stale cache must fail safe toward the lower limit on ambiguity rather than grant more than entitled.

---

## 6. Plan changes

### 6.1 Upgrade (immediate)

An upgrade takes effect immediately. Higher caps, faster interval floor, more regions, more seats, longer retention, and richer features become available as soon as the new plan is recorded and the entitlement cache is invalidated (section 5.3). In Phase 2, Paddle collects payment and proration for the remainder of the cycle is Paddle-handled (section 8). In Phase 1, an operator sets the plan internally and the upgrade is immediate with no payment.

No data migration is needed on upgrade: the scheduler simply stops clamping (section 5.2), so monitors that were running at the floor now run at their stored interval and previously-dropped regions resume dispatching.

### 6.2 Downgrade (bring usage under the limit first)

A downgrade that would exceed the lower plan's limits does **not** silently delete data (master section 11). Instead the owner is prompted to bring usage under the new limits before the downgrade is applied. The blocking conditions and the required action:

| Over-limit condition after downgrade | Required action before downgrade applies |
|--------------------------------------|------------------------------------------|
| Enabled monitors > new `monitors_cap` | Disable or delete monitors until under the cap (disabled monitors do not count, section 4.3) |
| Seats used > new `seats_included` (and tier has no add-on, e.g. Free = 1) | Remove members / revoke pending invites until seats fit |
| A monitor uses regions not in the new plan, or more than the new per-monitor cap | Drop the extra/premium regions from those monitors |
| Status pages > new `status_pages_cap` | Delete or unpublish pages until under the cap |
| Custom-domain page on a tier that loses custom domain | Remove the custom domain (page stays on the Pulse subdomain) |

What is **clampable vs blocking**: interval floor and region set are clampable by the scheduler (section 5.2), so a downgrade does not need the owner to slow down or shrink regions to proceed; the scheduler enforces it automatically and the stored config is preserved for a future upgrade. Monitor count, seats, and status-page count are **not** clampable without choosing what to drop, so those are blocking and the owner makes the choice. This split is deliberate: we never pick which monitor or which teammate to remove on the customer's behalf.

The billing UI presents this as a checklist ("to switch to Hobby, bring usage under these limits") and the downgrade button stays disabled until the org is within the target plan. This is testable (section 10).

### 6.3 Proration

Proration is **Paddle-handled and phased** (Phase 2, master section 11). When a customer changes plan or seat count mid-cycle, Paddle computes the prorated charge or credit for the remainder of the billing period; Pulse does not compute proration itself. Before Phase 2 there is no proration because there is no payment (Free and internally-set plans only).

---

## 7. Usage & limits UI

The billing/usage screen (master section 10 screen 12) and upgrade prompts throughout the app make limits visible. Owner manages; admin views (master section 4: "View billing and usage" = owner+admin; "Manage billing" = owner only).

### 7.1 Billing / usage screen

- **Current plan**: tier name, billing cycle (Phase 2), renewal date (Phase 2), plan-change entry point (owner only).
- **Seats**: used / available, with a breakdown of accepted members and reserved pending invites (section 4.2). At-limit state surfaces "buy more seats" (paid tiers) or "upgrade for more seats" (Free).
- **Monitors**: enabled count vs cap, as a usage meter. At-limit state surfaces the upgrade prompt.
- **Frequency tier**: the current min-interval floor (e.g. "checks as fast as every 1 min" on Professional).
- **Retention**: current retention window (e.g. "90 days of history").
- **Regions**: which regions are available and the per-monitor region cap; premium-region availability.
- **Usage meters**: monitors, seats, status pages each shown as used/cap with an at-limit visual (master section 10 screen 12: "usage meters and overage state").
- **Overage / at-limit state**: since v1 is a hard block (section 11 open decisions), there is no overage charge; the at-limit state shows "you've reached your plan limit" with the relevant upgrade prompt rather than a running overage cost.
- **Invoices** (Phase 2): list of Paddle invoices with amount, period, status, hosted PDF link. Owner manages payment method; admin views.

### 7.2 Upgrade prompts throughout the app

The prompt appears at the point of friction, not only on the billing screen:

- Monitor create form / list: when at the monitor cap, "Upgrade to add more monitors."
- Monitor form `interval_seconds`: the per-field error states the tier floor and links to upgrade.
- Monitor form `regions`: regions outside the plan are shown locked with an upgrade hint.
- Members screen: when at the seat cap, the invite action shows "Upgrade or buy a seat."
- Status page editor: at the page cap, or when custom domain is locked, an upgrade hint.
- API responses carry the upsell `code` so a client (and the docs/Swagger UI) can surface it.

Every upgrade prompt deep-links to the billing screen with the relevant plan pre-selected where possible. This is the conversion mechanism the master tracks (master section 14: conversion trigger reasons = monitor cap, seats, custom domain, frequency).

---

## 8. Paddle integration (phased, Phase 2)

Paddle is the Merchant of Record, so it is the seller of record and also handles sales tax/VAT on top of payments. The subscription columns Pulse stores are provider-agnostic, so the provider is not baked into the data model.

### 8.1 What Paddle owns

- **Payment**: card capture, PCI scope, charging.
- **Plan management / catalog pricing**: prices for the base package, the per-seat add-on, and the per-monitor add-on live as Paddle prices; plan changes go through Paddle.
- **Invoices**: generation, hosting, PDFs. Pulse mirrors a reference (section 2.4).
- **Proration**: mid-cycle plan/seat changes (section 6.3).
- **Dunning**: retrying failed payments and the failure schedule.
- **Subscription-state webhooks**: Paddle is the source of truth for subscription status; it notifies Pulse via webhook on `created` / `updated` / `payment_failed` / `canceled` / `period renewed`, and Pulse updates its Subscription record and invalidates the entitlement cache (section 5.3).

### 8.2 What Pulse owns

- **Entitlement enforcement.** This is Pulse's job, always, regardless of Paddle. Paddle says what plan the org is on; Pulse turns that into the entitlement set and enforces it at the api and scheduler (section 5). Paddle never enforces a monitor cap or an interval floor; that is product behavior in Pulse.
- The Plan-to-entitlement mapping (section 2.1, 2.2), the usage counting (section 4), the upgrade/downgrade flows (section 6), and the UI (section 7).

### 8.3 Before Paddle (Phase 1)

Plans are set internally by an operator. Free is the default at signup (master section 3). Limits are enforced identically; the only thing missing is payment, invoices, and proration. This is the master's explicit position (section 11): the model and enforcement do not wait for Paddle.

### 8.4 Free tier and trials

- **Free tier requires no card.** Signup lands on Free with no payment step (master section 3, section 10 onboarding: sign-in to first monitor in under two minutes, no billing friction). A Free org stays Free indefinitely with the Free entitlement.
- **Trial recommendation**: offer a **short trial of a paid tier, no card required up front**, started from the billing screen. The length is driven by `plan_prices.trial_days`: **3 days on monthly, 7 days on annual**. During the trial the org's entitlement is the trialed tier; on trial end without payment, the org drops to Free and the downgrade rules (section 6.2) apply (the owner is prompted to bring usage under Free before the drop completes, so nothing is silently deleted). A no-card trial maximizes activation while the no-silent-delete rule protects their data. A separate anti-abuse window (a 35-day re-trial deny anchored on `subscriptions.ended_at`) stops repeated trials; that is distinct from the trial length. This is an open decision with this as the recommended default (section 11).

---

## 9. RBAC

Reuses the master permission matrix (master section 4); this domain does not add roles.

- **Owner manages billing**: plan changes, payment method, invoices, buying seats, starting/ending a trial, applying a downgrade. (Master section 4: "Manage billing (plan, payment, invoices)" = owner only.)
- **Admin views billing and usage** but cannot change the plan or payment. (Master section 4: "View billing and usage" = owner + admin.)
- **Member and viewer** have no billing access. They still feel entitlements (a member creating the 3rd monitor on Free is blocked, since members are the operators, master section 4), but they cannot resolve it; the prompt tells them to ask an owner to upgrade.
- **API keys never reach owner** (master section 5, section 16 decision 5). Keys max out at admin, and billing management is owner-only, so **billing cannot be scripted or automated by a leaked key**. Plan changes, seat purchases, and org deletion stay deliberate human actions in the UI. Billing-management endpoints require owner, which no key holds, so they are UI-only by design (master section 9).

---

## 10. User stories, acceptance criteria, edge cases

### 10.1 User stories

- As a **solo dev on Free**, I want to know the moment I hit the monitor cap so I understand what upgrading buys me.
- As a **Team owner**, I want to invite teammates up to my seat count and buy more seats when I run out, without surprise charges.
- As an **owner downgrading**, I want to be told what to trim first so I never lose a monitor or a teammate by accident.
- As an **SRE managing monitors via the API**, I want a clear `code` when I hit a plan limit so my tooling can react.
- As **platform operations (Phase 1)**, I want to set an org's plan internally and have limits enforced immediately, before Paddle exists.

### 10.2 Acceptance criteria (testable)

1. **Over-cap monitor on Free is blocked.** Free cap is 10. Creating an 11th enabled monitor on a Free org is rejected with `code: monitor_limit_reached` and an upgrade prompt; the monitor is not created.
2. **Interval below the plan floor is rejected.** On Free (floor 15 min), setting `interval_seconds` below 900 s is rejected as a per-field error on `interval_seconds` (`code: interval_below_plan_floor`) stating the floor. On any tier, below the 30 s hard floor is rejected (master 6.2 / appendix A).
3. **Invite over seats is blocked.** On Free (1 seat, owner holds it), any invite is blocked with `code: seat_limit_reached`. Reserved pending invites count toward seats (section 4.2), so the second pending invite on a 1-extra-seat plan is also blocked.
4. **Downgrade requires bringing usage under the limit.** An org with 20 enabled monitors trying to downgrade to Free (cap 10) cannot complete the downgrade until enabled monitors are <= 10; the downgrade action stays disabled and lists what to trim (section 6.2). No monitor is deleted automatically.
5. **Scheduler refuses a sub-floor interval after downgrade.** A monitor stored at 60 s on an org downgraded to Free is dispatched no more often than every 15 min; the scheduler clamps to the entitlement floor on dispatch (section 5.2), independent of the stored value and independent of the api.
6. **Scheduler refuses a de-entitled region after downgrade.** A monitor configured for 4 regions on an org downgraded to a 1-region plan is dispatched only to the home region; the other 3 region jobs are not enqueued (section 5.2).
7. **Entitlement change takes effect promptly.** After a plan change, the Redis entitlement cache is invalidated and the next write check and next dispatch use the new limits (section 5.3).
8. **Free needs no card; API on Free is read-only.** Signup reaches a working monitor with no payment step; a Free API key can read but a write call returns 403 with an upsell (section 3, section 8.4).

### 10.3 Edge cases

- **Mid-cycle plan change.** Upgrade is immediate; Paddle prorates the remainder (section 6.1, 6.3). Downgrade mid-cycle still requires bringing usage under the limit first (section 6.2); proration credit is Paddle-handled.
- **Payment failure / dunning (Phase 2).** Paddle retries on its schedule. While `past_due`, Pulse keeps monitoring running (we do not want a billing hiccup to stop alerting a paying customer mid-incident) and shows a prominent "update payment" banner to owner/admin. If dunning fails out and the subscription cancels, the org drops to Free and the downgrade rules apply (the owner is prompted to bring usage under Free; nothing is silently deleted, section 6.2). What degrades on `canceled`: caps drop to Free, fast intervals clamp to the Free floor at the scheduler, extra regions stop dispatching, API drops to read-only. What does not degrade during `past_due` (before cancel): nothing; monitoring continues so a payment glitch never blinds a customer.
- **Seat freed by removing a member.** Removing an accepted member (owner/admin, master section 4) frees a seat immediately; revoking or expiring a pending invite also frees its reserved seat (section 4.2). The freed seat is available for a new invite without a plan change.
- **Last owner.** Billing actions cannot strand an org without an owner; the last owner cannot be removed or demoted (master section 4), so there is always someone who can manage billing.
- **Disabled monitors and the cap.** Disabling a monitor frees cap immediately (section 4.3), which is the supported way to get under a lower cap on downgrade without deleting.

---

## 11. Open decisions (with recommended defaults)

1. **Do pending invitations hold a seat?** Decided in the master: **yes, reserve a seat** (master section 3, section 16 decision 1). Carried here as locked; seats = accepted + pending (section 4.2).
2. **Overage handling: hard block vs soft overage.** Recommended: **hard block with an upgrade prompt for v1.** Rejecting the over-cap action is predictable and matches the pricing wedge (no surprise bills); soft overage (allow over the cap and bill the difference) is more forgiving but reintroduces bill unpredictability we are selling against. Trade-off: a customer at exactly the cap hits a wall instead of growing seamlessly; mitigated by clear at-limit meters and one-click upgrade (section 7). Revisit soft overage for higher tiers post-GA.
3. **Annual vs monthly billing.** Recommended: **offer both, default the pricing page to monthly, with an annual option at a discount (around two months free).** Monthly lowers the entry barrier for the price-sensitive Dev persona; annual improves retention and cash for the Team persona. Phase 2 (Paddle handles both cycles). `billing_cycle` is on the Subscription (section 2.3).
4. **Trial length and card requirement.** Recommended: **short paid-tier trial (3 days monthly, 7 days annual, from `plan_prices.trial_days`), no card up front**, dropping to Free on expiry under the no-silent-delete rule (section 8.4). A 35-day re-trial deny window (anchored on `subscriptions.ended_at`) stops repeat trials. Trade-off: no-card trials attract some tire-kickers, but the activation lift is worth it, and the Free fallback is safe.
5. **Per-monitor and per-seat add-on pricing model.** Recommended: **linear per-unit add-on above the included allowance** (each extra monitor and each extra seat at a flat price), not bracketed bundles, because linear is the most predictable and the easiest to display on the pricing page. GTM-tunable. Free has no add-ons (hard caps).

---

## 12. Dependencies

| Depends on | What this PRD needs from it |
|------------|-----------------------------|
| **PRD-001 Identity & Tenancy** | Org, membership, seat, and invitation model; the seat-counting rule (accepted + pending); the RBAC matrix (owner manages billing, admin views, member/viewer none). Section 4.2, section 9. |
| **PRD-002 Monitoring Engine** | The monitor entity, the `interval_seconds` field and 30 s hard floor, `enabled` flag (for cap counting), and the per-field validation envelope the interval/region errors extend. Section 5.1, appendix A reference. |
| **PRD-007 Multi-Region** | The `regions` set, `regions_per_monitor_cap`, premium-region designation, region COGS, and the scheduler's per-region dispatch that this PRD clamps to the entitlement. Section 4.1, 5.2, region cost (master 11). |
| **PRD-005 Public API & Webhooks** | The per-key rate-limit tiers, the read-only-vs-full API access gate, outbound-webhook entitlement, the standard error envelope and upsell `code`, and the owner-only/UI-only billing endpoints. Section 5.1, 7.2, 9. |
| **PRD-004 Status Pages** | The status-page entity, the page-count and custom-domain gates. Section 3, 5.1, 6.2. |
| **PRD-003 Notifications** | The channel-type entitlement (phased channels gated by tier). Section 3. |

Architecture realization is owned by **RFC-009 Entitlements Enforcement** (limits model, enforcement points, Redis caching), with the scheduler floor/region clamp in **RFC-004 Scheduler** and the API rate tiers in the API RFC (`PLANNING.md`).

---

## Appendix - Error codes (entitlement violations)

Returned in the standard envelope (`code` / `message` / `fields`, master section 9 / appendix A), each carrying an upsell hint for the UI.

| `code` | Field | When |
|--------|-------|------|
| `monitor_limit_reached` | (resource-level) | creating an enabled monitor over `monitors_cap` |
| `interval_below_plan_floor` | `interval_seconds` | interval below the tier's `min_interval_seconds` |
| `interval_below_hard_floor` | `interval_seconds` | interval below the 30 s hard floor (master 6.2) |
| `region_not_in_plan` | `regions` | a selected region is not in `regions_allowed` (incl. premium on a non-premium tier) |
| `region_count_exceeded` | `regions` | more regions than `regions_per_monitor_cap` |
| `seat_limit_reached` | (resource-level) | invite would exceed seats (accepted + pending) |
| `status_page_limit_reached` | (resource-level) | creating a page over `status_pages_cap` |
| `custom_domain_not_in_plan` | `custom_domain` | setting a custom domain without the entitlement |
| `api_write_not_in_plan` | (resource-level) | a write via a read-only (Free) API key |
| `api_rate_limited` | (resource-level) | per-key rate exceeded (429 with `Retry-After`) |
