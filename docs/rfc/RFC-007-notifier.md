# RFC-007 - Notifier

Status: DRAFT for review
Author: Principal Engineering (delivery/notifications)
Audience: notifier service authors, alerting (RFC-006) and api authors who produce its input, anyone wiring outbound delivery
Parent: `docs/rfc/RFC-000-architecture-overview.md` (section 2.5 notifier, section 5 eventing, section 7.3 webhook signing, section 14 reuse)
Depends on: RFC-002 (`notify.events` and `webhook.delivery` consume, the dedup-id contract), RFC-001 (`channels`, `outbound_webhooks`, `notify_dedup` tables), RFC-003 (signing-secret handling, decryption via `internal/crypto`)
Product source of truth: `docs/prd/PRD-003-notifications.md` (channels, locked payloads, delivery semantics, test-send, one-down/one-up), `docs/prd/PRD-005-public-api-and-webhooks.md` section 7 (org outbound webhooks), `docs/PRD.md` appendix B (byte-for-byte payloads), master section 12 (delivery-latency SLO)

House style: no em-dashes. Tables and diagrams over prose. Every load-bearing choice states the decision, the reasoning, and the rejected alternatives.

Scope note (index correction): RFC-000 section 13 conflates down-policy and probe-fleet health into the RFC-007 line. Those belong to RFC-008 (multi-region / probe fleet) and RFC-006 (verdict reduction). RFC-007 owns delivery only: it receives an already-reduced monitor-level verdict as a `notify.event` and delivers it.

---

## 1. Overview, scope, owned contracts

### 1.1 What the notifier is

The notifier is a stateless, HPA-scaled consumer in the control plane. It does two delivery jobs that share one delivery spine:

1. Per-monitor channel notifications: consume `notify.events` (produced by alerting), load the monitor's attached channels, decrypt their secret config, and deliver a down or recovery message to each channel (Slack, Discord, generic webhook, SMTP) using the proven `internal/notify` package.
2. Org-level outbound webhooks: consume `webhook.delivery` (produced by api/alerting), deliver a signed JSON event to a registered org webhook with HMAC signing, per-event id for receiver dedup, and a long retry budget.

The notifier never makes a product decision. The incident is already opened or closed by alerting; the notifier turns one `notify.event` into deliveries and records the outcome.

### 1.2 Scope

| In scope | Out of scope (owner) |
|----------|----------------------|
| Consume loop on `notify.events` and `webhook.delivery`, at-least-once, commit-after-process | The event schemas themselves (RFC-002) |
| Dedup of notify events (Redis fast path + Postgres backstop) | The one-down/one-up state machine and emission (RFC-006) |
| Loading + decrypting channel config | The channels/webhooks/dedup table DDL (RFC-001) |
| Per-channel delivery with retry/backoff via the reused `internal/notify` | The locked payload byte layout (PRD-003 appendix B; do not redesign) |
| Delivery-outcome recording (incident timeline + audit + metrics) | Webhook registration, secret rotation, the public UI/API for webhooks (api / RFC-012) |
| Org outbound-webhook signing, replay protection, 24h retry budget | The verdict reduction and region health (RFC-006/RFC-008) |
| Test-send recommendation (api calls the shared library directly) | KMS-backed crypto key, NetworkPolicy/TLS runtime (RFC-011) |

### 1.3 Contracts this RFC owns

| Contract | Decision in this RFC |
|----------|----------------------|
| Notify dedup id | `hex(sha256(incident_id, event_type))`, carried in the `notify.events` body and the `pulse-dedup-key` header (RFC-002 section 6.5), checked in Redis with a Postgres backstop (`notify_dedup`) (section 4) |
| Reuse of `internal/notify` | the service wraps `Manager.Dispatch` and the registered notifiers + `render.go` unchanged; payloads stay byte-for-byte appendix B (section 2) |
| Outbound-webhook signing | `X-Pulse-Signature: t=<ts>,v1=<hmac>`, HMAC-SHA256 over `<ts>.<raw body>` with the per-webhook secret, 5-minute receiver skew window (section 7) |
| Delivery-outcome record | a delivery row keyed to the incident + channel (or webhook), surfaced on the incident timeline and audited; `outbound_webhooks.last_delivery_*` updated for org webhooks (section 6) |
| Test-send | api calls the shared `internal/notify` library directly and synchronously; it does not route through the notifier service (section 9) |

---

