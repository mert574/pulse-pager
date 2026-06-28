# RFC-018 - Billing provider & payments

Parent: `PRD-006-billing-and-entitlements.md` (this is its Phase 2). Master: `PRD.md` section 11.

Status: Phase 1 built. The Paddle provider (`internal/billing`, `internal/billing/paddle`), the self-serve checkout and trial gate (`internal/api/billing_selfserve.go`), and the `subscriptions` / `payments` / `plan_prices` migrations are in. Trial is 3 days (7 annual) via `plan_prices.trial_days`, with a 35-day re-trial deny window anchored on `subscriptions.ended_at`. Later phases stay future.

## 1. Summary

Wire real recurring payments onto the existing entitlement model. Pulse already
enforces caps/floors server-side with operator-set plans (PRD-006 Phase 1, shipped:
`internal/entitlements`, `AdminUpdateOrgPlan`). This RFC adds the money: a billing
provider behind one interface, a webhook sync path, self-serve checkout, and the
operator controls the owner asked for (move any org's plan with no customer action,
cancel any subscription, refund any payment).

Pulse keeps owning entitlement enforcement only. The provider owns money movement,
tax, invoices, proration, and dunning (PRD-006 section 8: "do not build a billing
engine").

## 2. Provider decision

Use a **Merchant of Record (Paddle)**, not raw Stripe. A MoR is the legal seller, so
it calculates, collects, and remits VAT/GST/sales tax for us; that removes the single
biggest compliance burden of selling globally. Paddle (over Lemon Squeezy) because its
Billing API cleanly supports the operator-driven operations this RFC requires
(subscription price changes, cancellation, refunds via API).

The integration is provider-abstracted (a `billing.Provider` interface, same pattern
as the `PULSE_BUS` seam), so a future swap to raw Stripe is a new adapter, not a
redesign. The only delta with raw Stripe is that we would owe tax ourselves (Stripe
Tax helps calculate, but we remain merchant of record).

Not a lawyer's sign-off: tax registration thresholds and consumer law still need real
advice; the MoR choice is specifically to minimize that surface.

## 3. Architecture

- `internal/billing` - provider-agnostic core + adapters (`paddle`, later `stripe`):
  ```
  type Provider interface {
    Checkout(ctx, org, plan, cycle) (url string, err error)
    PortalURL(ctx, org) (string, error)
    UpdateSubscription(ctx, sub, target PlanChange) error   // operator/self-serve plan move
    CancelSubscription(ctx, sub, when CancelWhen) error      // immediate | period_end
    Refund(ctx, paymentID string, amount *Money, reason string) error
    SetCustomPrice(ctx, org, amount Money, cycle) (priceRef string, err error)  // per-org, section 7
    VerifyWebhook(payload []byte, sig string) (Event, error)
  }
  ```
  The rest of the app depends only on the interface.
- **Webhook ingest** - one hand-wired, signature-verified endpoint (outside the
  generated JSON contract, like `/auth/*`). It is the authoritative sync path: both
  self-serve and operator actions converge here, so subscription state cannot drift.
  Idempotent on the provider event id.
- **Entitlements** - unchanged enforcement. Effective plan resolves from the org's
  subscription (or an operator override); the resolvers already read `plan`.

## 4. Data model (goose migrations)

- `subscriptions` (one per org, RLS-scoped): `org_id`, `plan`, `billing_cycle
  (monthly|annual)`, `status (trialing|active|past_due|canceled)`, `provider`,
  `provider_customer_id`, `provider_subscription_id`, `provider_price_id`,
  `current_period_end`, `cancel_at_period_end`, timestamps. (PRD-006 2.3 already
  reserves these.)
- `payments` (read-only mirror for the refund UI): `org_id`, `provider_payment_id`,
  `amount`, `currency`, `status`, `period`, `hosted_invoice_url`, `refunded_amount`.
- `billing_events` (webhook idempotency): unique `provider_event_id`, `type`,
  `received_at`, `processed_at`.
- `plan_prices` (catalog -> provider price ids): `plan`, `cycle` -> `provider_price_id`.
  Custom is NOT in this table (section 7).
- Effective plan = operator override (comp/grandfather, nullable) ?? subscription.plan
  ?? `tier1`. The override lets an operator grant a plan with no provider charge.

## 5. Operator controls (the owner's requirements)

These live in the platform admin panel (superadmin, separate from org RBAC), every
action written to `audit.events`, behind a confirm dialog, with provider-side
idempotency keys so a retry never double-acts.

### 5.1 Move any org's plan, customer does nothing
`POST /admin/orgs/{orgId}/plan` already exists and is wired into the admin panel: it
sets the org's plan directly (operator override, no provider). Phase 2 only adds the
provider call: when the org has an active paid subscription, also call
`Provider.UpdateSubscription` to switch the price server-side (the customer is never
prompted) and let the webhook reconcile; orgs with no subscription keep working as
they do today. A `mode` (`prorate_now` | `next_cycle`) is added for the paid case.

### 5.2 Cancel any subscription
`POST /admin/orgs/{id}/subscription/cancel {when: immediate|period_end}` ->
`Provider.CancelSubscription` -> webhook -> org drops to Free (now or at period end).
Default `period_end`.

### 5.3 Refund any payment
`POST /admin/orgs/{id}/refund {payment_id, amount?}` -> `Provider.Refund` (full or
partial) -> mirror updates `payments.refunded_amount`. With a MoR the refund also
reverses tax automatically. Refund is irreversible: treat like a destructive,
confirm-required action.

## 6. Self-serve (Phase 3 of this RFC)

Hosted Checkout to buy and the provider Customer Portal to manage card / cancel /
self-upgrade. Two thin endpoints (`Checkout`, `PortalURL`); almost no custom UI. The
monthly/annual toggle is just two prices per tier in `plan_prices`.

## 7. The Custom plan (tierCustom): per-org price, negotiated org by org

Custom is the contract-negotiated tier (SSO, residency, SLA, bespoke limits). Unlike
Free/Hobby/Professional, it has **no fixed catalog price** - the amount is negotiated
per customer, so it differs org to org. It is never self-serve; the pricing page shows
"Contact us".

Two things are per-org for a Custom org, and both are set by the operator:

1. **A bespoke recurring price.** We do not use `plan_prices` for Custom. Instead the
   operator enters the negotiated amount + cycle for that one org, and the backend
   creates a per-org price in the provider:
   - Paddle: a non-catalog / custom price attached to that org's subscription.
   - Stripe (if ever used): an ad-hoc `price_data` amount, or a dedicated Price per
     customer.
   The resulting `provider_price_id` is stored on that org's `subscriptions` row
   (which is why `provider_price_id` is per-subscription, not read from the shared
   catalog). So each Custom org points at its own price; the amounts are independent
   org to org.

2. **Bespoke entitlement caps.** `tierCustom` carries generous code defaults, but the
   real limits are the per-org entitlement override that PRD-006 2.2 already allows
   (monitors_cap, regions, retention, seats, etc. stored per org). Moving an org to
   Custom sets both the price and these caps.

Operator flow:
- `POST /admin/orgs/{id}/plan {plan: "tierCustom", cycle, custom_amount, custom_caps}`.
- Backend: `Provider.SetCustomPrice(org, amount, cycle)` -> get a per-org `priceRef`
  -> start or `UpdateSubscription` to that price -> persist the override caps.
- **Renegotiation / changing the amount later**: same endpoint with a new
  `custom_amount`; the adapter creates the new per-org price and swaps the
  subscription onto it (proration per `mode`). The price history stays in the provider.
- **Moving off Custom** (back to a catalog tier): `UpdateSubscription` to the catalog
  `provider_price_id` from `plan_prices`, and clear the override caps.
- Comp / $0 Custom (design partner): use the operator override path with no provider
  price at all - Custom caps, no subscription, no charge.

Open question for Custom: do we keep a small internal record of the negotiated amount
per org for display in the admin panel, or always read it back from the provider? Lean
toward reading from the provider (single source of truth for money) plus the mirror in
`payments`.

## 8. Security & correctness

- Webhook signature verified per provider; events deduped via `billing_events`
  (at-least-once, like the bus).
- All admin billing actions: superadmin-only, audited, confirm-required, idempotent.
- Money is never computed in Pulse; we mirror provider numbers only.
- A downgrade that would exceed the lower plan's caps follows PRD-006's rule: do not
  destroy data; block new resources and surface "bring usage under the limit" rather
  than deleting monitors/members. (Open decision: block the downgrade outright vs
  allow + soft-disable the excess.)

## 9. Phasing

1. Foundation: schema + `billing.Provider` + Paddle adapter + webhook ingest (sync
   path only; no UI). Subscription state flows provider -> DB -> entitlements.
2. Operator controls (section 5 + the Custom flow in section 7) in the admin panel.
3. Self-serve checkout + Customer Portal (section 6).
4. Invoice/payment mirror + billing-screen polish.

## 10. Open decisions

1. Downgrade-over-cap: hard block vs allow + soft-disable excess.
2. Default cancel timing: recommend `period_end`.
3. Operator override lifetime: permanent-until-changed vs expiring grant.
4. Custom amount of record: read from provider vs also store internally (section 7).
