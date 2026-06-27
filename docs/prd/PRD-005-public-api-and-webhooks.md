# PRD-005 - Public API & Webhooks

Status: Draft
Owner: Product (Principal PM)
Parent: `PRD.md` (master, v2.1)
Domain: the public REST API, API keys, OpenAPI/Swagger UI, the documentation and pricing site, and outbound org-level webhooks.
Related sub-PRDs: PRD-001 Identity & Tenancy (keys, roles), PRD-002 Monitoring Engine (monitor/incident surface), PRD-003 Notifications (per-monitor channel vs org webhook), PRD-004 Status Pages, PRD-006 Billing & Entitlements (rate tiers, read-only free), PRD-007 Multi-Region (region selection, results, catalog).

This sub-PRD derives from master sections 5 (API keys), 9 (public API + webhooks), and 10 (docs surfaces). Where the master locks a decision, this document restates it and does not contradict it. Section numbers below in the form "master 9" point at `PRD.md`.

---

## 1. Overview, goals, non-goals

### 1.1 Overview

A first-class, documented REST API is core to Pulse's wedge: developer-first, API-first, where competitors treat the API as an afterthought (master 1, 9). This domain covers four product surfaces that hang together:

1. The public REST API at `/api/v1`, authenticated by per-org role-scoped API keys (master 5, 9).
2. The OpenAPI 3 spec as the single source of truth, served interactively as Swagger UI at `/api/docs` (master 9, 10).
3. The public documentation site and pricing page on GitHub Pages, kept in sync by CI from the same spec (master 10).
4. Outbound org-level webhooks so a team can ingest Pulse events into their own pipeline (master 9), distinct from the per-monitor generic-webhook channel in PRD-003.

This surface ships at GA (master 15, Phase 2), after identity, RBAC, and the monitoring loop are in place.

### 1.2 Goals

| Goal | Why it matters |
|------|----------------|
| Anything a member or admin can do in the UI, they can script through the API | The API-first wedge (master 1); SRE persona manages hundreds of monitors as code (master 2 persona C) |
| Zero drift between the live API and its docs | OpenAPI is the source of truth; docs regenerate from it on release (master 9, 10) |
| Time-to-first-API-call in minutes | Create a key, copy the curl from Swagger UI, run it against your own org |
| Predictable, plan-tiered limits a developer can reason about | Rate-limit headers, 429 with Retry-After, limits scale by tier (master 9, 11) |
| Events pushed to the customer, no polling | Outbound webhooks with signing and dedup (master 9) |

### 1.3 Non-goals

- No owner-equivalent API keys. Billing, ownership transfer, and org deletion stay UI-only by design so a leaked key cannot automate them (master 5, master 16 decision 5).
- No per-endpoint fine-grained scopes in v1. Role-scoping (member/admin) is the model; per-endpoint scopes are a phase-3 consideration (master 5, master 16 decision 5).
- No GraphQL, no gRPC public surface. REST + JSON only, matching the SPA backend conventions (master 9).
- No API key login to the UI. Keys authenticate the REST API only; they do not grant a session (master 5).
- No email+password or human auth flows here; those live in PRD-001.
- The webhook surface here is org-level event delivery. The per-monitor notification channels (incl. the generic-webhook channel) are PRD-003.

---

## 2. API key product behavior

Builds directly on master 5. The product contract:

### 2.1 Key properties

| Property | Behavior | Source |
|----------|----------|--------|
| Scope | Per-org. A key belongs to one org and acts only within it. No key spans orgs. | master 5 |
| Role | Created with a role: **member** or **admin** only. No owner-equivalent keys. The key can do exactly what that role can do via the API, never more. | master 5, 16.5 |
| Secret | Shown exactly once at creation. After that only a prefix is shown. Pulse stores a hash, never the secret. | master 5, 13 |
| Prefix | A short non-secret prefix (e.g. `pulse_sk_ab12...`) is stored and shown so a key is identifiable in the list and in logs without exposing the secret. | master 5 |
| Revocable | Any key can be revoked immediately. A revoked key fails all requests from that point. | master 5 |
| Last-used | The list shows a last-used timestamp so stale keys are easy to spot and rotate. | master 5 |
| Created-by | Each key records who created it (user id) and when. | master 5 |
| Name | A human label set at creation, editable. | master 10 screen 11 |