## 2. Reuse of internal/notify (wrap, do not reimplement)

### 2.1 What carries forward unchanged

The proven `internal/notify` package is reused verbatim (RFC-000 section 14). The notifier service is a thin shell around it. The pieces it wraps:

| Reused symbol | What it already does | Notifier wraps it by |
|---------------|----------------------|----------------------|
| `notify.Manager` + `Manager.Dispatch(ctx, ev, channels)` | concurrent per-channel fan-out, one goroutine per channel, retry up to `maxRetries` (3) with `defaultBackoff` (1s, 4s, 9s, capped at 30s), logs the give-up at error level, one channel failing never blocks others | building `ev` and the `[]*domain.Channel` from a `notify.event`, then calling `Dispatch` |
| `slackNotifier` (`{"text": ...}`, 2xx) | Slack incoming-webhook delivery | unchanged |
| `discordNotifier` (`{"content": ...}`, accepts 204) | Discord incoming-webhook delivery | unchanged |
| `webhookNotifier` (`buildEnvelope` -> appendix-B JSON + `custom_headers`, 2xx) | generic per-monitor webhook channel | unchanged |
| `smtpNotifier` (`buildEmail` + `realSMTPSend`, implicit TLS vs STARTTLS) | SMTP delivery per channel config | unchanged |
| `render.go` (`slackText`, `discordText`, `buildEmail`, `humanTime`, `humanDuration`, `reasonLine`) | the locked human-readable formatting | unchanged |

The payloads are locked byte-for-byte by PRD-003 section 4.3 / master appendix B. `buildEnvelope` already renders `mon_<id>` / `inc_<id>` and the RFC3339 fields exactly as appendix B requires. This RFC does not touch any of it. Any payload change is a product decision against appendix B, not a notifier change.

### 2.2 What the service adds around the library

The library is a single-process dispatcher with no Kafka, no dedup, no persistence. The service adds the distributed shell:

| Added layer | Responsibility |
|-------------|----------------|
| Consume loop | join the `notifier` and `notifier-webhook` consumer groups via `internal/bus`, decode events, commit after process (section 3) |
| Dedup | check the notify dedup id before dispatch so a redelivered event does not re-send (section 4) |
| Channel loading + decryption | from the event's `channel_ids`, load `channels` rows (org-scoped), decrypt secret config via `internal/crypto`, build `[]*domain.Channel` with `Config` as a decrypted in-memory map (section 3) |
| Delivery recording | record each (event, channel) outcome to Postgres + audit + metrics so the UI shows it (section 6) |
| Outbound-webhook deliverer | the `webhook.delivery` path: sign, deliver, replay-protect, 24h retry budget, update `outbound_webhooks.last_delivery_*` (section 7) |

One deviation flagged: the library `Manager.Dispatch` does not return per-channel results (it logs the give-up). The service needs per-channel outcomes to record them (section 6). Rather than fork `Dispatch` and risk drift, the service uses a thin outcome collector around the same notifiers (section 6.2). The Manager's retry/backoff and concurrency stay the authority; the collector only observes.

`smtp` and `email` are two distinct channel types now, not aliases. `domain.ChannelSMTP` (`smtp`) is bring-your-own SMTP with free-typed recipients (`smtp.go`); `domain.ChannelEmail` (`email`) is the platform mailer that delivers to org members via the platform's own SMTP (`platformemail.go`). They carry different config and different secret fields, so the api CRUD layer keeps them separate rather than mapping one onto the other.

---

## 3. Consume loop

### 3.1 Shape

```
consumer group "notifier" on topic notify.events (partition key monitor_id)
consumer group "notifier-webhook" on topic webhook.delivery (partition key org_id)

on notify.events message:
  decode envelope + payload
  dedup check on dedup_key (section 4)  -> if duplicate, commit and stop
  load attached channels for channel_ids, org-scoped, decrypt secret config
  build notify.Event{EventType, Monitor, Incident, Check, DurationSeconds, SentAt}
  Manager.Dispatch(ctx, ev, channels)   (reused, concurrent, retry/backoff)
  record per-channel outcome (section 6)
  return nil   -> commit the offset

on webhook.delivery message:
  decode envelope + payload
  dedup check on (outbound_event_id, webhook_id)
  load the outbound_webhooks row, decrypt the signing secret
  sign and deliver with the 24h retry budget (section 7)
  update last_delivery_* and record outcome
  return nil   -> commit the offset
```

