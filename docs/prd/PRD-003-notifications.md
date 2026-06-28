# PRD-003 - Notifications

Status: DRAFTING
Owner: Product (Principal PM)
Parent: Master PRD `PRD.md` (v2.1), section 7 (notification channels) and appendix B (payloads)
Related sub-PRDs: PRD-002 Monitoring Engine (produces the down/recovery events), PRD-006 Billing & Entitlements (channel-type gating by plan), PRD-001 Identity & Tenancy (RBAC), PRD-007 Multi-Region (the verdict that triggers a notification)
Reuse: notification payloads are reused verbatim from v1 12.7 as reproduced in master appendix B. The `internal/notify` Go package carries over (PLANNING.md reuse note).

---

## 1. Overview, goals, non-goals

### 1.1 Overview

Notifications are how Pulse tells a team that one of their monitors went down or came back. A **channel** is a reusable destination (Slack, Discord, generic webhook, or email) configured once per org and attached to any number of monitors. When the alerting service opens or closes an incident (master 6.4), it emits a notification event; the `notifier` service consumes that event from Kafka and delivers the message to every channel attached to the monitor, with retry and backoff on failure (master 6.6).

This document fully specifies the product behavior of channels and delivery. It does not redesign the alerting state machine (that is PRD-002) and it does not change the payloads (those are locked, master appendix B).

This domain sits at the end of the core loop in master section 1: register endpoint -> scheduled check -> detect issue -> open incident -> **notify** -> recover.

### 1.2 Goals

- Let a team configure reusable destinations per org and attach them to monitors many-to-many (master 7).
- Deliver exactly one down message and one recovery message per incident, per attached channel, with the proven payloads (master 6.4 contract, appendix B).
- Keep channel secrets safe: encrypted at rest, write-only over the API, redacted on read (master 7, 13).
- Make delivery reliable and observable: at-least-once with retry/backoff, give-up recorded visibly, duplicates recognizable (master 6.6).
- Hit the committed delivery latency: down/recovery sent within 30s of the triggering check at p99, excluding the third-party channel's own latency (master 12).
- Let any member+ send a test message to a channel from the UI (master 4, 7).
- Make new channel types (PagerDuty, Opsgenie, SMS, Telegram, Microsoft Teams) additive behind the same attach model (master 7, 15).

### 1.3 Non-goals