### 2.2 Auth header format

- Requests authenticate with a bearer token in the `Authorization` header: `Authorization: Bearer pulse_sk_<secret>`.
- The full secret value is the prefix plus the secret body shown once at creation. The server hashes the presented value and compares against stored hashes.
- A missing, malformed, unknown, or revoked credential returns `401` with the standard error envelope (`code: "unauthenticated"`).
- A valid key whose role is too low for the operation returns `403` (`code: "forbidden"`). The distinction between 401 (who are you) and 403 (you are known but not allowed) is kept strict.

### 2.3 How a key maps to permissions

A key inherits a role and is evaluated against the master RBAC matrix (master 4) exactly as a human membership of that role would be, restricted to the API surface. A key never exceeds its role.

| Key role | Can do (API) | Cannot do |
|----------|--------------|-----------|
| member | View + create/edit/delete monitors, run check-now, view + create/edit/delete channels and test, view status pages, create/edit/publish status pages, view incidents, acknowledge/annotate incidents | Manual incident close, manage people, manage keys, view/manage billing, edit org settings |
| admin | Everything a member key can, plus: manual incident close, invite/manage members and set roles (not to/from owner), create/revoke API keys, view audit log, edit org settings, view billing | Manage billing (plan/payment/invoices), transfer ownership, delete the org. These require owner, which keys are never issued for. |

The matrix is the contract. Every per-operation min-role in section 4 is the matrix row for that capability.

### 2.4 Rate limits per key, by plan tier

Per-key rate limits scale by the org's plan tier (master 9, 11). Indicative limits (exact numbers are GTM-tunable in PRD-006; the shape is fixed here):

Plan names below use the public names from `docs-site/pricing.html` (the source of
truth) with the internal code in parentheses; codes stay `tier1`/`tier2`/`tier3`/`tierCustom`.

| Plan tier | API access | Indicative per-key rate |
|-----------|------------|-------------------------|
| Free (`tier1`) | None: no API keys (the management UI shows an upgrade prompt; key creation is rejected) | n/a |
| Hobby (`tier2`) | Read-only | ~120 req/min, reads only (writes return 403, see 9.1) |
| Professional (`tier3`) | Read + write | ~300 req/min |
| Custom (`tierCustom`) | Read + write, highest rate | ~600 req/min |

- Limits are per key, not per org, so one noisy key does not starve another. (Open decision 11.3 covers whether to also cap at the org level.)
- Exceeding the limit returns `429` with `Retry-After` and the rate-limit headers (section 3.6).
- Limits follow the org's current entitlement. A downgrade lowers the cap; cached entitlements invalidate on plan change (master 11, PRD-006).

---

## 3. API conventions

The same JSON conventions as the SPA backend, reused from master 9 and v1 12.3. These are non-negotiable for consistency.

### 3.1 Versioning

- Base path is `/api/v1`. All public endpoints live under it.
- Additive changes (new optional fields, new endpoints, new enum values consumers can ignore) do **not** bump the version.
- Breaking changes go to a new major path `/api/v2` with its own OpenAPI spec. v1 stays supported through a deprecation window.
- Deprecation window: when `/api/v2` ships, `/api/v1` is supported for at least 12 months (recommended default, open decision territory but stated here so clients can plan). Deprecated endpoints return a `Deprecation` header and a `Sunset` header (RFC8594) with the planned end date, and the deprecation is documented on the docs site.

### 3.2 Cursor pagination

- All list endpoints are cursor-paginated. Response shape:

```json
{ "items": [ ... ], "next_cursor": "opaque-string-or-null" }
```

- `limit` query param: default 100, max 500. A `limit` above 500 is clamped to 500 (not an error).
- `next_cursor` is opaque; clients pass it back as `?cursor=...` to get the next page. `null` means no more pages.
- Cursors are stable under inserts (no skipped or duplicated rows on the happy path). Offset pagination is not offered.

### 3.3 Standard error envelope

- Every non-2xx response uses the standard envelope (master 9, v1 12.3):

```json
{ "error": { "code": "validation_failed", "message": "Human-readable summary.", "fields": { "interval_seconds": "must be >= the plan floor of 300" } } }
```