### 3.2 At-least-once, commit-after-process

The notifier uses `internal/bus` (RFC-002 section 2.4): offsets commit only after the handler returns nil, so a crash mid-handle redelivers. There is no auto-commit. This is the at-least-once spine; the dedup id (section 4) is what makes redelivery safe.

The handler commits the offset once the event is handled, where "handled" means every channel either delivered or used up its retry budget and was recorded as failed (section 6). A give-up does not return an error and does not loop the partition (RFC-002 section 8.3). Only a structural/unparseable message is routed to `notify.events.dlq` via `bus.Poison` (RFC-002 section 8.2). A transient infra error (Postgres deadlock, Redis blip, dispatch setup failure before any send) returns a normal error so the message redelivers and the dedup id keeps it safe.

### 3.3 Channel loading and decryption

The event carries `channel_ids` (RFC-002 section 4.5), not the channel config, because config is secret and lives encrypted in Postgres (RFC-001 section 4.3). The notifier:

1. Loads the `channels` rows for `channel_ids`, scoped to the event's `org_id` (RLS-backed, `app.current_org` set per the repository pattern in RFC-001 section 5.3).
2. Decrypts the secret subfields with `internal/crypto` (AES-256-GCM, reused unchanged, RFC-000 section 10). The secret subfields per type are fixed by RFC-001 section 4.3: slack `webhook_url`; discord `webhook_url`; webhook `url` and each `custom_headers[].value`; smtp `password`. Non-secret fields stay plaintext.
3. Builds `[]*domain.Channel` whose `Config` is the decrypted in-memory map the library expects (`domain.Channel.Config map[string]any`, `cfgString`/`cfgBool` read it).

Decrypted secrets live only in memory for the dispatch and are never logged (the library already never logs config). A channel that is disabled or whose id is dangling (deleted) is skipped: `Manager.Dispatch` already skips `nil` or `!ch.Enabled` channels, and a deleted id simply does not load, so it is a no-op (PRD-003 section 10.3, AC handled at delivery time).

A zero-length `channel_ids` still arrives (the incident still opened/closed, PRD-002 section 4.8); the notifier dedups, loads nothing, dispatches to nothing, and commits. This is the supported zero-channel monitor (PRD-003 AC4).

---

## 4. Idempotent delivery / dedup

### 4.1 The dedup id

The dedup id is `hex(sha256(incident_id, event_type))`, set by alerting on the `notify.event` body (`dedup_key`) and in the `pulse-dedup-key` header (RFC-002 section 4.5, 6.5). Because there is exactly one down and one recovery per incident (PRD-002 section 4.7), this id is stable per (incident, kind): a redelivered `notify.event` carries the same id, a legitimate recovery for the same incident carries a different id, so the down and the up each fire once and a redelivery of either is suppressed.

This reconciles with the per-channel retry already inside `internal/notify`: the dedup id guards the whole event (one down per incident), and the Manager's in-process retry guards one channel attempt. They are different layers. Dedup stops a second `notify.event` from re-running the whole fan-out; the Manager's retry handles a single channel's transient failure within one fan-out. At-least-once to the third party is still accepted (PRD-003 section 5: a receiver may rarely see a duplicate); the dedup id only removes the obvious duplicate of re-processing the same Kafka event.

### 4.2 Where the dedup state lives

Per RFC-000 section 5.3, dedup state is Redis (fast path) with a Postgres backstop (`notify_dedup`, RFC-001 section 4.6).

```
dedup(dedup_key, org_id):
  ok = Redis SET pulse:notify:dedup:<dedup_key> "1" NX EX <window>
  if not ok:                       -- key already present in Redis
      return DUPLICATE
  -- first time in Redis; confirm against the durable backstop in case Redis evicted
  inserted = Postgres INSERT INTO notify_dedup (org_id, dedup_id) VALUES (...)
             ON CONFLICT (org_id, dedup_id) DO NOTHING
  if inserted == 0:                -- already in Postgres (Redis had been evicted)
      return DUPLICATE
  return FIRST
```

| Aspect | Decision | Reasoning |
|--------|----------|-----------|
| Fast path | Redis `SET NX EX` | sub-ms, absorbs the common redelivery within the window without touching Postgres |
| Backstop | `notify_dedup` unique `(org_id, dedup_id)` | survives a Redis eviction or flush so a duplicate is still caught after the Redis key is gone |
| TTL / window | Redis TTL comfortably exceeds the `notify.events` retention (24h, RFC-002 section 3.4) and the redelivery horizon; the Postgres row is aged out by a background prune on `created_at` | the window only needs to outlive the at-least-once redelivery window; 24h+ is generous |

