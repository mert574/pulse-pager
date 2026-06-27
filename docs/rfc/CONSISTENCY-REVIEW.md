# Cross-RFC Consistency Review - Pulse

Status: RESOLVED
Reviewer: Principal architecture / editor
Scope: master `RFC-000` plus RFC-001 through RFC-013, checked against each other, against the master RFC-000 seams, and against the PRD layer (`PRD.md`, `prd/PRD-001..007`).

This review checks the seams between the sub-RFCs for contradictions, gaps, and broken cross-references. Mechanical errors (where the canonical decision is already settled elsewhere in the doc set and only a straggler doc disagrees) were fixed in place. Substantive conflicts that need a real decision are flagged for a lead, not decided here. Each item is tagged [FIXED], [CONFLICT], or [GAP].

Summary counts: 5 FIXED, 4 CONFLICT, 3 GAP. Everything else is a clean bill (listed at the end).

All thirteen sub-RFCs carry "DRAFT for review" in their own headers. This is the review.

---

## Seam 1 - API key hashing algorithm (RFC-001 vs RFC-003)

[FIXED] F-1: RFC-001 section 4.6 stated the `api_keys.key_hash` column was "HASHED (argon2id of presented secret)". RFC-003 section 5.2 decisively chose plain SHA-256 and explicitly rejected bcrypt and argon2, with the reasoning that an API key is 128 bits of uniform random secret (nothing to brute-force offline), so a slow password hash burns CPU on the high-rate verify path for zero security gain. RFC-001 itself says (section 13 dependency table) that "RFC-003 owns ... hashing; RFC-001 owns the columns", so RFC-003 is canonical here. Fixed the RFC-001 comment to "HASHED (SHA-256 of presented secret; RFC-003 5.2 explains why not bcrypt/argon2)".

---

## Seam 2 - refresh_tokens schema (RFC-001 vs RFC-003)

[FIXED] F-2: RFC-003 section 4.1 designs refresh-token rotation and reuse-detection on a token "family": every refresh rotates the token within the same `family_id`, and presenting a token that was already rotated (`replaced_by` is set) revokes the whole family. RFC-003 explicitly names `family_id` and `replaced_by` as columns and says "RFC-001 owns the table". But the RFC-001 `refresh_tokens` DDL did not have those two columns, so the schema could not support the rotation design. Added `family_id BIGINT NOT NULL` and `replaced_by BIGINT REFERENCES refresh_tokens(id)` to the RFC-001 DDL with comments pointing at RFC-003 4.1.

---

## Seam 3 - region_health status values (RFC-001 vs RFC-002 vs RFC-008)

[FIXED] F-3: RFC-001 section 4.5 had `region_health.status CHECK (status IN ('healthy','unhealthy'))` (two values). RFC-002's `region.health` wire schema (section 4.6) and RFC-008's classification (section 5.2) both use three values: `healthy` / `degraded` / `unhealthy`, where `degraded` is the failover early-warning state. RFC-008 section 5.2 flags this as a deviation and recommends widening the RFC-001 CHECK; it is a one-line additive change owned by RFC-001. Applied: widened the CHECK to `('healthy','degraded','unhealthy')`. For the down-policy denominator `degraded` and `unhealthy` are still treated the same (both exclude the region from `R`); the third value only matters for failover timing, exactly as RFC-008 states.

---

## Seam 4 - API key prefix string (PRD-005 / RFC-000 / RFC-001 vs RFC-003 / RFC-012)

[FIXED] F-4: The literal key prefix appeared two ways. PRD-005 (2.1, 2.2), RFC-000 (section 7.3), and RFC-001 (api_keys prefix comment) used `pulse_live_`; RFC-003 (section 5.1) and RFC-012 (section 5.3) standardized on `pulse_sk_`. RFC-012 is the public-API single source of truth and states outright that "the canonical prefix is `pulse_sk_` ... any doc, example, or fixture still showing `pulse_live_` must be updated". So the decision is already made; the stragglers just needed updating. Fixed the four straggler occurrences (PRD-005 2.1 and 2.2, RFC-000 7.3, RFC-001 api_keys comment) to `pulse_sk_`.