- `code`: stable machine-readable string (e.g. `validation_failed`, `unauthenticated`, `forbidden`, `not_found`, `conflict`, `rate_limited`, `entitlement_exceeded`).
- `message`: human-readable, safe to show.
- `fields`: present on validation errors, maps field name to per-field message. Reuses the per-field validation shape from appendix A / v1 12.4.
- Entitlement rejections (monitor cap, interval floor, region not in plan, seat cap) use `code: "entitlement_exceeded"` and carry an upsell-friendly message (master 11).

### 3.4 Timestamps

- All timestamps in API requests and responses are RFC3339 in UTC (e.g. `2026-06-21T14:00:00Z`). No local time, no epoch-only fields (master 9, appendix B).

### 3.5 Secret redaction

- Secrets are write-only over the API and never returned: channel webhook URLs, SMTP passwords, monitor headers flagged `secret`, API key secrets, webhook signing secrets (master 13).
- On read, a configured secret is represented as a redacted marker (e.g. `"configured": true`) or a prefix only, never the value. Secrets are never logged.

### 3.6 Rate-limit headers and 429

- Every API response carries:
  - `X-RateLimit-Limit`: the per-key ceiling for the window.
  - `X-RateLimit-Remaining`: requests left in the current window.
  - `X-RateLimit-Reset`: when the window resets (RFC3339 or epoch seconds, fixed in the spec).
- On exceed: `429 Too Many Requests`, standard envelope (`code: "rate_limited"`), and a `Retry-After` header (seconds).

### 3.7 Idempotency keys for unsafe writes

- Unsafe writes (POST that creates a resource, check-now, send-test) accept an optional `Idempotency-Key` request header.
- When supplied, the server records the key and the response. A retry with the same key and same request body returns the original result instead of creating a duplicate. This makes retries from CI and from at-least-once pipelines safe.
- Recommended, not required, in v1 (open decision 11.1). Keys are remembered for at least 24 hours.

---

## 4. API surface

Org is implied by the key (master 9); resources are addressed under `/api/v1`. Min-role is the master 4 matrix row for that capability. "member" implies a member or admin key works; "admin" means admin key required; "owner" capabilities are not reachable by any key (UI-only by design).

### 4.1 Monitors (PRD-002, region fields per PRD-007)

| Operation | Method + path | Min role |
|-----------|---------------|----------|
| List monitors | `GET /api/v1/monitors` | viewer-equivalent (read; any valid key) |
| Get a monitor (incl. per-region status, coverage-degraded) | `GET /api/v1/monitors/{id}` | read |
| Create monitor (incl. `regions`, `down_policy`, validated vs entitlement) | `POST /api/v1/monitors` | member |
| Update monitor | `PATCH /api/v1/monitors/{id}` | member |
| Delete monitor | `DELETE /api/v1/monitors/{id}` | member |
| Check now | `POST /api/v1/monitors/{id}/check-now` | member |
| List check results (`range`=24h/7d/30d, filter by region, paginated) | `GET /api/v1/monitors/{id}/results` | read |
| List incidents for a monitor | `GET /api/v1/monitors/{id}/incidents` | read |

Notes: `regions` and `down_policy` are validated against the org's region entitlement on write (master 6.2, 11; PRD-007). A monitor read exposes per-region status and any coverage-degraded state (master 6.7). Check-now is serialized per monitor; a concurrent call returns the in-flight or just-finished result or `409` (master 6.3).

### 4.2 Channels (PRD-003)

| Operation | Method + path | Min role |
|-----------|---------------|----------|
| List channels (redacted) | `GET /api/v1/channels` | read |
| Get channel (redacted) | `GET /api/v1/channels/{id}` | read |
| Create channel (secrets write-only) | `POST /api/v1/channels` | member |
| Update channel (secrets write-only) | `PATCH /api/v1/channels/{id}` | member |
| Delete channel | `DELETE /api/v1/channels/{id}` | member |
| Send test message | `POST /api/v1/channels/{id}/test` | member |

### 4.3 Incidents (PRD-002)