Order matters: Redis is set first (cheap gate), then Postgres confirms durability. The `notify_dedup` insert is the authoritative "this event was handled"; the Redis key is the cheap shortcut.

### 4.3 The race between two consumers

A partition rebalance can briefly hand the same `notify.event` to two consumers (RFC-002 section 8.5). The race is resolved at the dedup gate, not by locking:

- `Redis SET NX` is atomic: exactly one of the two racers gets `ok=true` and proceeds; the other sees the key and returns DUPLICATE.
- If both somehow pass Redis (eviction between the two), the `notify_dedup` unique constraint makes the second `INSERT ... ON CONFLICT DO NOTHING` return zero rows, so the second is recognized as DUPLICATE.

So at most one consumer dispatches per dedup id. The window where the dedup key is set but dispatch has not finished is the only soft spot: if the dispatching consumer crashes after setting the key but before delivering, the redelivery would be suppressed. That is acceptable under "at-least-once, avoid obvious duplicates" only if we do not lose the send entirely, so the key is set with `NX EX` but the handler treats a crash before recording any outcome as needing replay. To keep the send-once guarantee strong without double-sending, the dedup row is written inside the same path that records the outcome (section 6): the durable `notify_dedup` row is the "handled" marker, written together with delivery recording, while the Redis key is only the fast pre-check. If a consumer crashes mid-dispatch, the Redis key may exist but the Postgres "handled" marker does not, so on redelivery the Redis pre-check is treated as advisory and the Postgres state decides. See section 11 for the Redis-unavailable decision.

---

## 5. Per-channel delivery

### 5.1 Transport per channel type (all reused)

| Type | Transport | Success | Notes |
|------|-----------|---------|-------|
| slack | HTTPS POST `{"text": ...}` | 2xx | `postJSON` checks `200 <= code < 300` |
| discord | HTTPS POST `{"content": ...}` | 2xx incl. 204 | Discord returns 204; `postJSON` accepts it |
| webhook | HTTPS POST appendix-B envelope + `custom_headers` | 2xx | the per-monitor generic-webhook channel, distinct from org webhooks (section 7) |
| smtp | SMTP per channel config | server accepts the message | implicit TLS (`tls`) wraps from connect; otherwise STARTTLS is used when the server offers it; auth via `PlainAuth` when a username is set |

These four are the locked v1 payloads (`slack.go`, `discord.go`, `webhook.go`, `smtp.go`). More provider types now ship in the registry (platform email, pagerduty, opsgenie, telegram, teams, twilio); RFC-007a lists them with the API each uses. The notifier adds nothing to the wire format.

### 5.2 Retry, backoff, give-up

Retry/backoff is carried from `internal/notify` and not re-implemented:

| Property | Value (from `Manager`) |
|----------|------------------------|
| Max attempts per channel | 3 (`maxRetries`) |
| Backoff | `defaultBackoff`: 1s, 4s, 9s, capped at 30s, between attempts |
| Per-channel isolation | one channel runs in its own goroutine; one failing never blocks others (`Dispatch` + `sendWithRetry`) |
| Give-up | after the last attempt the Manager logs at error; the service records the failure visibly (section 6) |

PRD-003 section 10.3 distinguishes a likely-permanent 4xx (dead webhook, bad token, SMTP auth/recipient) from a transient 5xx. The reused notifier treats every non-2xx the same (retry up to 3). That is acceptable for v1: 3 attempts over ~14s on a hard 4xx is cheap, and the outcome is recorded the same way either way. The give-up reason captured in the outcome record carries the last error text so the user sees "HTTP 404" vs "HTTP 503" and can tell a dead webhook from a flaky one. A future optimization (give up sooner on a 4xx) is a `Manager` change, flagged in open questions, not done here so the proven code stays untouched.

The whole per-channel retry budget for one event finishes well inside the 30s SLO budget (worst case ~1s + 4s + send time across 3 attempts), and the SLO excludes the third party's own latency (PRD-003 section 5, master section 12).

---

## 6. Delivery-outcome recording

### 6.1 Decision: a deliveries record, surfaced on the incident timeline