Note: the remaining `pulse_live_` mentions in RFC-003 (5.1, Q1) and RFC-012 (5.3, deviation table) are the prose that records why `pulse_sk_` won and the instruction to update the others. They are correct to keep as the audit trail. The open questions RFC-003 Q1 and RFC-013 Q4 (both "which prefix wins?") are now answered: `pulse_sk_`, and every doc agrees.

---

## Seam 5 - RFC-000 RFC index dependency (RFC-000 section 13)

[FIXED] F-5: In the RFC-000 section 13 index, the RFC-006 (Alerting) entry read "Depends on: ... RFC-007 (down-policy + probe health)". Down-policy aggregation and probe-fleet health are owned by RFC-008 (Multi-Region and Probe Fleet), not RFC-007 (Notifier). RFC-006 itself (section 1.1) calls this out as an "index typo in RFC-000 section 13", and the RFC-008 index entry correctly lists "Depended on by: RFC-006 (verdict)". Fixed the RFC-006 dependency to RFC-008. The "Depended on by: RFC-007" half of the same line is correct and was left (alerting produces `notify.events`, which the notifier consumes).

---

## Seam 6 - Worker result-persistence path (RFC-000 vs RFC-005 vs RFC-006, also master PRD 6.6)

[CONFLICT] C-1 (substantive, needs a lead decision): The master architecture says the worker writes `check_results` to Postgres directly AND emits `check.results` to Kafka. This is stated in RFC-000 section 2.3 (worker "writes the check result to Postgres, emits check.results"), reinforced in RFC-000 section 5.3 (the worker's idempotency rule is the result-row write), and in master PRD 6.6 ("workers ... write the check result to PostgreSQL, and emit a check-result event").

RFC-005 section 5.3 deliberately changes this: the worker emits `check.results` to its regional Kafka only and does NOT write Postgres; a control-plane consumer, folded into the alerting transaction (RFC-006 section 5.4), does the idempotent `check_results` upsert (getting `result_id` locally) and then applies the verdict in the same transaction. RFC-005 flags this explicitly as "a deliberate, flagged deviation from the literal wording of RFC-000 section 2.3" and asks RFC-000/RFC-006 to confirm. RFC-006 section 5.4 implements its half consistently.

So RFC-005 and RFC-006 agree with each other but both diverge from RFC-000 and the master PRD. This is the one genuine architectural seam that is open. The sub-RFCs' reasoning is good: one write path instead of two means the durable row and the alerting trigger cannot diverge under partial failure, and the worker stays off the Postgres hot path (it already does not read Postgres). Recommendation: ratify the RFC-005/RFC-006 model and update RFC-000 section 2.3 / 5.3 (and the one product-level sentence in master PRD 6.6) to match, OR reject it and have RFC-005 revert to a direct worker write. Either way RFC-000 and the two sub-RFCs must end up saying the same thing. Lead to decide.

[RESOLVED] Ratified the RFC-005/RFC-006 emit-only model: the worker emits `check.results` to its regional Kafka and does not write Postgres; a single control-plane consumer does the idempotent `check_results` upsert keyed `(org_id, monitor_id, region, checked_at)` and applies the verdict in the same transaction. Edited RFC-000 section 2.3 (worker responsibility, writes row, and the Decision paragraph) and section 5.3 (worker/alerting idempotency rows) to make the row write the consumer's, and master `PRD.md` section 6.6 to "emit a check-result event; the platform persists the result." Added the timing nuance in RFC-006 section 5.4 (each row upserted promptly on consume so history/latency views are not delayed, verdict applied at round close). Also corrected the matching stragglers in RFC-002 sections 3 (`check.results` retention note), 6.2 and 6.3 (worker emit + control-plane upsert). RFC-005 and RFC-006 core design unchanged.

---

## Seam 7 - `scheduled_at` on `check.results` (RFC-006 vs RFC-002)

[GAP] G-1: RFC-006 section 3.1 needs `scheduled_at` as a first-class RFC3339 field on the `check.results` event so the multi-region aggregation window can group all regions of one tick (`agg:{monitor_id}:{scheduled_at}`) and close the round deterministically. RFC-002's `check.results` schema (section 4.4) does not list `scheduled_at` as a top-level field (it carries `job_id`, which embeds `scheduled_at_unix`, but RFC-006 wants it as an explicit field, not parsed out of the job id). RFC-006 section 11 raises this as an ask to RFC-002. It is an additive, agreed change but it belongs to RFC-002 (the eventing contract owner) to land with the right type and position. Recommendation: RFC-002 adds `scheduled_at` (RFC3339 UTC) to the `check.results` schema; RFC-006 and RFC-008 (which also keys its round window off `scheduled_at`) then reference the explicit field.

[RESOLVED] Added it. Edited RFC-002 section 4.4: `scheduled_at` is now a top-level RFC3339 UTC field on the `check.results` schema (in the JSON example and the field table), with a note that RFC-006/RFC-008 key the per-tick multi-region aggregation window off it. `job_id` is kept.

---

## Seam 8 - Bootstrap endpoint path (RFC-012 vs RFC-013)

[CONFLICT] C-2 (low severity, one path to pin): RFC-012 section 5.1 references `/auth/me` as the session bootstrap endpoint (carried from RFC-003); RFC-013 section 3.2 uses `/api/v1/me` and flags it as open question Q1 ("RFC-003 references `/auth/me`; this RFC uses `/api/v1/me`. Which is canonical?"). The two RFCs literally name different paths for the same call. Not dangerous, but the SPA and the OpenAPI spec must agree on one. Recommendation: RFC-012 (API contract owner) pins one path and RFC-003/RFC-013 cite it. `/api/v1/me` reads more consistent with the rest of the versioned surface; `/auth/me` keeps it next to the other `/auth/*` routes. Lead/API owner to pick.

[RESOLVED] Canonical path is `/api/v1/me`. Edited RFC-012 section 5.1 (me row now `GET /api/v1/me`, stated as canonical, the rest of the auth group stays under `/auth`) and its RFC-013 dependency line. Edited RFC-003 (two `/auth/me` prose spots in sections 4.4 and Q4 now `/api/v1/me`). Edited RFC-013 section 3.2 prose, marked Q1 and the deviation row RESOLVED to `/api/v1/me`. (RFC-003 Q1 is the key-prefix question, already resolved in F-4, untouched here.)

---

## Seam 9 - check-now job id format (RFC-004 vs RFC-005 / RFC-002)

[CONFLICT] C-3 (low severity): RFC-004 section 9.2 proposes the check-now job id `<monitor_id>:<region>:checknow:<now_unix>` so a manual check cannot collide with a scheduled tick's `<monitor_id>:<region>:<scheduled_at_unix>` id, and flags it (open question 3) for RFC-005 (worker) and RFC-002 (dedup token) to confirm. Neither RFC-005 nor RFC-002 actually confirms the format; they defer the full dedup token to "RFC-002". So the format is proposed but unratified. Recommendation: RFC-002 records the check-now job id shape in its idempotency-key contract (section 6.2) so the worker's dedup write has one definition to rely on. Low severity, but it should not stay dangling between three RFCs.

[RESOLVED] Ratified the format `<monitor_id>:<region>:checknow:<now_unix>` (RFC-004 section 9.2). Added a check-now row to RFC-002's idempotency-key contract (section 6.2) so the worker dedup write has one definition, alongside the scheduled `<monitor_id>:<region>:<scheduled_at_unix>` shape.

---

## Seam 10 - entitlement column coverage (RFC-009 vs RFC-001)

[GAP] G-2: RFC-009 section 2.2 references `ChannelTypesAllowed` as part of the entitlement set, but RFC-001's `entitlements` table (section 4.2) has no such column. RFC-009 flags this and recommends deriving the allowed channel types from the plan tier in the resolver rather than storing a column (there is no per-org channel-type override in v1). That recommendation is fine and self-consistent. Recommendation: keep it derived (no schema change) and make sure RFC-009's resolver section states it explicitly so a reader does not go looking for the missing column. No data-model change needed.

[RESOLVED] Kept DERIVED, no schema column, no per-org override in v1. Edited RFC-009 section 2.2 to state outright that the resolver derives `ChannelTypesAllowed` from the plan tier, there is no `entitlements` column and no per-org override in v1 (so a reader should not look for a missing column), and a column is only worth adding later if phased channels need a per-org grant. Marked RFC-009 Q1 RESOLVED. No RFC-001 change.

---

## Seam 11 - audit retention buffer vs system of record (RFC-002 vs RFC-001 / PRD-006)

[GAP] G-3 (clarification only, not a conflict): RFC-002 section 3 gives the `audit.events` Kafka topic a fixed 90-day retention, while RFC-001 (`audit_events`) and PRD-006 set per-tier audit retention (Free/Starter none, Team 30 days, Business 365 days). These do not conflict: RFC-002 section 3 itself says the Kafka topic is a buffer and Postgres is the system of record, so the 90-day topic retention is just a replay buffer, independent of the per-tier Postgres retention. One wrinkle worth a sentence: Business retains 365 days in Postgres but the Kafka buffer is only 90 days, which is fine (the buffer is not the store) but is easy to misread. Recommendation: leave the numbers, add a half-sentence in RFC-002 making clear the buffer length is intentionally shorter than the longest plan retention because Postgres, not Kafka, holds the durable trail.

[RESOLVED] Clarified. Edited RFC-002 section 3 (`audit.events` retention rationale) to add that the 90-day topic retention is intentionally independent of (and may be shorter than) the per-tier Postgres audit retention, because Postgres (`audit_events`) is the durable system of record, not Kafka. Numbers left unchanged.

---

## Clean bill (seams verified consistent across the RFC set, no action)

- Kafka topic names, producers/consumers, and partition keys are consistent across RFC-000, RFC-002, RFC-004, RFC-005, RFC-006, RFC-007, RFC-008 (`monitor.changed`/org_id, `check.jobs.<region>`/monitor_id, `check.results`/monitor_id, `notify.events`/monitor_id, `audit.events`/org_id, `billing.events`/org_id, `region.health`/region, `webhook.delivery`/org_id).
- Dedup-id / idempotency-key formulas agree everywhere: notify dedup `hex(sha256(incident_id, event_type))` (RFC-002, RFC-006, RFC-007 identical); check-result unique key `(org_id, monitor_id, region, checked_at)` (RFC-001, RFC-002, RFC-005, RFC-006, RFC-008); scheduler job id `<monitor_id>:<region>:<scheduled_at_unix>` (RFC-002, RFC-004, RFC-005).
- failure_reason values and priority order (`blocked_target` -> `connection_error` -> `timeout` -> `status_mismatch` -> `latency_exceeded` -> `body_assertion_failed`) match RFC-005, RFC-006, and PRD-002.
- Plan tier numbers in RFC-009 match PRD-006 and master PRD exactly: monitors 2/25/100/500, intervals 7200/300/60/60s, regions 1/2/4/6, seats 1/3/10/25, retention 7/30/90/180 days, status pages 1/1/3/10, API rate 30/120/300/600 req/min, 30s hard floor.
- Entitlement error codes are identical across RFC-009, RFC-012, RFC-000 section 12, and PRD-006 (all ten codes verbatim).
- Down-policy quorum math (`R` = healthy reporting regions, `|U| > |R|/2`, `|R| < 2` -> coverage-degraded) is consistent across RFC-006, RFC-008, PRD-002, PRD-007, and RFC-000 section 4.1.
- Coverage-degraded as an orthogonal signal (not a fifth monitor status) is consistent across RFC-008, PRD-002, PRD-004, PRD-007, and master decision 16.7.
- SLO targets (5s scheduling, 5s result-to-decision, 30s notification, 300ms/500ms API p99, 99.9% monthly) match RFC-004, RFC-006, RFC-007, RFC-008, RFC-010, RFC-011, and PRD-012.
- Leader election (k8s Lease via client-go), managed Postgres/Redis/Kafka, MirrorMaker 2, RS256 JWT, franz-go, pgx, golang-migrate, NetworkPolicy + TLS (no mesh in v1) are stated consistently across RFC-000 ADRs, RFC-002, RFC-004, RFC-011.
- API conventions (`/api/v1`, `{items, next_cursor}` with limit 100/max 500, `code`/`message`/`fields` error envelope, `Idempotency-Key` remembered 24h, `X-RateLimit-*` + 429/Retry-After, `X-Pulse-Signature: t=,v1=` webhook signing) are consistent between RFC-012, RFC-013, PRD-005, and RFC-000 section 5.3.
- Cookie/token naming (`pulse_at`, `pulse_rt`, `pulse_csrf` -> `X-CSRF-Token`), JWKS at `/.well-known/jwks.json`, Swagger UI at `/api/docs` match RFC-012 and RFC-013.
- No broken doc references: no RFC above 013, no PRD above 007, no ADR above 0010; all `master N` / `PRD-NNN section N` / `RFC-NNN section N` references resolve.