| Operation | Method + path | Min role |
|-----------|---------------|----------|
| List incidents (filter by status) | `GET /api/v1/incidents` | read |
| Get incident | `GET /api/v1/incidents/{id}` | read |
| Annotate incident | `POST /api/v1/incidents/{id}/annotations` | member |
| Manual close incident | `POST /api/v1/incidents/{id}/close` | **admin** |

Manual close is admin+ because it overrides the alerting machine (master 4, 6.4, 16.8).

### 4.4 Status pages (PRD-004)

| Operation | Method + path | Min role |
|-----------|---------------|----------|
| List status pages | `GET /api/v1/status-pages` | read |
| Get status page | `GET /api/v1/status-pages/{id}` | read |
| Create status page | `POST /api/v1/status-pages` | member |
| Update status page | `PATCH /api/v1/status-pages/{id}` | member |
| Publish / unpublish | `POST /api/v1/status-pages/{id}/publish` | member |

Custom-domain configuration is owner/admin in the UI (master 4, 8); v1 status pages ship without custom domain (master 15, Phase 2 out-scope), so no custom-domain API endpoint in v1.

### 4.5 Regions (PRD-007)

| Operation | Method + path | Min role |
|-----------|---------------|----------|
| List regions available to the org (its plan entitlement) | `GET /api/v1/regions` | read |

So clients pick valid region codes before creating or updating a monitor (master 9, PRD-007).

### 4.6 Members, invitations, API keys (PRD-001)

| Operation | Method + path | Min role |
|-----------|---------------|----------|
| List members | `GET /api/v1/members` | admin |
| Update a member's role (not to/from owner) | `PATCH /api/v1/members/{id}` | admin |
| Remove a member (not an owner) | `DELETE /api/v1/members/{id}` | admin |
| List invitations | `GET /api/v1/invitations` | admin |
| Create invitation (email + role) | `POST /api/v1/invitations` | admin |
| Revoke / resend invitation | `DELETE` / `POST /api/v1/invitations/{id}/resend` | admin |
| List API keys (metadata, never secret) | `GET /api/v1/api-keys` | admin |
| Create API key (secret shown once in response) | `POST /api/v1/api-keys` | admin |
| Revoke API key | `DELETE /api/v1/api-keys/{id}` | admin |
| List webhooks (section 7) | `GET /api/v1/webhooks` | admin |
| Create / update / delete webhook | `POST` / `PATCH` / `DELETE /api/v1/webhooks/{id}` | admin |

A create-key call is the one place a secret appears in a response body, and only the once. Transfer-of-ownership has no API (owner-only, master 4).

### 4.7 Billing (PRD-006)

| Operation | Method + path | Min role |
|-----------|---------------|----------|
| Read plan, seats, usage meters, overage state | `GET /api/v1/billing` | admin |
| Manage billing (plan change, payment method, invoices) | not available via API | owner only, UI-only |

Billing **read** is admin+ (master 4: admin views billing). Billing **management** is owner-only and stays UI-only by design, because no owner keys are issued (master 5, 9, 16.5). This is intentional: a leaked key cannot change the plan, swap the card, or run up charges.

---

## 5. OpenAPI + Swagger UI

- The **OpenAPI 3 specification is the single source of truth** for the public REST API (master 9). Handlers, validation, and docs all trace back to it. The spec defines paths, methods, request/response schemas, the error envelope, pagination shape, auth scheme, and rate-limit headers.
- The api service serves interactive **Swagger UI at `/api/docs`** (master 9, 10). A developer can browse the API, read schemas, and try calls against their own org by pasting a key.
- The spec is versioned with the API: it lives under `/api/v1`; a future `/api/v2` ships its own spec. The served spec always matches the running api service for that version (master 9).
- The same spec drives the docs-site API reference (section 6), so the public reference and the live API never diverge.
- Try-it-out calls from Swagger UI are real calls subject to auth, RBAC, rate limits, and entitlements like any other client.

---

## 6. Documentation site

A static **documentation site and pricing page on GitHub Pages** (master 10). Static so it stays cheap and stays up independently of the api service.

### 6.1 Content