Decision: record each (notify event, channel) outcome as a durable row that the incident timeline reads, plus an audit entry on failure, plus metrics. This is the trust requirement in PRD-003 section 5 and AC8: the incident detail must show that a notification to channel X failed so the team is not left thinking they were alerted.

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| A deliveries row per (incident, channel, event_type), read by the incident timeline (chosen) | chosen | Gives a structured, queryable record the UI renders directly on the incident, keyed so a redelivery upserts rather than duplicates. Matches "visible in the incident timeline" (PRD-003 section 5) and the `outbound_webhooks.last_delivery_*` pattern already in RFC-001 for org webhooks |
| An incident annotation (free-text) only | rejected | Loses structure (channel id, status, reason, attempt count) the UI needs to render a per-channel status list, and it is awkward to upsert on redelivery |
| Audit log only, no per-incident record | rejected | The audit log is org-wide and not the incident view; the team looks at the incident first. Audit stays as the secondary trail, not the primary surface |

RFC-001 owns the exact DDL. RFC-007 fixes the shape it needs: a `notify_deliveries` row keyed by `(org_id, incident_id, channel_id, event_type)` with `status` (delivered / failed), `attempts`, `last_error`, `delivered_at`. The key makes a redelivery an upsert no-op on success and lets a later real recovery delivery for the same incident write its own row (different `event_type`). This is flagged as an addition for RFC-001 to add to its schema; it is the natural sibling of `notify_dedup` and `outbound_webhooks.last_delivery_*`.

A failed delivery additionally emits an `audit.events` record (`action: notification.delivery_failed`, target the channel, actor `system`) so it appears in the org audit trail (PRD-003 section 9, RFC-002 section 4.8), and increments a metric (section 10).

### 6.2 How the outcome is observed without forking the Manager

`Manager.Dispatch` does not return per-channel results. To record outcomes without re-implementing the retry/backoff/concurrency, the service runs the same notifiers through a small outcome collector that wraps each channel send and captures (channel id, final status, attempts, last error), while the Manager owns the attempt loop. The collector observes; it does not change delivery behavior. This keeps the proven `Dispatch` semantics authoritative and avoids drift. (If a later refactor gives `Dispatch` a results return, the collector goes away; until then it is a thin observer, not a fork.)

### 6.3 Surfacing to the user

| Surface | What it shows |
|---------|---------------|
| Incident timeline (api read) | per-channel delivery status for the down and recovery events: delivered, or failed with a short reason and attempt count (PRD-003 AC8) |
| Audit log | a `notification.delivery_failed` entry per failed channel (owner/admin visible) |
| Metrics | delivery success/failure per channel type, delivery latency per channel type, dedup suppressions (RFC-000 section 9.1) |

---

## 7. Org-level outbound webhooks

Distinct from the per-monitor generic-webhook channel (PRD-003): an org webhook is a programmatic event feed for the whole org (PRD-005 section 7). api/alerting fan out the org's subscribed events onto `webhook.delivery` (RFC-002 section 4.9); the notifier delivers them. Registration, secret rotation, and the UI/API are owned by api/RFC-012; the notifier only delivers.

### 7.1 Delivery flow

```
on webhook.delivery (webhook_id, outbound_event_id, event, created_at, data, org_id):
  dedup on (outbound_event_id, webhook_id)              -- Redis NX + notify_dedup-style backstop
  load outbound_webhooks row (org-scoped); if disabled or deleted -> commit and stop
  decrypt signing_secret via internal/crypto
  body = JSON { event_id: outbound_event_id, event, org_id, created_at, data }   -- PRD-005 7.3 envelope
  ts  = unix seconds now
  sig = hex(hmac_sha256(signing_secret, ts + "." + raw_body))
  POST body to endpoint_url with:
       Content-Type: application/json
       X-Pulse-Signature: t=<ts>,v1=<sig>
  2xx -> success; else retry per the 24h budget (7.3)
  update outbound_webhooks.last_delivery_at/status/error
  record outcome; return nil -> commit
```

### 7.2 Signing and replay protection

Per RFC-000 section 7.3 and PRD-005 section 7.2:

| Element | Decision |
|---------|----------|
| Header | `X-Pulse-Signature: t=<ts>,v1=<hmac>` |
| HMAC input | `<ts> + "." + raw_request_body` (the timestamp is bound into the signature so a replay with a stale `ts` does not verify) |
| Algorithm | HMAC-SHA256 with the per-webhook `signing_secret` (decrypted from `outbound_webhooks.signing_secret`, AES-256-GCM at rest, RFC-001 section 4.6) |
| Per-event id | `event_id = outbound_event_id` in the body so the receiver dedups an at-least-once redelivery (PRD-005 section 7.2) |
| Replay window | the signed `ts` lets the receiver reject deliveries outside a ~5-minute skew (PRD-005 section 7.2); combined with `event_id` dedup it stops replay |
| Secret lifecycle | shown once at creation, stored encrypted, rotated by api; the notifier only reads and decrypts it |

The signing is a notifier/delivery concern; the Kafka `webhook.delivery` event carries `webhook_id` so the notifier loads the secret, not the secret itself (RFC-002 section 4.9). The secret never rides the bus and is never logged.

### 7.3 Retry budget

Decision: exponential backoff for up to ~24h, then stop and surface the webhook as failing (PRD-005 section 7.2 retry budget, open decision 11.3).

| Aspect | Decision | Reasoning |
|--------|----------|-----------|
| Budget | ~24h of spaced retries, then give up | a customer pipeline can be down for hours; 24h gives a real chance to recover without dropping events while in budget (PRD-005 section 10.3 edge case "webhook receiver down") |
| Where the budget lives | NOT in the in-process `Manager` (that caps at 3 attempts over seconds, right for a chat channel, wrong for a 24h budget) | the org-webhook path is its own deliverer with its own backoff schedule; it does not reuse `Manager.sendWithRetry` |
| How a 24h budget survives restarts | the notifier does not hold a 24h timer in memory. A failed attempt that is still in budget returns a normal error so the Kafka message is NOT committed and redelivers, OR is re-scheduled via a delayed retry; the durable budget anchor is the event's `created_at` so any consumer can compute "still in budget?" on each attempt | a stateless service must not depend on an in-memory long timer; the budget is recomputed from `created_at` |
| Give-up | stop, set `last_delivery_status = failed` with `last_delivery_error`, record visibly so an owner/admin sees a broken receiver (PRD-005 section 7.2 failure visibility), commit the offset | a give-up must not loop the partition |

Implementation note for the 24h budget across redeliveries: `webhook.delivery` retention is 24h (RFC-002 section 3.4), which matches the budget, so an uncommitted message that redelivers covers most of the budget naturally. For spacing beyond a single redelivery cycle, the deliverer uses a delayed-retry topic (a small `webhook.delivery.retry` with a visibility delay) rather than blocking a partition for hours. RFC-011 provisions it; the budget rule (recompute from `created_at`, stop at 24h) is fixed here.

### 7.4 Free tier

Outbound webhooks are paid-tier only (PRD-005 section 7.4). api will not register a webhook for a Free org, so no `webhook.delivery` event is produced for one. The notifier does not re-check entitlement; it trusts api authorized the registration (RFC-000 section 7.1, identity/authorization as data).

---

## 8. Multi-region detail in messages

Notifications fire on the monitor-level verdict, not per region (PRD-003 section 7, master section 6.7). Alerting reduces per-region results into one verdict before emitting one `notify.event`; the notifier never sends one message per region.

| Where region detail appears | Rule |
|-----------------------------|------|
| `notify.events` body | carries `regions_observed_unhealthy` (RFC-002 section 4.5), the regions that counted toward the down verdict |
| Slack / Discord / email body | MAY append an additive human-readable line (for example `Regions: eu-west, us-east`) after the existing locked lines (PRD-003 section 7, open decision 11.1) |
| Locked generic-webhook envelope (appendix B) | NOT changed. No region field is added in v1. Structured per-region detail in the envelope is a later additive change, not shipped silently (PRD-003 section 7 / 11.1) |
| Single-region monitors (Phase 0/1) | no region line; nothing meaningful to name |

So region detail is purely an additive human-readable line in the chat/email bodies. The byte-for-byte appendix-B envelope and the Slack/Discord/email locked lines are unchanged; the region line is appended, never inserted into an existing field. This stays within "do not redesign the payloads": the `render.go` functions get an optional trailing line for multi-region monitors, which does not alter any existing field or the webhook envelope.

---

## 9. Test-send

Decision: the api "send test message" path calls the shared `internal/notify` library directly and synchronously. It does NOT route through the notifier service.

