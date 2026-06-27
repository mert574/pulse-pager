# RFC-012 - API Design and OpenAPI

Status: DRAFT for review
Author: Principal API Architect
Audience: api service authors, frontend (RFC-013), docs/CI authors, and any external developer building against the public API
Parent: `docs/rfc/RFC-000-architecture-overview.md` (section 2.1 api, section 12 entitlement enforcement, the conventions)
Depends on: RFC-000, RFC-001 (the id strategy this RFC's codec owns), RFC-003 (auth, API keys, org context), RFC-009 / RFC-000 section 12 (entitlement error codes)
Product source of truth: `docs/prd/PRD-005-public-api-and-webhooks.md`, master PRD section 9 (API conventions) and section 10 (docs surfaces), and every sub-PRD's API surface
Consumed by: RFC-013 (the SPA is the largest client of this contract)

House style: no em-dashes. Tables and diagrams over prose. Every load-bearing choice states the decision, the reasoning, and the rejected alternatives.

---

## 1. Overview, scope, and owned contracts

### 1.1 What this RFC is

This RFC fixes the shape of the Pulse HTTP API: the one contract that both the Lit SPA (RFC-013) and external developers (PRD-005) call. It nails down the conventions (versioning, errors, pagination, casing, idempotency), the complete resource-by-resource surface, how auth and org context arrive on the wire, how rate limits and entitlement rejections surface, and the OpenAPI-spec-to-published-docs pipeline. It also owns the external id codec that RFC-001 deliberately handed off.

There is one HTTP surface served by the api service (RFC-000 section 2.1). The SPA backend routes and the public `/api/v1` routes are the same router, the same handlers, and the same OpenAPI spec. The only differences are which credential authenticates the caller (session cookie vs API key) and how org context arrives. We do not maintain two API definitions.

### 1.2 Scope

| In scope | Out of scope (owner) |
|----------|----------------------|
| `/api/v1` base path, versioning + deprecation policy | OAuth provider mechanics, JWT/refresh/key internals (RFC-003) |
| The JSON error envelope and the full `code` enum | The entitlement model and limit values (RFC-009, PRD-006) |
| Cursor pagination, filtering, sorting, the `range` filter | RBAC matrix definition (RFC-003 section 7, master 4) |
| Secret redaction rules and idempotency-key behavior | Schema/DDL and the BIGINT internal id (RFC-001) |
| The external id codec (`mon_`, `inc_`, ...) | Webhook delivery, retry, signing mechanics (RFC-007, PRD-005 section 7) |
| The full resource-by-resource API surface table | The SPA itself, how it stores the cookie (RFC-013) |
| OpenAPI 3 as source of truth + the CI no-drift check | Stripe webhook handler (PRD-006) |
| Swagger UI at `/api/docs` and the GitHub Pages docs/pricing site | Notifier and the per-monitor channel delivery (RFC-007) |
| Rate-limit headers and 429 shape | Rate-limit counter storage (Redis, RFC-009) |
| The org-webhook management endpoints (CRUD only) | |

### 1.3 Contracts this RFC owns

| Contract | Decision |
|----------|----------|
| Base path + versioning | `/api/v1`; breaking change goes to `/api/v2`; additive changes never bump; ~12 month deprecation window (section 2.1) |
| Error envelope | `{"error":{"code","message","i18n?","fields?}}` with a fixed `code` enum; `message`/`fields` are localizable per the i18n shape (section 2.4, 2.10, RFC-014) |
| Pagination | `{items, next_cursor}`, opaque cursor, `limit` default 100 / max 500 clamped (section 2.6) |
| Timestamps | RFC3339 in UTC everywhere (section 2.5) |
| Field casing | snake_case for every request and response field (section 2.8) |
| Secret redaction | secrets are write-only; reads show `*_set` booleans or a prefix only (section 2.7) |
| Idempotency | optional `Idempotency-Key` header on unsafe writes, remembered 24h (section 2.9) |
| External id codec | prefix + decimal of the internal bigint, swappable to a keyed encoding with no schema change (section 3) |
| Spec discipline | spec-first OpenAPI 3, codegen for server types + a CI route/spec drift check (section 8) |
| Docs pipeline | Swagger UI at `/api/docs` (live) + GitHub Pages reference regenerated from the spec on release (sections 9, 10) |

---

## 2. Conventions

The same JSON conventions as the SPA backend, fixed by master 9 and PRD-005 section 3. They are non-negotiable for consistency, and they apply identically to session-authenticated SPA calls and key-authenticated public calls.

### 2.1 Base path and versioning

| Rule | Decision |
|------|----------|
| Base path | All public + SPA resource endpoints live under `/api/v1`. Auth-flow and well-known endpoints (`/auth/...`, `/.well-known/jwks.json`) are not versioned (they are infrastructure, not the resource contract). |
| Additive changes | New optional request fields, new response fields, new endpoints, and new enum values a client can ignore do NOT bump the version. Clients must tolerate unknown fields and unknown enum values. |
| Breaking changes | A removed/renamed field, a changed type, a tightened validation, a removed endpoint, or a changed default goes to a new major path `/api/v2` with its own OpenAPI spec. |
| Deprecation window | When `/api/v2` ships, `/api/v1` stays supported for at least 12 months (PRD-005 open decision 11.4). |
| Deprecation signaling | A deprecated endpoint returns a `Deprecation: true` header and a `Sunset: <RFC1123 date>` header (RFC 8594) with the planned end date, and the deprecation is called out on the docs site. |

The spec is versioned with the path. `/api/v1` has one spec; a future `/api/v2` ships its own. The served spec always matches the running api for that version (PRD-005 section 5).

### 2.2 Resource naming

| Rule | Example |
|------|---------|
| Plural noun collections | `/monitors`, `/channels`, `/incidents`, `/status-pages`, `/api-keys`, `/webhooks` |
| Resource by external id | `/monitors/{id}` where `{id}` is the prefixed external id `mon_123` (section 3) |
| Sub-collections under the parent | `/monitors/{id}/results`, `/monitors/{id}/incidents`, `/incidents/{id}/annotations` |
| Actions as a verb sub-resource (POST) | `/monitors/{id}/check-now`, `/channels/{id}/test`, `/incidents/{id}/close`, `/status-pages/{id}/publish` |
| Kebab-case in path segments | `status-pages`, `api-keys`, `check-now` |
| snake_case in JSON bodies and query params | `interval_seconds`, `down_policy`, `next_cursor` |

Actions that are not pure CRUD (check-now, test, publish, close) are modeled as a POST to a named sub-resource rather than overloading PATCH, so the intent is explicit in the route and in audit logs.

### 2.3 HTTP method semantics

| Method | Use | Body | Idempotent |
|--------|-----|------|------------|
| GET | read a resource or list | none | yes |
| POST | create a resource, or run an action (check-now, test, publish, close, annotate) | yes | no (unless an `Idempotency-Key` is supplied, section 2.9) |
| PATCH | partial update; only the supplied fields change | yes (partial) | no |
| DELETE | remove a resource (revoke a key, delete a monitor) | none | yes |

PUT is not used. Full-replace update is not offered; PATCH partial-update is the only mutation verb, which avoids the "did an omitted field mean clear it or leave it" ambiguity.

| Status | Meaning |
|--------|---------|
| 200 | OK (read, update, action returning a body) |
| 201 | Created (POST that creates a resource) |
| 202 | Accepted (an action accepted for async processing, e.g. check-now when serialized behind an in-flight run) |
| 204 | No content (DELETE) |
| 400 | Malformed request (bad JSON, bad cursor) |
| 401 | Unauthenticated (missing/invalid/unknown/revoked credential), `code: "unauthenticated"` |
| 403 | Authenticated but not allowed: role gate or entitlement gate failed (`code: "forbidden"` or `code: "entitlement_exceeded"`) |
| 404 | Not found, or not visible to this org (we 404 cross-org reads rather than 403-leaking existence; RFC-003 section 6.1) |
| 409 | Conflict (concurrent check-now, idempotency-key reused with a different body, at-least-one-owner) |
| 422 | Validation failed, `code: "validation_failed"` with a `fields` map |
| 429 | Rate limited, `code: "rate_limited"` + `Retry-After` |
| 5xx | Server error; never leaks internals in `message` |

Note: PRD-005 section 2.2 describes validation as `400`-shaped in prose. This RFC uses `422 Unprocessable Entity` for body-level validation failures and reserves `400` for requests that cannot be parsed at all (bad JSON, bad cursor). Both carry the standard envelope, so a client keyed on the `code` field is unaffected. Deviation flagged for the spec; the `code` is the contract, the HTTP status is a hint.

### 2.4 Standard error envelope

Every non-2xx response uses one envelope (master 9, PRD-005 section 3.3, v1 12.3):

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

- `code` is a stable machine-readable string. Clients branch on it, never on `message`.
- `message` is human-readable and safe to show. It never contains internal detail, SQL, or stack traces. It is the English render of `i18n` (the always-present fallback).
- `i18n` is an optional `{code, params?}` that localizes the top-level message. Its `code` is an i18n key (the RFC-014 namespace), distinct from `error.code` (the machine enum clients branch on). A client with a catalog renders from `i18n.code` + `params`; a client without one shows `message`.
- `fields` is present only on validation and entitlement-per-field errors; it maps a field name to a per-field localizable object `{code, params?, message}` (the appendix A / v1 12.4 per-field shape, now i18n-extended). `fields[name].message` is the English fallback; `fields[name].code` localizes the same message. See section 2.10 and RFC-014.

The full `code` enum (the OpenAPI spec declares this as a closed string enum; new codes are an additive change):

| `code` | HTTP | Meaning | Has `fields`? |
|--------|------|---------|---------------|
| `unauthenticated` | 401 | no credential, or unknown/malformed/revoked credential (RFC-003 section 5) | no |
| `forbidden` | 403 | role gate failed: the actor's role lacks this capability (RFC-003 section 7) | no |
| `not_found` | 404 | resource does not exist or is not visible to this org | no |
| `conflict` | 409 | state conflict (concurrent check-now, owner invariant, idempotency-key body mismatch) | no |
| `validation_failed` | 422 | one or more fields failed validation (PRD appendix A) | yes |
| `rate_limited` | 429 | per-key rate limit exceeded; see `Retry-After` (section 6) | no |
| `entitlement_exceeded` | 403 | the entitlement gate rejected the write (one of the metered-limit codes below) | usually yes |
| `bad_request` | 400 | request could not be parsed (bad JSON, bad cursor, bad query param) | no |
| `internal` | 500 | unexpected server error | no |

The entitlement gate uses `code: "entitlement_exceeded"` as the top-level code and carries the specific metered-limit reason. The specific reasons (binding, from RFC-000 section 12 / RFC-009) appear either as the `fields` entry for the offending field or in the `message`, and clients may also read them from a `reason` sub-field of the message context. The metered reasons are:

`monitor_limit_reached`, `interval_below_plan_floor`, `interval_below_hard_floor`, `region_not_in_plan`, `region_count_exceeded`, `seat_limit_reached`, `status_page_limit_reached`, `custom_domain_not_in_plan`, `api_write_not_in_plan`, `api_rate_limited`.

See section 7 for exactly how each surfaces.

### 2.5 Timestamps

All timestamps in requests and responses are RFC3339 in UTC, e.g. `2026-06-21T14:00:00Z` (master 9, appendix B). No local time, no epoch-only fields. The one exception is `X-RateLimit-Reset` and `Retry-After`, which follow their header conventions (section 6).

### 2.6 Cursor pagination

Every list endpoint is cursor-paginated. Offset pagination is not offered.

```json
{ "items": [ /* ... */ ], "next_cursor": "opaque-string-or-null" }
```

| Rule | Decision |
|------|----------|
| Response shape | `{ items, next_cursor }`. `next_cursor` is `null` on the last page. |
| `limit` | query param, default 100, max 500. A value above 500 is clamped to 500, not rejected (PRD-005 AC4). |
| `cursor` | opaque base64 string; the client passes it back as `?cursor=...`. Clients must not parse or construct it. |
| Stability | the cursor encodes the sort key of the last row (e.g. `(checked_at, id)` for results), so paging is stable under inserts: no skipped or duplicated rows on the happy path (PRD-005 AC4). |
| Churn tolerance | a cursor from an older page still returns a valid (possibly shifted) page, never a 500 (PRD-005 edge case). |

What the cursor encodes is an implementation detail behind the opaque string; section 12 specifies the keyset per list.

### 2.7 Secret redaction

Secrets are write-only over the API and never returned (master 13, PRD-005 section 3.5): channel webhook URLs and SMTP passwords, monitor headers flagged secret, API key secrets, and webhook signing secrets.

| On write | On read |
|----------|---------|
| The client sends the secret value in the create/update body. | The server never returns the value. |
| | A configured secret is represented as a `*_set` boolean (e.g. `"webhook_secret_set": true`) or, where a stable identifier helps (API keys), a non-secret `prefix`. |

The one and only place a secret value appears in a response body is the create response of `POST /api/v1/api-keys` and the create/rotate response of a webhook signing secret, each shown exactly once (PRD-005 AC1). Secrets are never logged.

### 2.8 Field casing

Every JSON field name in a request body, response body, and every query parameter is snake_case (`interval_seconds`, `down_policy`, `next_cursor`, `created_at`). Path segments are kebab-case (`status-pages`, `check-now`). This is uniform across the whole surface so a client never guesses casing per resource.

### 2.9 Idempotency keys for unsafe writes

Unsafe writes (any POST that creates a resource, plus check-now and send-test) accept an optional `Idempotency-Key` request header (PRD-005 section 3.7).

| Rule | Behavior |
|------|----------|
| Supplied | the server records `(org_id, key, request_hash, response)` in `idempotency_keys` (RFC-001 section 4.6) and returns the recorded response on a retry with the same key and same body, instead of acting again. |
| Same key, different body | `409 conflict`: the key already maps to a different request (PRD-005 edge case). |
| Retention | keys are remembered at least 24 hours. |
| v1 stance | optional but recommended; not required (PRD-005 open decision 11.1). CI and at-least-once pipelines should always send one. |

This is what makes "reconcile monitors on every pipeline run without duplicating" safe (PRD-005 story 2, AC9).

### 2.10 Internationalization (the localizable-string shape)

Every user-facing string the API returns is a localizable object `{code, params?, message}` (RFC-014 owns this contract). `code` is a stable i18n key the client localizes against its own catalog; `params` carries machine values for interpolation (ICU MessageFormat); `message` is the English source string and the always-present fallback for a client with no catalog. This is the API-wide convention; the error envelope (section 2.4) and the channel catalog already follow it.

| Rule | Decision |
|------|----------|
| The shape | `{ "code": "<stable.i18n.key>", "params": { ... }?, "message": "<English fallback>" }` |
| Interpolation standard | ICU MessageFormat; the English source pattern lives with the code in the catalog (RFC-014 section 10) |
| What never localizes | machine codes (the `error.code` enum, enum values), ids, RFC3339 UTC timestamps, and user-entered data (monitor names, org names). The wire stays UTC and machine-neutral; the client formats dates to locale + timezone (section 2.5) |
| Locale negotiation | one resolved locale per request: explicit `?lang=` / `X-Pulse-Locale` > user locale > org default > `Accept-Language` > `en` (RFC-014 section 5) |
| Rendering | the SPA renders client-side from `code` + `params`; the server may additionally render `message` into the resolved locale for non-UI clients (RFC-014 section 6). The object is always returned |

v1 ships `en` only; adding a locale is catalog files only, with no API or spec change (RFC-014 section 11). RFC-014 is the full design.

---

## 3. The external id codec (owned here, per RFC-001 section 3.4)

RFC-001 fixes that internal primary keys are `BIGINT GENERATED ALWAYS AS IDENTITY` and that external-facing ids are a prefixed string encoding that bigint, computed at the API serialization boundary and never stored. RFC-001 deliberately handed the encoding choice to this RFC. This is it.

### 3.1 Decision

The codec is a pure pair of functions:

```
external(prefix, bigint) -> "<prefix>_<encoded>"
internal(string)         -> (prefix, bigint)   // or an error
```

For v1 the encoding is the decimal string of the bigint. `external("mon", 123) = "mon_123"`. `internal("mon_123") = ("mon", 123)`. This matches the PRD payloads literally (`mon_123`, `inc_456` in PRD-003 and PRD-005 examples).

The prefix-to-table mapping is fixed by RFC-001 section 2:

| Prefix | Resource | Prefix | Resource |
|--------|----------|--------|----------|
| `usr_` | users | `mon_` | monitors |
| `org_` | organizations | `chn_` | channels |
| `inv_` | invitations | `res_` | check_results |
| `key_` | api_keys (the row id; distinct from the key's own secret prefix) | `inc_` | incidents |
| `sp_` | status_pages | `wh_` | outbound_webhooks |

Internal-only tables (memberships, seats, joins, rollups, audit, idempotency) carry no external id; they are addressed only through their parent resource.

### 3.2 Reasoning

| Factor | Why decimal-of-bigint for v1 |
|--------|------------------------------|
| Match the PRD contract verbatim | The PRD payload examples are `mon_123` / `inc_456`. A decimal encoding makes those examples literally correct, so the spec, the docs, and the PRD agree with no asterisk. |
| URL-safety | Decimal digits plus an ASCII prefix and one underscore are URL-safe with no escaping and copy-paste clean. |
| Stability | The id is a deterministic function of an immutable bigint PK, so an external id never changes for the life of a row. Bookmarks, CI configs, and webhook payloads stay valid. |
| No PII | The id encodes a surrogate row number and a type tag. It carries no email, name, or tenant secret. |
| Decoupling | The public id is decoupled from any future re-keying: because nothing is stored, the codec can change shape without a migration (section 3.3). |

### 3.3 The enumeration tradeoff and the upgrade path

A decimal id is guessable and leaks ordering: a competitor reading `mon_5012` can infer roughly how many monitors exist, and can enumerate `mon_1, mon_2, ...`. RFC-001 section 3.2 already addresses the access side: enumeration resistance comes from authz + Postgres RLS, not from id opacity. Org A asking for `mon_2` that belongs to org B gets a `404`, never the row. The codec is not a security boundary.

What a decimal id does leak is aggregate counts and growth rate. We accept that for v1 because the PRD contract wants the literal examples and because counts are low-value. If product later wants opacity, the codec swaps to a keyed encoding (sqids/hashids over the bigint with a per-deployment salt, or a base62 of an encrypted bigint) with zero schema change, because nothing is stored. Only the serialization boundary changes; the bigint PK, the FKs, RLS, and the event keys are untouched.

| Alternative | Verdict | Reasoning |
|-------------|---------|-----------|
| Decimal of bigint (chosen for v1) | chosen | Matches the PRD examples exactly, URL-safe, stable, no PII. The only cost is count/order leakage, which RLS makes harmless for access and which is low-value to a competitor. |
| Prefix + base62 of the bigint | available, deferred | Shorter strings and slightly less obvious ordering, but base62 of a small monotonic int is still trivially ordered, so it buys almost no opacity for the loss of matching the PRD examples. Hold for if we want compactness. |
| Prefix + keyed sqids/hashids | the opacity upgrade | Real opacity (non-sequential, salt-dependent). This is the swap-in if product asks for it. Reversible, collision-free, no stored state. Not v1 because it breaks the literal PRD examples and adds no access-control value. |
| Expose the raw bigint, no prefix | rejected (RFC-001) | Leaks counts and couples the public id to the storage surrogate forever. The prefix at least namespaces the id by type and lets `internal()` validate the prefix matches the route. |

### 3.4 Codec validation behavior

`internal(string)` rejects a malformed id (wrong shape, non-numeric body, unknown prefix) with `400 bad_request` before any DB lookup. A well-formed id whose prefix does not match the route (e.g. `chn_1` passed to `/monitors/{id}`) is also a `400`, not a `404`, because it is a client mistake, not a missing row. A well-formed, correct-prefix id that no row matches (or that belongs to another org) is a `404`.

---

## 4. The API surface

Org is implied for API-key calls (the key fixes the org, RFC-003 section 5.4) and supplied in the path for SPA/session calls (section 5.4). The paths below are the resource paths under `/api/v1`; the SPA prefixes them with `/orgs/{org_id}` per section 5.4. "Min role" is the master 4 / RFC-003 section 7 matrix row for that capability. Auth column: `key` = works with an API key, `session` = session/cookie only (no key issued at that level), `both` = either. "Gated?" = the write passes through the entitlement gate (section 7).

### 4.1 Monitors (PRD-002, region fields per PRD-007)

| Operation | Method + path | Min role | Auth | Gated? |
|-----------|---------------|----------|------|--------|
| List monitors | `GET /monitors` | read (any valid key) | both | no |
| Get a monitor (per-region status, coverage-degraded) | `GET /monitors/{id}` | read | both | no |
| Create monitor (`regions`, `down_policy`, `interval_seconds`) | `POST /monitors` | member | both | yes (monitor cap, interval floor, region set) |
| Update monitor | `PATCH /monitors/{id}` | member | both | yes (interval floor, region set) |
| Delete monitor | `DELETE /monitors/{id}` | member | both | no |
| Check now | `POST /monitors/{id}/check-now` | member | both | no (serialized; 202/409) |
| List check results (`range`, `region` filter, cursor) | `GET /monitors/{id}/results` | read | both | no |
| List incidents for a monitor | `GET /monitors/{id}/incidents` | read | both | no |

Notes: `regions` and `down_policy` (`any` / `quorum` / `all`, default `quorum`) are validated against the org's region entitlement on write; a region not in plan returns `entitlement_exceeded` with a `fields.regions` entry (PRD-007, section 7). A monitor read exposes per-region status and any coverage-degraded state (master 6.7). Check-now is serialized per monitor: a concurrent call returns the in-flight or just-finished result (`202`) or `409` (master 6.3). `results` supports `range`=24h/7d/30d and a `region` filter (section 12).

### 4.2 Channels (PRD-003)

| Operation | Method + path | Min role | Auth | Gated? |
|-----------|---------------|----------|------|--------|
| List channels (redacted) | `GET /channels` | read | both | no |
| Get channel (redacted) | `GET /channels/{id}` | read | both | no |
| Create channel (secrets write-only) | `POST /channels` | member | both | no |
| Update channel (secrets write-only) | `PATCH /channels/{id}` | member | both | no |
| Delete channel | `DELETE /channels/{id}` | member | both | no |
| Send test message | `POST /channels/{id}/test` | member | both | no |

Channel reads are redacted: webhook URLs and SMTP passwords are write-only and read back as `*_set` booleans (section 2.7).

### 4.3 Incidents (PRD-002)

| Operation | Method + path | Min role | Auth | Gated? |
|-----------|---------------|----------|------|--------|
| List incidents (filter by `status`) | `GET /incidents` | read | both | no |
| Get incident | `GET /incidents/{id}` | read | both | no |
| Annotate incident | `POST /incidents/{id}/annotations` | member | both | no |
| Manual close incident | `POST /incidents/{id}/close` | **admin** | both | no |

Manual close is admin+ because it overrides the alerting state machine (master 6.4, 16.8). It is reachable by an admin key because admin keys exist; owner-only actions are not.

### 4.4 Status pages (PRD-004)

| Operation | Method + path | Min role | Auth | Gated? |
|-----------|---------------|----------|------|--------|
| List status pages | `GET /status-pages` | read | both | no |
| Get status page | `GET /status-pages/{id}` | read | both | no |
| Create status page | `POST /status-pages` | member | both | yes (status-page cap) |
| Update status page | `PATCH /status-pages/{id}` | member | both | no |
| Publish / unpublish | `POST /status-pages/{id}/publish` | member | both | no |

No custom-domain endpoint in v1 (out of Phase 2 scope, PRD-005 section 4.4). The public read of a published status page is served by api on a separate unauthenticated path (`status-page read path`, RFC-003 section 8.4) using `status_pages.public_token`, not the prefixed id, and is not part of `/api/v1`.

### 4.5 Members, invitations, API keys (PRD-001)

| Operation | Method + path | Min role | Auth | Gated? |
|-----------|---------------|----------|------|--------|
| List members | `GET /members` | admin | both | no |
| Update a member's role (not to/from owner) | `PATCH /members/{id}` | admin | both | no |
| Remove a member (not an owner) | `DELETE /members/{id}` | admin | both | no |
| List invitations | `GET /invitations` | admin | both | no |
| Create invitation (email + role) | `POST /invitations` | admin | both | yes (seat cap) |
| Revoke invitation | `DELETE /invitations/{id}` | admin | both | no |
| Resend invitation | `POST /invitations/{id}/resend` | admin | both | no |
| List API keys (metadata only) | `GET /api-keys` | admin | both | no |
| Create API key (secret shown once) | `POST /api-keys` | admin | both | no |
| Revoke API key | `DELETE /api-keys/{id}` | admin | both | no |

`POST /api-keys` is the one place a key secret appears in a response, exactly once (PRD-005 AC1). A subsequent `GET /api-keys` returns only metadata: `name`, `role`, `prefix`, `created_by`, `created_at`, `last_used_at`. Updating a member to or from owner has no API: ownership transfer is owner-only and UI-only (master 4, PRD-001).

### 4.6 Outbound webhooks (PRD-005 section 7; delivery is RFC-007)

| Operation | Method + path | Min role | Auth | Gated? |
|-----------|---------------|----------|------|--------|
| List webhooks | `GET /webhooks` | admin | both | no |
| Get webhook | `GET /webhooks/{id}` | admin | both | no |
| Create webhook (signing secret shown once) | `POST /webhooks` | admin | both | yes (paid-tier feature) |
| Update webhook (URL, event types, enabled) | `PATCH /webhooks/{id}` | admin | both | no |
| Rotate signing secret (new secret shown once) | `POST /webhooks/{id}/rotate-secret` | admin | both | no |
| Delete webhook | `DELETE /webhooks/{id}` | admin | both | no |

This RFC owns only the management CRUD. The delivery, retry, backoff, signing format, and replay protection are RFC-007 / PRD-005 section 7. A registration holds: `url`, selected `event_types`, `enabled`, the signing secret (write-only, `signing_secret_set` on read), `created_by`, and `last_delivery_status`. Outbound webhooks are a paid-tier feature, so creating one on a Free org returns `entitlement_exceeded` (section 7).

### 4.7 Billing (PRD-006)

| Operation | Method + path | Min role | Auth | Gated? |
|-----------|---------------|----------|------|--------|
| Read plan, seats, usage meters, overage state | `GET /billing` | admin | both | no |
| Manage billing (plan, payment method, invoices) | not available via API | owner | session, UI-only | n/a |

Billing read is admin+. Billing management is owner-only and stays UI-only by design: no owner keys are issued, so a leaked key cannot change the plan or run up charges (PRD-005 section 4.7, master 16.5). This is the role gate doing exactly what it should: every key fails the owner check.

### 4.8 Regions catalog (PRD-007)

| Operation | Method + path | Min role | Auth | Gated? |
|-----------|---------------|----------|------|--------|
| List regions available to the org (plan entitlement) | `GET /regions` | read | both | no |

Returns the region codes the org's plan includes, so clients pick valid codes before a monitor write (PRD-005 section 4.5, 8).

### 4.9 Audit log (PRD-001)

| Operation | Method + path | Min role | Auth | Gated? |
|-----------|---------------|----------|------|--------|
| List audit events (cursor, filter by actor/action/range) | `GET /audit-events` | admin | both | no |

The audit list is a big list; section 12 covers its pagination and filters.

### 4.10 Meta endpoints (not versioned resources)

| Operation | Method + path | Auth | Purpose |
|-----------|---------------|------|---------|
| Health / liveness | `GET /healthz` | none | k8s liveness; returns 200 if the process is up |
| Readiness | `GET /readyz` | none | k8s readiness; 200 only when Postgres + Redis reachable |
| OpenAPI spec | `GET /api/v1/openapi.json` | none | the served spec for this version (section 8) |
| Swagger UI | `GET /api/docs` | none (try-it needs a key) | interactive docs (section 9) |
| JWKS | `GET /.well-known/jwks.json` | none | RS256 public keys (RFC-003 section 3.5) |

`/healthz` and `/readyz` are operational endpoints (RFC-010), not part of the resource contract, and are unauthenticated so the orchestrator can probe them.

---

## 5. Auth surface

Auth internals live in RFC-003. This section fixes the wire-level URLs and headers so the SPA and external clients agree.

### 5.1 Auth-flow endpoints (browser/session, not versioned)

These are the OAuth + session endpoints served by api (RFC-003 sections 2, 3, 4). They live under `/auth`, outside `/api/v1`, because they are session infrastructure, not the resource API.

| Operation | Method + path | Auth | Purpose |
|-----------|---------------|------|---------|
| Begin OAuth login | `GET /auth/{provider}/login` | none | redirects to Google/GitHub with PKCE + state (RFC-003 section 2.2). `{provider}` is `google` or `github`. Optional `return_to`. |
| OAuth callback | `GET /auth/{provider}/callback?code&state` | none (state-checked) | exchanges the code, creates/links the user, sets the cookies, redirects (RFC-003 section 2.5) |
| Current user (claims for render) | `GET /api/v1/me` | session | returns the non-secret user claims (email, sub, name) and "orgs I belong to" for the SPA to render; never returns the token (RFC-003 section 4.4). The canonical bootstrap path is `/api/v1/me` (it lives under the versioned surface; the rest of this group stays under `/auth`); RFC-003 and RFC-013 cite this path |
| Refresh | `POST /auth/refresh` | refresh cookie | rotates the refresh token, issues a fresh access token (RFC-003 section 4) |
| Logout (this device) | `POST /auth/logout` | session | revokes the one refresh family, clears cookies on this device (RFC-003 section 4.3) |
| Log out all devices | `POST /auth/logout-all` | session | revokes every refresh token for the user (PRD-001 AC13) |
| List sessions | `GET /auth/sessions` | session | the active refresh families with device/UA/IP for the sessions UI (RFC-003 section 4.1) |

The refresh cookie (`pulse_rt`) is path-scoped to `/auth`, so it is only sent to these endpoints, never on ordinary `/api/v1` calls (RFC-003 section 4.4). The access token (`pulse_at`) and CSRF token (`pulse_csrf`) cookies are scoped to `/`.

### 5.2 Session (browser) auth

| Aspect | Decision |
|--------|----------|
| Credential | the `pulse_at` httpOnly cookie carrying the RS256 access JWT (RFC-003 section 4.4). Same-origin, so no Authorization header from the SPA. |
| CSRF | double-submit: the SPA reads the non-httpOnly `pulse_csrf` cookie and echoes it in an `X-CSRF-Token` header on every unsafe (non-GET) request (RFC-003 section 4.5). |
| Token in the spec | the OpenAPI security scheme for the SPA is a cookie scheme; the public-API scheme is the bearer key (section 5.3). Both are declared so Swagger UI offers the right input. |

A non-browser caller that holds a JWT may instead send `Authorization: Bearer <jwt>`; the SPA does not, but the header path stays available (RFC-003 section 4.4). It is not the primary public-API model: the public API uses keys.

### 5.3 API-key auth and the canonical prefix

| Aspect | Decision |
|--------|----------|
| Header | `Authorization: Bearer pulse_sk_<secret>` (RFC-003 section 5.1). |
| Resolution | the key verify returns `(org_id, role, key_id)`, so org, role, and credential are resolved from the key alone with no token and no org parameter (RFC-003 section 5.4). |
| 401 vs 403 | missing/malformed/unknown/revoked key is `401 unauthenticated`; a valid key whose role is too low is `403 forbidden`. The distinction is kept strict (PRD-005 section 2.2). |

Resolving the prefix conflict: PRD-005 examples use `pulse_live_` and RFC-003 standardizes on `pulse_sk_` ("secret key") and flagged the difference for this RFC to settle. The canonical prefix is `pulse_sk_`. The OpenAPI spec, Swagger UI examples, the docs site, and the create-key response all use `pulse_sk_`. The PRD-005 `pulse_live_` spelling is superseded; any doc, example, or fixture still showing `pulse_live_` must be updated to `pulse_sk_`.

Reasoning: RFC-003 owns the key format and already picked `pulse_sk_`, with a clear rationale ("secret key", and it leaves room for a future `pulse_pk_` publishable-key idea without a naming clash, unlike the Stripe-style `live`/`test` axis which we do not have since keys are not environment-scoped). Picking the auth RFC's choice keeps the format definition single-sourced. The behavior is identical either way; only the literal string differs, and we converge on one.

### 5.4 Org-context URL structure

This is the one place the SPA and the public API differ in shape, and it must be consistent.

| Caller | How org arrives | URL shape |
|--------|-----------------|-----------|
| SPA / session (JWT) | explicit in the path, primary mechanism (RFC-003 section 6.2) | `/api/v1/orgs/{org_id}/<resource>` e.g. `/api/v1/orgs/org_42/monitors` |
| SPA / session, non-path-shaped call | `X-Pulse-Org: org_<id>` header, the accepted alternate | `/api/v1/<resource>` + the header |
| Public API (key) | fixed by the key, no org parameter read | `/api/v1/<resource>` e.g. `/api/v1/monitors` |

The resource paths in section 4 are written without the `/orgs/{org_id}` prefix because that prefix is the SPA's org-context mechanism, not part of the resource identity. The same handler serves both shapes: for a key request the org comes from the key and any `/orgs/...` segment or `X-Pulse-Org` header is ignored (the key cannot be asked to act in another org, RFC-003 section 5.4); for a session request the org comes from the path (or the header) and is always checked against the user's membership before it is trusted (RFC-003 section 6.2). Supplying an org you are not a member of is a `403`, never access.

The `{org_id}` in the path is the prefixed external id (`org_42`), decoded by the codec (section 3) like any other id.

---

## 6. Rate limiting

Per-key, per-plan token bucket (PRD-005 section 2.4, RFC-009 owns the counter storage in Redis).

| Aspect | Decision |
|--------|----------|
| Scope | per key, not per org, so one noisy key does not starve another (PRD-005 section 2.4). |
| Tier shape | the per-key ceiling scales with the org's plan: Free ~30 req/min reads-only, Starter ~120, Team ~300, Business ~600 (indicative; PRD-006 tunes the numbers). |
| Follows entitlement | a downgrade lowers the cap on the next request once the cached entitlement invalidates (master 11). |

Every response carries:

| Header | Meaning |
|--------|---------|
| `X-RateLimit-Limit` | the per-key ceiling for the window |
| `X-RateLimit-Remaining` | requests left in the current window |
| `X-RateLimit-Reset` | when the window resets; epoch seconds (fixed here for the spec, since `Retry-After` is already seconds-based and a uniform integer is simpler for clients than mixing RFC3339 here) |

On exceed: `429 Too Many Requests`, the standard envelope with `code: "rate_limited"`, and a `Retry-After: <seconds>` header. The rejected call has no side effect; the client backs off and retries (PRD-005 AC5, edge cases).

Note: PRD-005 section 3.6 left `X-RateLimit-Reset` as "RFC3339 or epoch seconds, fixed in the spec." This RFC fixes it to epoch seconds. Deviation from the general RFC3339-everywhere rule, scoped to this one header for client simplicity and consistency with `Retry-After`.

---

## 7. Entitlement and validation errors on write

Every write passes through two independent gates, both of which must pass (PRD-005 section 9.2, RFC-003 section 7.4):

1. Role gate: the actor's role vs the operation's min-role (section 4). Failure is `403 forbidden`.
2. Entitlement gate: the org's plan allows the write (RFC-000 section 12, RFC-009). Failure is `403 entitlement_exceeded`.

### 7.1 How each metered limit surfaces

The metered reasons are binding from RFC-000 section 12. How each appears on the wire:

| Reason | Trigger | HTTP + code | Where the detail goes |
|--------|---------|-------------|-----------------------|
| `monitor_limit_reached` | create monitor over the plan cap | 403 `entitlement_exceeded` | `message` with an upgrade hint |
| `interval_below_plan_floor` | `interval_seconds` below the plan floor | 422 `validation_failed` (a field-level rule) | `fields.interval_seconds` e.g. "must be >= the plan floor of 300" |
| `interval_below_hard_floor` | `interval_seconds` below the 30s hard floor | 422 `validation_failed` | `fields.interval_seconds` (the hard floor overrides any plan) |
| `region_not_in_plan` | a `regions` entry not in the plan | 403 `entitlement_exceeded` | `fields.regions` |
| `region_count_exceeded` | more regions than the plan allows | 403 `entitlement_exceeded` | `fields.regions` |
| `seat_limit_reached` | invite over the seat cap | 403 `entitlement_exceeded` | `message` with an upgrade hint |
| `status_page_limit_reached` | create status page over the cap | 403 `entitlement_exceeded` | `message` |
| `custom_domain_not_in_plan` | (no v1 endpoint; reserved) | 403 `entitlement_exceeded` | n/a in v1 |
| `api_write_not_in_plan` | any write from a Free-tier key | 403 `entitlement_exceeded` (PRD-005 calls it `forbidden`; see note) | `message` with an upsell |
| `api_rate_limited` | per-key rate exceeded | 429 `rate_limited` | `Retry-After` header (section 6) |

Note on Free-tier writes: PRD-005 section 9.1 says a Free write returns `403` with `code: "forbidden"`. RFC-000 section 12 calls the underlying reason `api_write_not_in_plan`, which is an entitlement fact, not a role fact. This RFC returns `403` with `code: "entitlement_exceeded"` (the entitlement gate, with `api_write_not_in_plan` in the message) so a client can render the upsell consistently with every other entitlement rejection, rather than treating Free-write specially as a role failure. Deviation from PRD-005's literal `forbidden`, flagged; the HTTP status (403) is the same, only the `code` differs, and `entitlement_exceeded` is the more accurate and more upsell-friendly choice (master 11 wants every metered rejection to carry an upsell).

The distinction that matters: a floor violation is a per-field validation error (`422`, the client can fix the value), while a cap/seat/region/plan-feature violation is an entitlement rejection (`403`, the client must upgrade). A field error means "your input is wrong"; an entitlement error means "your plan does not allow this." Both carry the envelope; clients branch on `code`.

### 7.2 Plain validation

Field validation rules from PRD appendix A (URL shape, header limits, name length, interval bounds, valid region codes, valid `down_policy` enum) surface as `422 validation_failed` with a `fields` map naming each bad field (PRD-005 AC6). Validation runs before the entitlement gate where both could fire, so a malformed request is told it is malformed rather than told to upgrade.

---

## 8. OpenAPI 3 as source of truth

### 8.1 Decision: spec-first, with codegen and a drift check

The OpenAPI 3 spec is the single source of truth (master 9, PRD-005 section 5). We author the spec first, generate the server-side request/response types and the route registration stubs from it, and a CI check fails the build if the running router and the spec disagree.

| Approach | Verdict | Reasoning |
|----------|---------|-----------|
| Spec-first + codegen + CI drift check (chosen) | chosen | The spec is a real artifact reviewed in PRs, so the contract is designed deliberately, not as a byproduct of handler annotations. Codegen of types/stubs means the handlers cannot silently diverge from the declared schemas (a missing field is a compile error). The CI drift check (8.3) closes the last gap: a route added without a spec entry, or a spec entry with no route, fails the build. This is the strongest no-drift story and it is what makes PRD-005's "the spec served at `/api/docs` lists every operation in section 4" testable (AC13). |
| Code-first: annotate Go handlers, generate the spec | rejected | The spec becomes a derived artifact, harder to review as a contract and easy to let drift through forgotten or wrong annotations. The contract is then whatever the code happens to emit, which inverts "spec is the source of truth." Annotations also scatter the contract across handler files instead of one reviewable document. |
| Hand-write the spec, hand-write handlers, no codegen | rejected | No mechanical link between spec and code, so they drift the moment someone edits one and not the other. Exactly the failure mode PRD-005 section 6.2 exists to prevent. |

We pay a small cost: the spec must be edited before the handler, and the codegen step is in the build. That ordering is the point: the contract is decided first.

### 8.2 Where the spec lives

| Item | Location |
|------|----------|
| Source spec | `api/openapi/v1.yaml` in the api repo module, version-controlled, reviewed in PRs |
| Served spec | `GET /api/v1/openapi.json` (the YAML rendered to JSON at build, embedded in the binary so the served spec always matches the running build) |
| Generated types/stubs | generated into `internal/apigen` at build, not hand-edited, not committed as the contract (committed only as a build artifact if at all) |
| A future v2 | `api/openapi/v2.yaml`, served at `/api/v2/openapi.json`, its own lifecycle |

### 8.3 Keeping the spec and the routes in sync (the CI check)

A CI job (RFC-000 section 11.4) runs on every PR:

1. Generate types/stubs from `v1.yaml`; the build fails if a handler's signature no longer matches the generated request/response type (a renamed or removed field).
2. Reflect the actual registered router and diff its `(method, path)` set against the spec's `(method, path)` set. A route present in one but not the other fails the job. This catches "added an endpoint, forgot the spec" and "removed a spec entry, left the route."
3. Lint the spec (spectral or equivalent) so the envelope, the pagination shape, the `code` enum, the security schemes, and the rate-limit headers are present and consistent across operations.

This is what makes the no-drift guarantee a build-time fact rather than a review hope, and it is the mechanical backing for PRD-005 AC13.

### 8.4 Versioning the spec with the API

The spec version tracks the API major version (section 2.1). `/api/v1` is one spec file with an `info.version` that bumps on additive releases (so docs can show "as of x.y") but the path stays `/api/v1`. A breaking change is a new spec file under `/api/v2`, served alongside v1 for the deprecation window. The two specs are independent documents.

---

## 9. Swagger UI

The api service serves interactive Swagger UI at `/api/docs` (master 9, PRD-005 section 5).

| Aspect | Decision |
|--------|----------|
| Source | renders the served `GET /api/v1/openapi.json`, so it always matches the running build (8.2). |
| Try-it-out | real calls against the developer's own org, subject to auth, RBAC, rate limits, and entitlements like any other client (PRD-005 section 5). No sandbox, no mock. |
| Auth in the explorer | the bearer security scheme lets a developer paste `pulse_sk_<secret>` once and run authenticated calls. The cookie scheme is declared for the SPA but the explorer's primary path is the key (a developer reading `/api/docs` is the API audience). |
| Access | `/api/docs` itself is unauthenticated to read (it is just the rendered spec); the calls it makes need a key. |

This is the "time-to-first-API-call in minutes" surface (PRD-005 goal): create a key, open `/api/docs`, paste it, run a call.

---

## 10. Docs site and GitHub Pages

A static documentation site and pricing page on GitHub Pages (master 10, PRD-005 section 6). Static so it is cheap and stays up independently of the api service.

| Surface | What it is | Statically generated? |
|---------|------------|-----------------------|
| Product guides | getting started, create a monitor, connect a channel, read incidents, set up a status page, use the API from CI, set up outbound webhooks and verify signatures | yes, authored markdown |
| API reference | every resource, operation, schema, error code, and the conventions, generated from `v1.yaml` | yes, build artifact of the spec |
| Pricing page | the plan tiers and what each includes, mapped to PRD-006 / master 11 | yes, authored, kept in step with the plan catalog |
| Conventions reference | versioning + deprecation, error envelope, cursor pagination, RFC3339, idempotency, rate-limit headers | yes, authored |
| Webhook reference | event types, payload shapes, signature verification, retry/backoff, replay protection | yes, authored (cross-links RFC-007) |

| Surface | Live (served by api) | Static (GitHub Pages) |
|---------|----------------------|------------------------|
| Swagger UI `/api/docs` | yes, interactive try-it | no |
| API reference | no | yes, indexable, linkable |

### 10.1 The no-drift CI job

On each release a CI job regenerates the static API reference from the same `v1.yaml` and publishes it to GitHub Pages (master 10, PRD-005 section 6.2). The published reference is a build artifact of the spec, not hand-maintained, so the public docs never drift from the live API. Swagger UI (live) and the GitHub Pages reference (static) come from the same spec, so PRD-005 AC13 ("the GitHub Pages reference built from the same spec matches the served spec") holds by construction.

---

## 11. Outbound webhooks API (management only)

The org-webhook management endpoints are in section 4.6. This RFC owns the CRUD shape only: register, list, get, update, rotate-secret, delete. Everything about delivery (at-least-once, retry, backoff, the `X-Pulse-Signature: t=<ts>,v1=<hmac>` format, replay window, dedup on `event_id`, the payload envelope) is PRD-005 section 7 and RFC-007.

| API-side fact this RFC fixes | |
|------|------|
| Resource | `/webhooks`, external id prefix `wh_` (section 3) |
| Min role | admin for all operations (PRD-005 section 4.6) |
| Secret handling | the signing secret is write-only; create and rotate-secret return it once, reads show `signing_secret_set` (section 2.7) |
| Entitlement | creating a webhook is a paid-tier feature; a Free org gets `entitlement_exceeded` (section 7) |
| Rotate | `POST /webhooks/{id}/rotate-secret` returns a fresh secret once and invalidates the old one |
| Fields on read | `url`, `event_types`, `enabled`, `signing_secret_set`, `created_by`, `created_at`, `last_delivery_status` |

---

## 12. Pagination, filtering, sorting for the big lists

The general pagination contract is section 2.6. The big lists need a fixed keyset and filters so the cursor is stable and clients can narrow.

| List | Default sort | Cursor keyset | Filters |
|------|--------------|---------------|---------|
| `GET /monitors/{id}/results` | `checked_at` desc | `(checked_at, id)` | `range` (24h/7d/30d), `region` (a region code) |
| `GET /incidents` | `started_at` desc | `(started_at, id)` | `status` (open/closed), `monitor_id` |
| `GET /monitors/{id}/incidents` | `started_at` desc | `(started_at, id)` | `status` |
| `GET /audit-events` | `created_at` desc | `(created_at, id)` | `actor`, `action`, `range` (24h/7d/30d) |
| `GET /monitors`, `/channels`, `/status-pages`, etc. | `created_at` desc | `(created_at, id)` | resource-specific where useful |

### 12.1 The `range` filter semantics

`range` is a fixed enum: `24h`, `7d`, `30d` (PRD-005 section 4.1, 8). It selects rows whose time column (`checked_at` for results, `created_at` for audit) falls within that window ending at now, in UTC. It is a convenience over a full `from`/`to` pair, which v1 does not offer for these lists; the three windows match the product's retention and chart tiers. A `range` value outside the enum is a `422 validation_failed` with `fields.range`. `range` and the cursor compose: the cursor pages within the selected window.

### 12.2 Sorting

v1 fixes the sort per list (the table above); there is no client-chosen sort field in v1. This keeps the cursor keyset well-defined and the indexes (RFC-001 section 6.5) aligned with the one sort order each list uses. Client-chosen sort is a deferred, additive change.

---

## 13. Open questions and dependencies

### 13.1 Open questions

| # | Question | Recommended default |
|---|----------|---------------------|
| Q1 | Is `Idempotency-Key` required or optional in v1? | Optional but recommended, remembered 24h (PRD-005 11.1). Tighten to required per-endpoint later if duplicate-create reports appear. |
| Q2 | Per-endpoint key scopes vs role-scoping? | Role-scoping only in v1 (member/admin); per-endpoint scopes deferred to phase 3 (PRD-005 11.2, RFC-003). |
| Q3 | Should we add an org-level rate cap on top of per-key? | Per-key only in v1; add an org ceiling if metering shows aggregate abuse (PRD-005 11.3). |
| Q4 | `X-RateLimit-Reset` format. | This RFC fixed it to epoch seconds (section 6). Confirm with RFC-013 that the SPA renders it fine. |
| Q5 | Free-write `code`: `forbidden` (PRD-005) vs `entitlement_exceeded` (this RFC). | This RFC uses `entitlement_exceeded` (section 7.1). Needs a one-line PRD-005 update to converge. |
| Q6 | Validation HTTP status: `400` (PRD-005 prose) vs `422` (this RFC). | This RFC uses `422` for body validation and `400` for unparseable requests (section 2.3). Needs PRD-005 alignment; the `code` is the contract regardless. |
| Q7 | When (if ever) to swap the id codec to a keyed/opaque encoding. | Decimal in v1; swap to sqids/hashids if product wants opacity (section 3.3). No schema change needed, so it can wait for a real ask. |

### 13.2 Deviations flagged

| Deviation | From | Resolution |
|-----------|------|------------|
| Canonical API-key prefix is `pulse_sk_` | PRD-005 examples use `pulse_live_` | This RFC and RFC-003 both pick `pulse_sk_`; PRD-005 examples and any fixtures must update (section 5.3). |
| Validation is `422`, parse errors are `400` | PRD-005 prose implies `400` for validation | The `code` (`validation_failed` / `bad_request`) is the contract; the status is a hint (section 2.3). |
| Free-write rejection is `entitlement_exceeded` | PRD-005 section 9.1 says `forbidden` | More accurate and more upsell-friendly; same `403` status (section 7.1). |
| `X-RateLimit-Reset` is epoch seconds | PRD-005 left it open (RFC3339 or epoch) | Fixed to epoch seconds for client simplicity (section 6). |

### 13.3 Dependencies

| Depends on | For what |
|------------|----------|
| RFC-000 | service catalog (api owns this surface), section 12 entitlement enforcement and the metered-limit codes, the conventions |
| RFC-001 | the BIGINT internal id and the prefix-to-table map this RFC's codec encodes (section 3) |
| RFC-003 | OAuth/JWT/refresh/key internals, the `pulse_sk_` format, the org-context mechanism, the RBAC matrix, the auth-flow endpoint behavior |
| RFC-009 | the entitlement model, the rate-limit counter storage, the limit values the gate enforces |
| PRD-005 | the product contract for the whole surface, the docs/pricing site, the webhook product behavior |
| PRD-002/003/004/006/007 | the per-resource fields and behavior for monitors, channels, status pages, billing, regions |

### 13.4 Depended on by

| Consumer | For what |
|----------|----------|
| RFC-013 (frontend) | the entire `/api/v1` contract, the `/orgs/{org_id}` path shape, the error envelope, the cookie + CSRF scheme, the `/api/v1/me` claims shape |
| External developers | the public API, the spec at `/api/docs`, the GitHub Pages reference |
| RFC-007 (notifier/webhook delivery) | the webhook registration shape this RFC's management endpoints produce |