| Surface | What lives there |
|---------|------------------|
| Product guides | Getting started, creating a monitor, connecting a channel, reading incidents, setting up a status page, using the API from CI, setting up outbound webhooks and verifying signatures |
| API reference | Auto-generated from the OpenAPI spec: every resource, operation, schema, error code, and the auth/pagination/rate-limit conventions |
| Pricing page | The plan tiers and what each includes (monitors, interval floor, regions, seats, retention, status pages, API access and rate, webhooks), mapped to PRD-006 / master 11 |
| Conventions reference | Versioning and deprecation policy, error envelope, cursor pagination, RFC3339, idempotency, rate-limit headers |
| Webhook reference | Event types, payload shapes, signature verification steps, retry/backoff behavior, replay protection |

### 6.2 No-drift mechanism

- A **CI job regenerates the API reference from the OpenAPI spec on each release** (master 10, 15). The published reference is a build artifact of the spec, not hand-maintained, so the public docs never drift from the live API.
- Swagger UI at `/api/docs` is the interactive surface (live, served by the api service); the GitHub Pages reference is the static, indexable, linkable surface. Both come from the same spec.

---

## 7. Outbound webhooks

Org-level event delivery, distinct from the per-monitor generic-webhook **channel** (PRD-003). A channel fires on one monitor's state change as a notification; an org webhook is a programmatic event feed for the whole org so SREs wire Pulse into their own pipelines without polling (master 9).

### 7.1 Event types

| Event type | Fires when | Phase |
|------------|------------|-------|
| `monitor.down` | a monitor's incident opens (state machine, master 6.4) | v1 (GA) |
| `monitor.recovery` | a monitor recovers and its incident closes by recovery | v1 (GA) |
| `incident.opened` | an incident is opened | v1 (GA) |
| `incident.closed` | an incident closes (recovered, disabled, or manual close) | v1 (GA) |
| `monitor.created` | a monitor is created | phased (master 9, Phase 3) |
| `monitor.updated` | a monitor's config changes | phased |
| `monitor.deleted` | a monitor is deleted | phased |
| `member.added` / `member.removed` / `member.role_changed` | membership changes | phased |

`monitor.down`/`monitor.recovery` and `incident.opened`/`incident.closed` overlap in timing but are distinct events; a subscriber can listen to either granularity. A webhook registration selects which event types it wants.

### 7.2 Delivery contract

| Property | Behavior |
|----------|----------|
| Delivery | At-least-once with retry and backoff (master 9, consistent with notifier delivery in master 6.6) |
| Dedup | Each event carries a stable per-event id (`event_id`). Receivers dedup on it so an at-least-once redelivery is harmless. |
| Ordering | Per-org best-effort ordering; receivers must not assume strict global order and should use timestamps + event ids. |
| Signing | Each webhook has its own signing secret. Pulse signs each delivery and sends the signature in a header (e.g. `X-Pulse-Signature: t=<ts>,v1=<hmac>`), HMAC over the timestamp + raw body. |
| Verification | The receiver recomputes the HMAC with the shared secret and compares. A mismatch means reject. The signing secret is shown once at webhook creation and stored as a secret (master 13). |
| Replay protection | The signed timestamp is in the header. Receivers reject deliveries whose timestamp is outside an allowed skew window (recommended 5 minutes). Combined with `event_id` dedup, this stops replay. |
| Retry budget | Recommended default: retry with exponential backoff for up to ~24 hours, then stop and surface the webhook as failing in the UI/audit (open decision 11.3). |
| Failure visibility | Repeated delivery failure is visible (last delivery status, last success) so an owner/admin can see a broken receiver, like notifier failures in master 6.6. |

### 7.3 Payload shape

Each delivery is a JSON body with a consistent envelope: `event_id`, `event` (type), `org_id`, `created_at` (RFC3339), and a `data` object carrying the relevant resource snapshot (monitor, incident, check) using the same field shapes as appendix B where they overlap. Secrets are never included.

### 7.4 Registration and management

- Managed in the UI (a new surface under org settings / integrations) and via the API (section 4.6: `GET/POST/PATCH/DELETE /api/v1/webhooks`), admin+ per the matrix.
- A registration holds: target URL, selected event types, enabled flag, the signing secret (shown once), created-by, last-delivery status. Owner/admin can rotate the signing secret and disable or delete a webhook.
- Outbound webhooks are a paid-tier feature; the Free tier does not get them (master 11 plan table).

---

## 8. Multi-region in the API