```
api handler POST /api/v1/channels/{id}/test (member+):
  authz role gate + entitlement gate (RFC-003)
  load channel, decrypt config
  result = notify.Manager.Test(ctx, channel)     -- the library's Test() entry, single attempt
  return 200 {delivered:true} or 400/502 {delivered:false, reason: <short error>}
```

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| api calls `internal/notify` directly, synchronous (chosen) | chosen | The user needs a synchronous pass/fail with a reason at setup time (PRD-003 section 6: "the UI shows whether the test delivered or failed"). The library exposes `Manager.Test(ctx, ch)` for exactly this. api already holds the channel, decrypts it, and runs in the same module, so a direct call is the simplest correct path and gives the immediate result the UI wants. PRD-003 section 6 frames test-send as a synchronous one-channel action |
| api produces a Kafka event the notifier delivers | rejected | A test must return a synchronous result; routing through Kafka makes it async and forces a result-callback or polling path for no benefit. Test-send is one channel, one attempt, no incident, no dedup, no one-down/one-up (PRD-003 section 6), so the consume-loop machinery adds only latency and complexity |

Test-send is a single attempt (`Notifier.TestMessage` / `Manager.Test`), targets one channel, touches no monitor, opens no incident, and does not consume the one-down/one-up contract (PRD-003 section 6). A disabled channel is blocked by api with a clear message (PRD-003 open decision 11.4). This reuses the library; it is the one place the shared package is called outside the notifier service, and it is consistent because the library is in the same module (RFC-000 section 3).

---

## 10. Scaling

### 10.1 Consumer-group parallelism and HPA

| Aspect | Decision |
|--------|----------|
| Scale signal | `notify.events` + `webhook.delivery` consumer-group lag (RFC-000 section 11.1, RFC-002 section 8.1) |
| HPA | scale the notifier on lag up to the partition counts: `notify.events` 32, `webhook.delivery` 16 (RFC-002 section 3.3) |
| Parallelism | per-monitor ordering is preserved because `notify.events` is keyed by `monitor_id`; cross-monitor fan-out is unbounded up to partition count |
| Burst | a wide outage opens many incidents at once; the 32 partitions on `notify.events` are sized for exactly that burst (RFC-002 section 9.2) |

### 10.2 Delivery-latency SLO

The SLO is: down/recovery sent within 30s of the triggering check at p99, excluding the third-party channel's own latency (master section 12, PRD-003 AC9).

| How met | How measured |
|---------|--------------|
| Lag-based HPA keeps `notify.events` drained so the consume-to-dispatch step stays short; the per-channel retry budget (worst case ~14s over 3 attempts) fits inside 30s; the dedup check is a single sub-ms Redis op | end-to-end OTel span from `check.checked_at` to the notifier's outbound call start, p99 (RFC-000 section 9.4); the span excludes the third party's own response time, matching the SLO's exclusion |
| The trace id is propagated over Kafka headers scheduler -> worker -> alerting -> notifier (RFC-002 section 2.4) so one check is followed end to end | per-channel delivery-latency histogram and `notify.events` lag (RFC-000 section 9.1) |

### 10.3 Rate-limiting outbound to a single provider

A wide outage can open many incidents whose channels point at the same Slack workspace or the same org-webhook host, which the provider may throttle (429). Decision: a per-destination outbound rate limit (a Redis token bucket keyed by destination host, or by Slack/Discord webhook URL hash) so the notifier does not overload one provider and trigger throttling. When the bucket is empty, the send waits briefly within the retry/backoff window rather than hammering; a provider 429 is treated like a transient failure and retried with backoff (the reused `Manager` backoff for channels, the 24h budget for org webhooks). This bounds our own request rate to one provider and keeps the give-up-and-record behavior for a provider that stays throttled. RFC-011 sizes the bucket; the posture (limit per destination, back off on 429) is fixed here.

---

## 11. Failure modes