- No on-call scheduling, escalation rotations, or acknowledgement-routing. Pulse integrates with PagerDuty/Opsgenie for that (master 1 non-goals, 15 phase 3).
- No re-notify-while-down in v1. One down, one up, per incident. Re-notify is a phased option, default off (master 6.4, 15).
- No per-channel message templating or custom formatting in v1. Payloads are fixed (master appendix B).
- No notification routing rules (severity-based, time-of-day, etc.) in v1.
- No status-page subscriber notifications here. That is a status-page feature, phased (master 8, PRD-004).
- No SMS/voice in v1 (phased, master 15).
- This PRD does not own org-level outbound webhooks (the events firehose into a customer's pipeline). Those are PRD-005, master 9. The per-monitor generic-webhook channel here is a notification channel, not the org-level webhook.

---

## 2. Channel entity

A channel is an org-owned, reusable notification destination. It is never shared across orgs (master 3, 13 tenant isolation).

### 2.1 Common fields

| Field | Type | Rules |
|-------|------|-------|
| `id` | string | server-generated, e.g. `chn_...`, unique per org scope |
| `org_id` | string | the owning org; every query scoped to it (master 13) |
| `name` | string | required, non-empty after trim, max 200 chars (mirrors the name rule in appendix A) |
| `type` | enum | required, one of `slack` / `discord` / `webhook` / `email` in v1; phased types added later (section 8) |
| `config` | object | type-specific, see 2.2; secret subfields encrypted at rest and redacted on read (2.3) |
| `enabled` | bool | required, default true. A disabled channel is skipped on delivery and on test (sections 4, 10) |
| `created_at` / `created_by` | RFC3339 / user ref | audit fields; channel-created is an audited action (master 13) |

The set of monitors a channel is attached to is the join model in section 3, not a field on the channel.

### 2.2 Type-specific config (the four v1 types)

Secret columns are marked. Non-secret fields are returned as-is on read; secret fields are never returned, only a `*_set` boolean (2.3).

| Type | Field | Type | Secret? | Rules |
|------|-------|------|:-------:|-------|
| `slack` | `webhook_url` | string | yes | required, Slack incoming-webhook URL (https). Write-only; read returns `webhook_url_set` |
| `discord` | `webhook_url` | string | yes | required, Discord incoming-webhook URL (https). Write-only; read returns `webhook_url_set` |
| `webhook` | `url` | string | yes | required, absolute https URL the envelope is POSTed to. Write-only; read returns `url_set` |
| `webhook` | `custom_headers` | list of {key, value} | yes (values) | optional, max ~20, non-empty keys, no duplicate keys. Sent with each request (appendix B). Values treated as secret (may carry tokens); read returns `custom_headers_set` plus the key names, never values |
| `email` | `host` | string | no | required, SMTP host |
| `email` | `port` | integer | no | required, 1..65535 (typically 587 or 465) |
| `email` | `username` | string | no | optional, SMTP auth user (blank = unauthenticated relay) |
| `email` | `password` | string | yes | optional, SMTP auth password. Write-only; read returns `password_set` |
| `email` | `from` | string | no | required, sender address mail is sent from |
| `email` | `to` | list of string | no | required, non-empty, one or more recipient addresses |
| `email` | `tls` | enum | no | required, one of `starttls` / `implicit` / `none`, default `starttls`. `none` allowed but discouraged in UI help (see 10 edge cases) |

Validation is server-authoritative, UI mirrors (same posture as appendix A). A create or update that fails these rules returns the standard per-field error envelope (`code`/`message`/`fields`, master 9 / v1 12.3).

### 2.3 Secrets, encryption, write-only, redaction

This follows the secret-class handling in master 13.

- **Secret fields**: Slack `webhook_url`, Discord `webhook_url`, generic-webhook `url` and `custom_headers` values, SMTP `password`. (SMTP `username`, `host`, `port`, `from`, `to`, `tls` are config, not secret, and are returned on read.)
- **Encrypted at rest**: AES-256-GCM with a per-value nonce, the v1 12.6 approach (master 13). A database leak does not expose channel secrets in plaintext.
- **Write-only over the API**: secret fields can be set on create and update, but are never returned by any read endpoint and never logged (master 7, 13).
- **Redaction on read**: a channel read returns, instead of each secret value, a boolean `*_set` (`webhook_url_set`, `url_set`, `custom_headers_set`, `password_set`) so the UI can show "configured" without leaking the value (master 10 screen 7: "secrets shown as configured, never the value"). For generic-webhook custom headers, the read may also return header **key names** (not values) so the user can see which headers exist.
- **Update semantics**: omitting a secret field on update leaves the stored value unchanged; sending a new value replaces it; sending an explicit empty/null clears it where the field is optional (SMTP password). A secret that is required (Slack/Discord/webhook URL) cannot be cleared without deleting the channel.

---

## 3. Attach model (monitor <-> channel)

- Channels attach to monitors **many-to-many**: one channel can serve many monitors, one monitor can have many channels (master 7).
- On the monitor side this is the `notification_channel_ids` list (master 6.2, appendix A): an optional list where each id must reference an existing channel in the **same org**; empty is allowed.
- **A monitor with zero channels still tracks but sends nothing.** It opens and closes incidents, changes status, and records history exactly as normal; it just produces no notification delivery (master 6.4, 6.5). This is a first-class, supported state, not an error.
- Attaching/detaching a channel is editing the monitor's `notification_channel_ids`. It takes effect on the next notification event; it does not retroactively send or recall messages for an already-open incident (see section 10 edge cases for channel changes mid-incident).
- A channel that is disabled or deleted is handled at delivery time (section 10), so the attach list can safely outlive a channel's enabled state.

---

## 4. Notification events and payloads

### 4.1 The two events and the contract

The alerting service emits exactly two kinds of notification event (master 6.4):

- **down**: emitted when an incident opens (the consecutive-unhealthy count reaches `failure_threshold`, master 6.4 step 6).
- **recovery**: emitted when an open incident closes on a healthy check (master 6.4 step 8).

**Contract: one down notification, one recovery notification, per incident, per attached channel.** No re-notify while a monitor stays down (master 6.4 steps 7, the contract line, and 15). A monitor closed by being disabled gets `close_reason = disabled` and **no** recovery notification (master 6.4). Re-notify-while-down is phased and default off (section 11).

One notification event fans out to all enabled channels attached to the monitor at the time the event is processed. Each (event, channel) pair is one delivery.

### 4.2 What data a notification carries

Every notification is built from the incident plus the triggering check (master 6.4, appendix B):

- monitor: id, name, url, method.
- incident: id, `started_at` (the FIRST failing check in the run that opened the incident, master 6.4), `ended_at` (null on down, set on recovery).
- check: `checked_at`, `healthy`, `failure_reason` (one of the master 6.3 reasons on down; null on recovery), `status_code`, `latency_ms`, `error`.
- `sent_at`: when the notification was produced.
- recovery only: `duration_seconds` = `ended_at - started_at` (master 6.4 recovery duration rule).

### 4.3 Payloads (reproduced verbatim from master appendix B)

These are locked. Do not change field names, order, or formatting.

#### 4.3.1 Generic webhook (POST, application/json)

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

Field types (master appendix B): `event` string (`down`/`recovery`); monitor fields strings; `incident.id` string, `started_at` RFC3339, `ended_at` RFC3339 on recovery else null; `check.checked_at` RFC3339, `healthy` bool, `failure_reason` string-or-null (one of the master 6.3 reasons; null on recovery), `status_code` integer-or-null, `latency_ms` integer-or-null, `error` short-string-or-null; `sent_at` RFC3339. The channel's configured `custom_headers` are sent with the request.

#### 4.3.2 Slack (incoming webhook)

Down:

```json
{ "text": ":red_circle: *DOWN* Prod API health\nhttps://api.example.com/health\nReason: status_mismatch (HTTP 503)\nDown since 2026-06-21 14:00:00 UTC" }
```

Recovery:

```json
{ "text": ":large_green_circle: *RECOVERED* Prod API health\nhttps://api.example.com/health\nWas down for 10m 0s (since 2026-06-21 14:00:00 UTC)" }
```

#### 4.3.3 Discord (incoming webhook)

Down:

```json
{ "content": "**DOWN** Prod API health\nhttps://api.example.com/health\nReason: status_mismatch (HTTP 503)\nDown since 2026-06-21 14:00:00 UTC" }
```

Recovery uses `**RECOVERED**` and the down-duration line (same shape as Slack recovery, with Discord markdown).

#### 4.3.4 Email (SMTP)

Subject: `[Pulse] DOWN: Prod API health` or `[Pulse] RECOVERED: Prod API health`. Plain-text body (HTML optional) with the same facts: monitor name, URL, reason + status/latency, when it went down, and on recovery the duration. Sent from the configured `from` to the configured recipients over the configured host/port with TLS per channel config.

### 4.4 Human-readable formatting rules (master appendix B, 7)

- Human-readable timestamps (Slack/Discord/email bodies) are **UTC with the `UTC` suffix**, formatted `YYYY-MM-DD HH:MM:SS UTC` (e.g. `2026-06-21 14:00:00 UTC`).
- API and webhook fields use **RFC3339** (the generic-webhook envelope above), per the master section 9 conventions.
- **Duration format** (recovery): `Xm Ys` style as shown (`10m 0s`). Longer outages extend the same compound form (hours then minutes then seconds), derived from `duration_seconds`.
- The down line names the `failure_reason` and, when present, the HTTP status: `Reason: status_mismatch (HTTP 503)`.

---

## 5. Delivery semantics (product behavior)

The `notifier` service consumes notification events from Kafka and delivers them (master 6.6). The technical design is RFC-007; this section is the product contract.

- **At-least-once delivery.** Outbound delivery is at-least-once (master 6.6). A delivery may be retried, so a receiver could in rare cases see a duplicate. We do not promise exactly-once to the third party; we promise it will arrive.
- **Idempotency-friendly identity.** Each notification carries a stable identity (incident id + event kind + channel id) so a duplicate delivery is recognizable downstream (master 6.6: "payloads carry an idempotency-friendly identity so a duplicate delivery is recognizable"). A generic-webhook receiver can dedup on that identity. The one-down/one-up dedup itself is enforced upstream in the alerting service and must be exactly-once in effect even if a check event is redelivered (master 6.6).
- **Retry with backoff.** A failed delivery (network error, timeout, provider 5xx, SMTP transient failure) is retried with exponential backoff (master 6.6). The exact backoff schedule and attempt count are RFC-007's call; the product commitment is "we keep trying for a while, spaced out, before giving up."
- **Give-up behavior.** After the retry budget is used up, the notifier stops trying that delivery and records the failure (it does not retry forever and does not silently drop). Permanent failures (e.g. a 4xx from the provider that will never succeed, an invalid SMTP recipient) may give up sooner than transient ones; the product behavior is the same: stop and record visibly.
- **Failure visibility.** A delivery that fails after retries is recorded **visibly in the incident timeline and the audit/log** (master 6.6). The incident detail shows that a notification to channel X failed so the team is not left thinking they were alerted when they were not. This is a trust requirement, not a nice-to-have.
- **Latency target.** Down/recovery is sent within **30s of the triggering check at p99, excluding the third-party channel's own latency** (master 12). Our controllable budget is 30s end-to-end across worker -> Kafka -> alerting -> Kafka -> notifier -> outbound call. Slack/Discord/SMTP latency on top of that is theirs, not ours.
- **Per-channel independence.** One channel failing does not block or fail the others attached to the same monitor. Each (event, channel) delivery succeeds or retries on its own.

---

## 6. Send-test-message

- Every channel supports **send test message** from the UI (master 7, 10 screen 7).
- **Who**: member, admin, or owner (the "Create / edit / delete channels, send test" matrix row, master 4). Viewers cannot send tests.
- **What it sends**: a clearly-labelled test notification to that one channel using the channel's real config, in the same format the channel uses for real notifications, so the user confirms the destination is wired correctly. The test message is unmistakably a test (e.g. the monitor name reads as a test/sample and the body says this is a test from Pulse) so it is never confused with a real outage.
- **Scope**: a test targets exactly one channel and one delivery attempt path; it does not touch any monitor, does not open an incident, and does not consume the one-down/one-up contract.
- **Result surfaced to the user**: the UI shows whether the test delivered or failed (and a short reason on failure, e.g. invalid webhook URL, SMTP auth rejected, connection refused) so misconfiguration is caught at setup, not during a real outage.
- A **disabled** channel: the UI either blocks the test with a clear message or sends it while making clear the channel is disabled (recommended: block, since a disabled channel will not deliver real alerts anyway). See open decision 11.4.

---

## 7. Multi-region interaction

- Notifications fire on the **monitor-level verdict**, not per region. When a monitor has more than one region, the alerting service first reduces the per-region results into a single monitor-level healthy/unhealthy verdict using `down_policy` and probe-fleet health (master 6.7, PRD-007), then drives the state machine, which emits at most one down and one recovery event per incident (master 6.4). There is never one notification per region.
- A missing result from a region Pulse runs (our own probe region degraded) is excluded from the verdict and **never triggers a notification** (master 6.7 product guarantee: "we never page you because our own probe region went down").
- **Decision: the notification may name which regions saw the failure.** The down message should be able to say which of the selected regions observed the target unhealthy (the regions that counted toward the `down_policy` verdict), because "down from eu-west and us-east, healthy from ap-south" is materially more useful to an on-call engineer than "down". Recommended rollout below.
  - The **locked v1 payloads in section 4 do not have a region field** and must not be changed (master appendix B is locked). So region detail in v1 ships only as an **additive** line in the human-readable bodies (Slack/Discord/email), never by changing the generic-webhook envelope's locked fields. For example an optional extra line `Regions: eu-west, us-east` appended after the existing lines, which does not alter any existing field.
  - The generic-webhook envelope stays byte-for-byte as appendix B in v1. If structured per-region detail is wanted in the envelope, it is an **additive, optional** field added in a later phase (additive changes do not bump the API version, master 9) and tracked as open decision 11.1, not shipped silently.
  - Single-region monitors (Phase 0/1, the only mode until multi-region GA at Phase 2, master 15) carry no region line; there is nothing meaningful to name.

---

## 8. Phased channel types

New channel types slot in behind the **same attach model** (master 7): a new `type` enum value with its own type-specific config, encrypted/redacted the same way (section 2.3), attached to monitors the same many-to-many way (section 3), delivered by the same notifier with the same retry/visibility semantics (section 5). No change to the alerting contract.

| Channel type | Phase (master 15) | Plan tiers (master 11, PRD-006) | Config (illustrative) | Notes |
|--------------|-------------------|---------------------------------|------------------------|-------|
| Slack | v1 (Phase 1) | Hobby and above (`tier2`) | `webhook_url` (secret) | shipped |
| Discord | v1 (Phase 1) | all tiers (incl. Free) | `webhook_url` (secret) | shipped |
| Generic webhook | v1 (Phase 1) | Professional and above (`tier3`) | `url` (secret) + `custom_headers` (secret) | shipped |
| Email (BYO SMTP) | v1 (Phase 1) | all tiers (incl. Free) | host/port/username/password/from/to/tls | shipped |
| PagerDuty | Phase 3 | Professional and above (master 11 "All + PagerDuty/Opsgenie") | integration/routing key (secret) | integration, not on-call rebuild (master 1, 15) |
| Opsgenie | Phase 3 | Professional and above (master 11) | API key (secret) | integration |
| SMS | Phase 3 | Custom only (per-message COGS) | phone number(s); provider creds are platform-level, not per channel | has per-message COGS, metered/limited by plan |
| Telegram | v1 (Phase 1) | all tiers (incl. Free) | bot token (secret) + chat id | shipped |
| Microsoft Teams | Phase 3 | tier-gated per PRD-006 | webhook_url (secret) | webhook-style like Slack/Discord |

Plan-tier gating of channel **types** is enforced by Billing & Entitlements (PRD-006, master 11 "Channel types" row). The notifier and the channel CRUD check the org's entitlement: creating a channel of a type the plan does not include is blocked on write with the standard per-field error and an upsell (same enforcement posture as other entitlements, master 11). Exact tier-to-type mapping for the phased types is GTM-tunable and owned by PRD-006; master 11 anchors PagerDuty/Opsgenie at Professional and above.

---

## 9. RBAC

Reused from the master permission matrix (master 4). No new roles.

| Capability | Owner | Admin | Member | Viewer |
|-----------|:-----:|:-----:|:------:|:------:|
| View notification channels (redacted, `*_set` only, never secret values) | Y | Y | Y | Y |
| Create / edit / delete channels | Y | Y | Y | N |
| Send test message | Y | Y | Y | N |
| Attach/detach channels on a monitor (via monitor edit) | Y | Y | Y | N |

- **Members are the operators**: they configure channels and send tests, matching the Team persona "let engineers configure" (master 4 design notes).
- **Viewers see redacted, never nothing**: viewers can see that channels exist and which are configured (`*_set` booleans), but never any secret value (master 4: "Viewers are read-only, including on channels (redacted, so no secret leakage)"). There is no "sees nothing" state for channels; the redacted view is the floor.
- **No one ever reads a secret value back**, regardless of role. Write-only is absolute (section 2.3, master 7/13). Even owners do not get the plaintext webhook URL or SMTP password back.
- **API keys** follow their role ceiling (member or admin, master 5): a member/admin key can CRUD channels (secrets write-only) and send tests via the API (master 9 channels surface), a viewer-equivalent does not exist for keys.
- Channel created and channel deleted are **audited actions** (master 13 audit log list), visible to owner/admin.

---

## 10. User stories, acceptance criteria, edge cases

### 10.1 User stories

- As a **member**, I create a Slack channel for #alerts and attach it to my prod monitors so the team is pinged when prod goes down.
- As a **member**, I send a test message to a newly configured SMTP channel so I confirm the mail actually arrives before I rely on it.
- As an **admin**, I see in the incident timeline that the Discord delivery failed during last night's outage, so I know the team was not actually alerted there and I can fix the webhook.
- As a **viewer** (a stakeholder), I can see that two channels are configured on the prod monitor without seeing any secret URL.
- As an **SRE**, I attach a generic-webhook channel with a signing header so Pulse down/recovery events flow into my own pipeline, deduped on the per-incident identity.

### 10.2 Acceptance criteria (testable)

- AC1 - **All four types deliver a test.** A member can create a Slack, Discord, generic-webhook, and SMTP channel and send a test from the UI for each; each test arrives at its destination in the channel's format and the UI reports success.
- AC2 - **All four types deliver a real down + recovery.** With each of the four channel types attached to a monitor, when the monitor opens an incident exactly one down message arrives, and when it recovers exactly one recovery message arrives, per channel, with the payloads byte-matching master appendix B (section 4.3) and the human-readable formatting rules (section 4.4).
- AC3 - **One-down/one-up dedup.** While a monitor stays down across many failing checks, no further down messages are sent (master 6.4). On recovery exactly one recovery message is sent.
- AC4 - **Zero-channel monitor.** A monitor with no attached channels opens/closes incidents and changes status but sends nothing (section 3, master 6.4).
- AC5 - **Secrets never returned.** A channel read (UI or API), for any role including owner, returns `*_set` booleans and non-secret config only; no secret value is ever present in any response or log (section 2.3).
- AC6 - **Redaction visibility for viewer.** A viewer can list channels and see `*_set` state but cannot create, edit, delete, or test (section 9).
- AC7 - **Disabled close, no recovery alert.** Disabling a down monitor closes its incident with `close_reason = disabled` and sends no recovery notification (master 6.4, section 4.1).
- AC8 - **Failure recorded visibly.** When a delivery fails after retries, the incident timeline and audit/log show the failed delivery with channel and reason (section 5, master 6.6).
- AC9 - **Latency.** Under normal load, down/recovery is sent within 30s of the triggering check at p99, excluding third-party latency (master 12, section 5).
- AC10 - **Type gating.** Creating a channel of a plan-excluded type is blocked on write with the standard per-field error and an upsell (section 8, master 11).

### 10.3 Edge cases

| Edge case | Behavior |
|-----------|----------|
| Channel disabled mid-incident (after down, before recovery) | The down message already went to it. On recovery, a disabled channel is skipped (no recovery message). The contract is per-incident-per-channel, evaluated at each event's processing time; disabling between events means that channel just gets fewer messages, not an error. |
| Channel deleted mid-incident | Same as disabled: the channel is gone at recovery time, so it gets no recovery message; remaining channels are unaffected. The monitor's `notification_channel_ids` referencing a deleted channel is treated as no-op at delivery (the join is cleaned up; a dangling id never errors a delivery). |
| Channel attached mid-incident (after down) | A channel attached after the incident opened does **not** receive a retroactive down message (no replay). It will receive the recovery message if it is attached and enabled when recovery fires. (See open decision 11.5 if we want the recovery without the matching down to be suppressed.) |
| Provider returns 500 (Slack/Discord/webhook) | Transient failure: retried with backoff (section 5). If it keeps failing past the retry budget, give up and record the failure visibly (incident timeline + audit/log). Other channels unaffected. |
| Provider returns 4xx (e.g. 404 dead webhook, 401 bad token) | Treated as likely-permanent: may give up sooner than a 5xx, still recorded visibly so the user fixes the channel. |
| SMTP TLS failure (handshake fails, cert invalid, STARTTLS not offered) | Delivery fails; retried per the transient/permanent classification (a handshake that can't ever succeed is permanent). Recorded visibly with a TLS-specific reason so the user can fix host/port/tls config. We do **not** silently downgrade to plaintext; the configured `tls` mode is honored. `tls: none` is the only path that sends without TLS, and the UI discourages it. |
| SMTP auth rejected | Permanent-ish failure, recorded visibly with an auth reason; user fixes username/password. |
| Generic-webhook receiver slow but eventually 200 | Counts as delivered; the receiver's own latency is excluded from our 30s target (master 12, section 5). |
| Duplicate delivery (at-least-once retry double-sends) | Acceptable per at-least-once. Receiver can dedup on the per-incident identity (section 5). Human channels (Slack/Discord/email) may rarely show a duplicate; this is the documented trade-off of at-least-once. |
| Monitor with all channels disabled | Behaves like zero channels for that event: tracks, opens/closes incidents, sends nothing. |

---

## 11. Open decisions (with recommended defaults)

1. **Per-region detail in notifications.** Recommended: **yes, but additive and human-readable only in v1.** Add an optional `Regions: ...` line to Slack/Discord/email bodies naming the regions that saw the failure; do **not** alter the locked generic-webhook envelope (master appendix B). A structured per-region field in the envelope is a later additive change (master 9), not v1. Trade-off: webhook consumers do not get structured region data in v1; acceptable since multi-region itself only GAs at Phase 2 (master 15) and the envelope is locked. (Section 7.)

2. **Re-notify while down.** Recommended: **off by default**, matching master 6.4 and 15. If shipped (phase 3), it is an opt-in per-monitor or per-channel reminder cadence, never on by default, so we do not become noisy (master 14 signal-to-noise). Trade-off: a long outage produces only the initial down message until recovery; the status page and dashboard still show ongoing down.

3. **SMTP `tls: none` allowed?** Recommended: **allow but discourage.** Some internal relays are plaintext on a trusted network. Keep `none` available, default to `starttls`, and warn in UI help. Trade-off: a user can misconfigure to plaintext; the warning plus the default mitigate it.

4. **Test-send on a disabled channel.** Recommended: **block with a clear message** ("enable the channel to test it"), since a disabled channel will not deliver real alerts anyway and a passing test on a disabled channel is misleading. Trade-off: a user wanting to verify config before enabling must enable first; minor. (Section 6.)

5. **Recovery without a matching down on a late-attached channel.** Recommended: **send the recovery anyway** (simpler, and a lone "RECOVERED" is low-harm). Trade-off: a channel attached during an outage sees a recovery it never saw a down for; acceptable and rare. Revisit only if users report confusion. (Section 10.3.)

6. **Generic-webhook custom-header values as secret.** Recommended: **treat values as secret** (they often carry auth tokens), redact on read, show only key names. Trade-off: a user cannot read back a non-sensitive header value they set; acceptable, they can re-set it. (Section 2.2/2.3.)

---

## 12. Dependencies

- **PRD-002 Monitoring Engine** (master 6.4, 6.5): produces the incident open/close transitions and therefore the down/recovery notification events, the one-down/one-up dedup, `started_at`/`ended_at`/`duration_seconds`, `failure_reason`, and the disabled-close (no recovery alert) behavior. This PRD consumes those events; it does not own the state machine.
- **PRD-007 Multi-Region** (master 6.7): owns the per-region results, `down_policy` aggregation, and probe-fleet health that produce the single monitor verdict the notification fires on, and the region list a notification body may name (section 7).
- **PRD-006 Billing & Entitlements** (master 11): owns plan-tier gating of channel **types** (which tiers get PagerDuty/Opsgenie/SMS/Teams/Telegram), enforced on write the same way other entitlements are (section 8). Any per-message COGS limit on SMS lives there.
- **PRD-001 Identity & Tenancy** (master 4, 13): the RBAC matrix this PRD reuses (section 9), org scoping/tenant isolation for channels, and the audit log that records channel created/deleted and failed deliveries.
- **PRD-005 Public API & Webhooks** (master 9): the REST surface for channel CRUD (redacted, secrets write-only) and test, and the separate org-level outbound webhooks (not the per-monitor webhook channel here).
- **RFC-007 Notifier** (PLANNING.md): the technical design of delivery, retry/backoff schedule, idempotency keys, and Kafka consumption that implements section 5. The reused `internal/notify` Go package carries over.
- **RFC-002 Eventing & Kafka Contracts**: the notification event schema, ordering, and idempotency on the wire.