References PRD-007 and master 6.7.

- Monitors expose region selection (`regions`, a list of region codes) and `down_policy` (`any` / `quorum` / `all`, default `quorum`). Both are settable on create and update and validated against the org's region entitlement; picking a region the plan does not include is rejected with `entitlement_exceeded` and the per-field error shape (master 6.2, 11; PRD-007).
- Check results carry the `region` they ran from. `GET /monitors/{id}/results` is filterable by region (master 6.3, 9).
- A monitor read exposes per-region status and any coverage-degraded state, so a client can see which region saw a failure and when our own coverage is reduced (master 6.7).
- `GET /api/v1/regions` returns the regions available to the org (its plan entitlement) so clients pick valid codes before writing (master 9; section 4.5).
- The API never declares a monitor down on missing data from our own degraded region; that aggregation is owned by the platform (master 6.7) and the API only reports the resulting state.

---

## 9. RBAC and plan gating

References PRD-001 (roles) and PRD-006 (rate tiers).

### 9.1 No API on Free, read-only on Hobby, full on Professional/Custom

| Tier | API behavior |
|------|--------------|
| Free (`tier1`) | **No API access.** The org cannot create API keys at all: the key-management UI shows an upgrade prompt and `POST /api/v1/api-keys` is rejected. |
| Hobby (`tier2`) | API is **read-only**. Write operations (create/update/delete monitors, channels, status pages, incident actions, member/key/webhook management) return `403` with `code: "forbidden"` and an upsell message. Reads work within the rate. |
| Professional (`tier3`) / Custom (`tierCustom`) | Full read + write at the tier's rate (section 2.4). |

This is a plan entitlement, evaluated per request against the org's cached entitlement (master 11). It is separate from the key's role: a Hobby org's admin key still cannot write, because the org has only a read entitlement; a Free org has no keys at all.

### 9.2 Two independent gates on every call

Every API call is checked against both, and both must pass:

1. **Role gate**: the key's role vs the operation's min-role (master 4, section 4).
2. **Entitlement gate**: the org's plan allows the operation (Free read-only; metered write limits like monitor cap, interval floor, region set, seat cap return `entitlement_exceeded`) (master 11, PRD-006).

Owner-only billing management fails the role gate for every key, which is why it stays UI-only (section 4.7).

---

## 10. User stories, acceptance criteria, edge cases

### 10.1 User stories

- **As a solo dev, I script monitor creation from CI** so my monitors live in version control. I create an admin or member key, store it as a CI secret, and `POST /api/v1/monitors` for each endpoint in my repo on deploy.
- **As an SRE, I manage hundreds of monitors as code** through the API, picking regions and a down policy per monitor, and I reconcile them on every pipeline run using idempotency keys so reruns do not duplicate.
- **As an SRE, I ingest Pulse events into our pipeline** by registering an org webhook for `monitor.down` / `monitor.recovery` / `incident.opened` / `incident.closed`, verifying the signature, and routing into our own incident tooling without polling.
- **As a developer, I explore the API** at `/api/docs`, paste my key, and run a real call against my own org before writing any client code.

### 10.2 Acceptance criteria (testable)

1. A `POST /api/v1/api-keys` returns the full secret exactly once; a subsequent `GET /api/v1/api-keys` returns only metadata (name, role, prefix, created-by, created, last-used), never the secret.
2. A request with a valid member key to a member-level write succeeds (2xx); the same call with no key returns 401; with a member key to an admin-only operation (e.g. manual incident close) returns 403.
3. A revoked key returns 401 on the very next request after revocation.
4. Listing results with `limit=1000` returns at most 500 items and a `next_cursor`; paging through with the cursor returns all rows with no duplicates or gaps on a static dataset.
5. Every successful response includes `X-RateLimit-Limit`, `-Remaining`, `-Reset`; exceeding the limit returns 429 with `Retry-After` and `code: "rate_limited"`.
6. A validation failure returns the standard envelope with `code: "validation_failed"` and a `fields` map naming each bad field.
7. Creating a monitor with a region not in the org's plan returns `entitlement_exceeded` with a `fields` entry on `regions`.
8. A Free-tier key can `GET /api/v1/monitors` but a `POST /api/v1/monitors` returns 403 with an upsell message.
9. Two identical creates with the same `Idempotency-Key` produce one resource and identical responses.
10. An outbound webhook delivery includes a valid `X-Pulse-Signature`; recomputing the HMAC with the signing secret matches; a tampered body fails verification.
11. A duplicate webhook delivery carries the same `event_id` as the first.
12. `GET /api/v1/billing` returns plan/usage for an admin key; there is no API path that changes the plan, payment method, or deletes the org.
13. The OpenAPI spec served at `/api/docs` lists every operation in section 4 with its correct method, path, and auth requirement; the GitHub Pages API reference built from the same spec matches it.