| Failure | Behavior |
|---------|----------|
| Provider 5xx / timeout (Slack/Discord/webhook) | transient: the `Manager` retries up to 3 with backoff; if still failing, give up and record the failure visibly (incident timeline + audit + metric). Other channels unaffected (PRD-003 section 10.3) |
| Provider hard 4xx (404 dead webhook, 401 bad token) | retried the same 3 times in v1 (the reused notifier does not distinguish), then give up and record with the last error text so the user can tell it is a dead/misconfigured endpoint. Sooner give-up on 4xx is a future `Manager` change (open questions), not done here |
| SMTP auth / TLS failure | the reused `smtpNotifier` returns a descriptive error (`smtp: starttls: ...`, `smtp: auth: ...`); recorded visibly with the TLS/auth-specific reason. The configured `tls` mode is honored; no silent downgrade to plaintext (PRD-003 section 10.3) |
| Channel deleted or disabled mid-incident | handled at delivery time: `Dispatch` skips disabled channels, a deleted id does not load, so it is a no-op. The down may have gone out before disable; recovery skips it (PRD-003 section 10.3). No error |
| Org-webhook receiver down | retried with backoff within the ~24h budget (section 7.3); while in budget no event is dropped; after 24h give up, set `last_delivery_status=failed`, surface as failing (PRD-005 section 10.3) |
| Redis dedup unavailable | decision: fail toward send-once-more, not skip. If Redis is down, the dedup fast path is unavailable; fall back to the Postgres `notify_dedup` backstop as the authority (a unique-constraint insert still dedups). If Postgres is also unavailable, return a transient error so the event redelivers later rather than risk a missed alert. The principle: a missed alert is worse than a rare duplicate (PRD-003 section 5 accepts at-least-once), so when dedup state cannot be confirmed we send, never silently drop. This mirrors RFC-003's "key verify fails closed, entitlement can fail open" split: an alert must not be lost to a cache outage |
| notifier fully down | `notify.events` and `webhook.delivery` hold 24h (RFC-002 section 3.4), so the notifier can be down ~a day and catch up on recovery; lag-based HPA scales the catch-up. Delivery is late but not lost while in retention |
| Poison `notify.event` (unparseable) | routed to `notify.events.dlq` via `bus.Poison`, offset committed so the partition advances; a DLQ write raises an RFC-010 alert (RFC-002 section 8.2) |

---

## 12. Open questions and dependencies

### 12.1 Open questions

| # | Question | Owner / lean |
|---|----------|--------------|
| Q1 | Should `Manager` give up sooner on a hard 4xx than on a 5xx (PRD-003 section 10.3 distinguishes them)? v1 retries both 3 times. | RFC-007 / lean: keep the reused 3-attempt behavior in v1, add 4xx fast-give-up only if delivery-failure metrics show wasted retries. Any change is to `internal/notify`, not the service |
| Q2 | The `notify_deliveries` table (section 6.1) is an addition this RFC needs for incident-timeline recording. RFC-001 must add the DDL. | RFC-001 (sibling of `notify_dedup` / `outbound_webhooks.last_delivery_*`) |
| Q3 | The org-webhook delayed-retry mechanism for the 24h budget (a `webhook.delivery.retry` topic with visibility delay vs an external scheduler) | RFC-011 provisions; budget rule (recompute from `created_at`) fixed here |
| Q4 | Outbound per-destination rate-limit bucket size (section 10.3) | RFC-011 sizes; posture fixed here |
| Q5 | Region line wording in chat/email bodies (PRD-003 open decision 11.1) and whether a structured envelope field ever lands | product / PRD-003 |
| Q6 | RESOLVED: `email` and `smtp` are now two distinct channel types, not one renamed. `smtp` is bring-your-own SMTP; `email` is the platform mailer to org members (`platformemail.go`) | resolved in code |

### 12.2 Dependencies

| This RFC depends on | For |
|---------------------|-----|
| RFC-000 | the notifier service contract (section 2.5), the dedup decision (5.3), webhook signing (7.3), reuse map (14) |
| RFC-002 | `notify.events` + `webhook.delivery` schemas, the dedup-id and `(outbound_event_id, webhook_id)` tokens, commit-after-process, DLQ |
| RFC-001 | `channels` (encrypted config), `outbound_webhooks` (encrypted signing secret, `last_delivery_*`), `notify_dedup`, and the new `notify_deliveries` table (Q2) |
| RFC-003 | the secret-handling discipline and decryption via `internal/crypto`; webhook signing secrets are key-like secrets stored encrypted |
| RFC-006 | produces `notify.events` with the dedup id and the reduced verdict; the notifier consumes its output |
| `internal/notify`, `internal/crypto`, `internal/bus`, `internal/redis` | reused/wrapped: delivery + retry, decryption, consume loop, dedup helpers |
| RFC-011 | the delayed-retry topic, the outbound rate-limit bucket, KMS-backed crypto key, NetworkPolicy + TLS |

| Depends on this RFC | For |
|---------------------|-----|
| none downstream | RFC-007 is a leaf (RFC-000 section 13) |
```