### 10.3 Edge cases

| Edge case | Expected behavior |
|-----------|-------------------|
| Key revoked mid-request | The in-flight request may complete; the next request returns 401. No partial-auth state. |
| Rate limit exceeded | 429 + `Retry-After`; no side effect from the rejected call; client backs off and retries. |
| Webhook receiver down | At-least-once retry with backoff up to the retry budget (11.3); then the webhook is shown as failing; no events silently lost while within budget. |
| Webhook signature mismatch on receiver | Receiver rejects; Pulse still considers the delivery sent (we cannot know the receiver's verification result). Receivers must verify before acting. |
| Duplicate webhook delivery | Same `event_id`; receiver dedups; idempotent on their side. |
| Concurrent check-now on one monitor | Serialized; second call gets the in-flight/just-finished result or 409 (master 6.3). |
| Plan downgrade lowers rate or removes write | Next request reflects the new entitlement once the cache invalidates; existing keys are not deleted, just bounded by the lower plan. |
| Idempotency-Key reused with a different body | Treated as a conflict (`409`), since the key already maps to a different request. |
| Cursor from an older page after data churn | Still returns a valid (possibly shifted) page; no 500. |

---

## 11. Open decisions (with recommended defaults)

1. **Is `Idempotency-Key` required or optional on unsafe writes?** Recommended: **optional but recommended in v1**, remembered 24h. Trade-off: optional is friendlier for quick scripts; required is safer for at-least-once pipelines. We can tighten to required for specific endpoints later if duplicate-creation reports show up. (Used in section 3.7.)

2. **Per-endpoint scopes vs role-scoping for keys.** Recommended: **role-scoping only in v1** (member/admin), per master 5 and 16.5. Per-endpoint scopes are deferred to phase 3. Trade-off: less granular least-privilege per key now; far simpler model for the target developer audience.

3. **Webhook retry budget and org-level rate cap.** Recommended: **exponential backoff up to ~24h then stop and surface as failing**; **per-key rate limits only in v1, no separate org-level cap** unless abuse shows up. Trade-off: a single org with many keys could push aggregate load; we add an org ceiling if metering shows it is needed.

4. **Deprecation window length for a future `/api/v2`.** Recommended: **at least 12 months** of `/api/v1` support with `Deprecation` + `Sunset` headers. Trade-off: longer support cost vs developer trust; 12 months is a safe default for the audience.

---

## 12. Dependencies

| Depends on | For what |
|------------|----------|
| PRD-001 Identity & Tenancy | API keys (per-org, role-scoped, hash storage, revoke, last-used, created-by), the RBAC matrix that every min-role traces to, org scoping of every call |
| PRD-006 Billing & Entitlements | Rate-limit tiers, Free read-only entitlement, write entitlement gates (monitor cap, interval floor, region set, seat cap), cached entitlement on the hot path |
| PRD-002 Monitoring Engine | The monitors / results / incidents resources and their fields, check-now semantics, the alerting state machine that fires webhook events |
| PRD-003 Notifications | The channels resource; the distinction between the per-monitor generic-webhook channel and org-level webhooks |
| PRD-004 Status Pages | The status-pages resource and operations |
| PRD-007 Multi-Region | `regions` and `down_policy` fields, per-region results, coverage-degraded state, the regions catalog endpoint |
| Master 9, 10 (this domain) | Versioning, conventions, OpenAPI-as-source-of-truth, Swagger UI at `/api/docs`, the GitHub Pages docs + pricing site with CI regeneration |
| Phasing (master 15, Phase 2) | This entire surface ships at GA, after identity, monitoring, and channels exist |
