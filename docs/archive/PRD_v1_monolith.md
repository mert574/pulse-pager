# Pulse - Product Requirements Document (v1.0)

Status: Final - approved for architecture
Owner: Product
Audience: Tech lead and engineers building v1

## 1. Summary

Pulse is a self-hostable uptime and health-check tool for small teams. You register an HTTP endpoint and a check interval. Pulse checks it on schedule, decides if it's healthy or not, and notifies you through a channel you picked (Slack, email, Discord, or a generic webhook) when something breaks and again when it recovers.

The whole point is the core loop: register endpoint -> scheduled batch check -> detect issue -> notify. Everything in this PRD that isn't in service of that loop is either trimmed or pushed to a later phase.

Tech is already decided and fixed: Go backend, Lit web components frontend, single self-hosted binary with embedded SQLite storage.

## 2. Problem and users

### The problem

Small teams run services (APIs, websites, internal tools) and want to know when one goes down before a customer tells them. The big hosted options (Pingdom, Datadog, Better Uptime, etc.) are either expensive, overkill, or send your monitoring data to a third party. The cheap DIY options (a cron job running curl) have no history, no UI, and bad alerting.

So the pain is: "I want simple, private, owned uptime monitoring with decent alerting, without paying per-monitor or running a heavy stack."

### Who uses it

- **Primary: a developer or small ops person at a team of 1-15 people.** They self-host Pulse on a small VM or a container next to their other infra. They are technical enough to write a JSON body and paste a Slack webhook URL, but they don't want to manage Prometheus + Alertmanager + Grafana just to know if their API is up.
- **Secondary: the rest of that small team** who just want to glance at a dashboard and see green/red, and get pinged in Slack when something's wrong.

This is not for: large orgs with SLAs and on-call rotations, multi-region synthetic monitoring, or hundreds of users with per-team RBAC. Those are explicit non-goals for v1.

### Key use cases / user stories

1. As a developer, I add my production API health endpoint and get a Slack message within a minute or two of it going down.
2. As a developer, I want to check that the endpoint returns 200 AND responds in under 2 seconds, and get alerted if either fails.
3. As an ops person, I want to see the last 24 hours of a monitor's checks (up/down, latency) so I can tell if a blip was real.
4. As a developer, I don't want to be spammed: one alert when it goes down, one when it comes back, not one every 60 seconds while it's down.
5. As a team, we want a single dashboard showing all our monitors and their current status.
6. As the person running Pulse, I want the UI behind a login so randoms on the internet can't see or edit our monitors.

## 3. Scope

### In scope for v1

- HTTP/HTTPS endpoint monitors with configurable method, headers, body, expected status codes, timeout, interval, name, enabled flag.
- A scheduler that batch-checks all enabled monitors on their own intervals.
- Health decision based on: status code match, timeout, connection failure, optional max-latency assertion, optional response-body-contains assertion.
- Check result history stored in SQLite, with a retention window.
- Notification channels: Slack (incoming webhook), Discord (incoming webhook), generic webhook (POST JSON), and email (SMTP).
- Alert rule: fire after N consecutive failures (default 1), send a recovery notification when it goes back to healthy. De-dupe so you get one "down" and one "up" per incident.
- Incidents: a record of a down-period (start, end, which checks failed).
- Web UI: list monitors with current status and latency, CRUD monitors, configure notification channels, view a monitor's recent history, view incidents.
- Single-user (or shared-credential) auth for the UI and API.

### Explicitly out of scope for v1 (deferred)

- Non-HTTP checks (TCP port, ICMP ping, DNS, TLS cert expiry, gRPC, keyword on a real browser page).
- Multi-region / distributed checking from more than one location.
- Multi-user accounts, roles, teams, per-monitor permissions.
- Status pages (public shareable uptime page).
- Maintenance windows / scheduled mute.
- Escalation policies, on-call schedules, paging (PagerDuty/Opsgenie).
- SMS / phone / push notifications.
- Alert when latency is degraded over a rolling window (percentile-based SLO alerting).
- Tags/groups/folders for organizing monitors.
- Metrics export (Prometheus endpoint), API tokens for third parties.
- Mobile app.

These are listed so we don't accidentally build them. Several are obvious phase-2 candidates (status pages, TCP/TLS checks, maintenance windows) and the data model leaves room for them.

## 4. Functional requirements

### 4.1 Monitors

A monitor is the thing you want checked. Fields:

- `name` (required, string) - human label, e.g. "Prod API health".
- `url` (required, string) - the full http(s) URL to hit.
- `method` (required, enum) - GET, POST, PUT, PATCH, DELETE, HEAD. Default GET.
- `headers` (optional, list of key/value) - sent with the request. Used for auth headers, content-type, etc.
- `body` (optional, string) - request body sent for methods that take one. Stored as-is, sent verbatim.
- `expected_status_codes` (required) - one or more codes or simple ranges that count as healthy. Default `200`. Allow a small set like `200,204` or `2xx`. See decision in section 9.
- `timeout_seconds` (required) - how long to wait before calling it a timeout. Default 10. Max 60.
- `interval_seconds` (required) - how often to check. Default 60. Minimum 30 (see NFR). Allowed presets plus free entry.
- `enabled` (required, bool) - default true. Disabled monitors are not scheduled and fire no alerts.
- `max_latency_ms` (optional) - if set, a response slower than this counts as unhealthy even if the status code matched.
- `body_contains` (optional, string) - if set, the response body must contain this substring to count as healthy.
- `failure_threshold` (required) - number of consecutive failed checks before an alert fires. Default 1.
- `notification_channel_ids` (list) - which channels to notify for this monitor. Can be zero (then it's silently tracked, no alerts).

Per-field validation rules are in section 12.4. Edits to a monitor's assertions do not retroactively change past check results or close an open incident; see section 11 and 12.5.

### 4.2 Check execution

For each scheduled check, Pulse makes the configured HTTP request with the configured timeout and records the outcome. A check result is **healthy** only if all configured assertions pass:

1. The request completed without a connection error and without timing out, AND
2. The response status code matches `expected_status_codes`, AND
3. If `max_latency_ms` is set, the measured response time is at or under it, AND
4. If `body_contains` is set, the response body contains that substring.

A check is **unhealthy** if any of these fail. The recorded failure reason is one of: `connection_error`, `timeout`, `status_mismatch`, `latency_exceeded`, `body_assertion_failed`, `blocked_target` (only when `PULSE_BLOCK_PRIVATE_NETWORKS` is on and the target resolves to a blocked range). We record one primary reason in priority order: `blocked_target` first (we never sent the request), then connection/timeout, then status, then latency, then body. We also store the actual status code and latency for context.

Each check result stores: monitor id, timestamp, healthy bool, failure reason (nullable), http status code (nullable), latency in ms (nullable), short error text (nullable, truncated).

Response bodies are read up to a cap (e.g. 64 KB) only when `body_contains` is set, otherwise we don't need to keep the body at all. We never store full response bodies. When `body_contains` is set, we read at most the 64 KB cap and run the substring match against what we read; if the match string would only appear past the cap, the check fails the body assertion. Document this cap in the UI help text.

A manual "check now" produces a normal check result and feeds the alerting state machine exactly like a scheduled check. To avoid two checks racing on one monitor, only one check per monitor runs at a time: take a per-monitor lock at the start of a check. If a "check now" arrives while a check for that monitor is already running, return the in-flight/just-finished result instead of starting a second one (HTTP 409 with a short message, or return the latest result). A "check now" does not shift the scheduled cadence; the next scheduled check still fires at its normal time.

### 4.3 Check history / results storage

- All check results are written to SQLite.
- Retention default: keep raw check results for 30 days, then delete. Configurable. A background cleanup job runs periodically (e.g. hourly).
- The UI reads from this for the per-monitor history view and latency chart.
- We do not need rolled-up aggregates for v1; querying raw results for "last 24h / 7d" of a single monitor is fine at our target scale. If it gets slow later we add hourly rollups (phase 2).

### 4.4 Notification channels

A channel is a reusable destination you configure once and attach to many monitors. v1 channels:

1. **Slack** - config: incoming webhook URL. We POST a formatted message.
2. **Discord** - config: incoming webhook URL. We POST a formatted message.
3. **Generic webhook** - config: URL, plus optional custom headers. We POST a fixed JSON payload describing the event (monitor name, url, status, reason, timestamps). This is the escape hatch so people can wire Pulse into anything.
4. **Email (SMTP)** - config: SMTP host, port, username, password, from address, to address(es), TLS on/off. We send a plain text or simple HTML email.

Each channel has: `id`, `name`, `type`, type-specific config (stored as JSON), `enabled`.

Each channel must support a **"send test message"** action from the UI so the user can confirm it works before relying on it.

Channel config holds secrets (webhook URLs, SMTP password). See security NFR.

### 4.5 Alerting rules

Default behavior, chosen to be useful out of the box and not spammy:

- A monitor enters a **down** state after `failure_threshold` consecutive unhealthy checks (default 1). At that moment Pulse opens an **incident** and sends ONE "down" notification to each attached channel.
- While the monitor stays down, Pulse does NOT keep sending notifications. (No re-notify / reminder in v1. Noted as a possible phase-2 option with a sane default of off.)
- When the next check is healthy again, Pulse closes the incident and sends ONE "recovery" notification to each attached channel. Recovery notifications are on by default.
- If a monitor is disabled while down, the incident is closed without a recovery alert (state change was manual). `ended_at` is set to the time it was disabled and we record on the incident that it was closed by disable, not by recovery.
- If a monitor is edited (its config/assertions change) while down, the open incident stays open. We do not re-evaluate past results against the new config and we do not auto-close. The next check runs with the new config and its result drives state as normal: a healthy result closes the incident and sends recovery, an unhealthy one keeps it open. See section 11 and the table in 12.5.
- Re-notify while down is off in v1 and there is no config for it. One down alert, one up alert, per incident.

So the contract is: one down alert, one up alert, per incident. `failure_threshold` lets you absorb flaky single-check blips by setting it to 2 or 3.

The failure counter resets to zero on any healthy check. The threshold counts consecutive unhealthy checks; a single healthy check in the middle resets the count so you need a fresh run of `failure_threshold` failures to open an incident.

Notification content (down): monitor name, URL, what failed (reason + status/latency), time it went down. Recovery: monitor name, URL, time it recovered, how long it was down. Duration is `ended_at - started_at` where `started_at` is the timestamp of the first failing check in the run that opened the incident (not the check that crossed the threshold).

### 4.6 Dashboard / UI

Built in Lit. Screens:

1. **Monitors list (home)** - every monitor with: name, current status (up / down / disabled / pending), last check time, last latency, maybe a small recent-history sparkline. Quick toggle for enabled. Clear visual red for down. How current status, last check time, and last latency are derived is specified in section 12.2; "last latency" is the latency of the most recent check result for that monitor.
2. **Monitor detail** - full config, current status, recent check history (table and a latency-over-time chart), the incidents for this monitor.
3. **Monitor create/edit form** - all the fields from 4.1, with channel selection and a validation pass (valid URL, sane interval/timeout).
4. **Channels** - list channels, CRUD, and the "send test message" button per channel.
5. **Incidents** - a global list of incidents across all monitors (open and recently closed), each showing monitor, start, end, duration, cause.
6. **Login** - simple auth screen for the UI.

Keep it clean and fast. No theming, no dashboards-builder, no widgets. One product, opinionated layout.

## 5. Non-functional requirements

- **Scale target for v1:** comfortably handle up to ~200 monitors at intervals down to 30s on a small VM (1-2 vCPU, 512MB-1GB RAM). That's the realistic ceiling for "small team self-host". Design shouldn't fall over before that. We are not designing for thousands of monitors in v1.
- **Scheduling accuracy:** a check should run within a few seconds of its scheduled time under normal load. Checks must not pile up: if a monitor's check is slow, it shouldn't block other monitors' checks. Outbound checks run concurrently with a bound on max in-flight requests so we don't exhaust the host.
- **Check correctness:** a slow or hanging endpoint must be cut off at `timeout_seconds` and counted as a timeout, never left hanging. Each check's latency measurement is the wall-clock time from request start to response received.
- **Reliability of alerts:** if a notification send fails (e.g. Slack webhook returns 500), retry a couple of times with backoff, then give up and record the failure visibly in the UI/logs. A missed alert is worse than a late one, but we won't build a full delivery queue in v1.
- **Persistence:** all config and history survive a restart. On startup the scheduler rebuilds its schedule from the DB and resumes. In-flight incident state is derived from stored data, not held only in memory.

### Security (call out the real risks)

This app is a juicy target because it (a) makes arbitrary outbound HTTP requests and (b) stores secrets.

- **Outbound request / SSRF risk:** users can point a monitor at any URL, including internal addresses (`http://169.254.169.254/...`, `http://localhost`, private ranges). Since it's self-hosted and single-tenant, the operator pointing at their own internals is mostly fine. Locked for v1: a config flag `PULSE_BLOCK_PRIVATE_NETWORKS` (default false, allow). When set to true, Pulse refuses to check URLs that resolve to loopback, link-local, or RFC1918 private ranges and records the check as a failure with reason `blocked_target` (it does not silently pass). Default is allow because it's the operator's own box; the flag matters when the UI is shared with less-trusted teammates. See section 11.
- **Stored secrets:** SMTP passwords, webhook URLs, and per-monitor secret headers are sensitive. Two things, both locked for v1: (1) never return secret values back to the frontend after they're saved (write-only fields, show "configured" not the value), and (2) encrypt secret columns at rest with AES-256-GCM using a key from env var `PULSE_SECRET_KEY`. If the key is missing or invalid at startup, the app refuses to start (fail closed) so we never silently store plaintext. Key rotation is out of scope for v1. See section 11 and 12.6.
- **Auth for the UI/API:** the dashboard and API must require authentication. v1: a single admin credential whose username and password come from env vars `PULSE_ADMIN_USER` and `PULSE_ADMIN_PASSWORD` at startup. The password is stored hashed (bcrypt or argon2), never in plaintext. Login creates a session backed by an httpOnly, secure, SameSite cookie. There is no setup screen and no in-UI password change in v1. No anonymous access to any data or mutating endpoint. The only unauthenticated endpoint is `GET /healthz`, a basic liveness check for Pulse itself. See section 11 for the full auth decision.
- **Don't be an open relay / proxy:** because we POST to generic webhooks and make arbitrary requests, keep the instance authenticated so it can't be abused by outsiders to send traffic on their behalf.
- **Transport:** assume the operator puts TLS in front (reverse proxy). Support running behind a proxy (respect a base path / forwarded headers config). We don't need to terminate TLS ourselves in v1.

## 6. Data model (conceptual)

Entities and relationships:

- **Monitor** - one row per thing being checked. Holds all fields from 4.1. Has a derived current status that we can compute or cache.
- **NotificationChannel** - one row per destination. Has type and a JSON config blob. Secret fields inside the config (webhook URLs, SMTP password) are stored encrypted at rest per 12.6; non-secret fields can be plaintext. Reusable across monitors.
- **MonitorChannel** (join) - many-to-many between Monitor and NotificationChannel. A monitor can notify several channels; a channel can serve many monitors.
- **CheckResult** - one row per executed check. Belongs to a Monitor. This is the high-volume table (subject to retention cleanup).
- **Incident** - one row per down-period. Belongs to a Monitor. Has started_at (timestamp of the first failing check in the run that opened it), ended_at (nullable while open), cause/first-failure-reason, a close_reason (`recovered` or `disabled`, null while open), and links to the failure. Open incident = monitor currently considered down. At most one open incident per monitor at a time.
- **User / Credential** - for v1 a single admin credential (username + bcrypt/argon2 password hash) seeded from env vars at startup, plus active sessions. If we keep it as a table it's future-proof for multi-user, but one row is fine. On startup, if the username changed or no credential row exists, we upsert the row from the env vars; if the password env var changed, we re-hash and update.

Relationships:

- Monitor 1..* CheckResult
- Monitor 1..* Incident
- Monitor *..* NotificationChannel (via MonitorChannel)

## 7. API surface (conceptual REST)

All endpoints (except auth login and the Pulse liveness check) require an authenticated session. Shared conventions (timestamp format, error shape, pagination, redaction) are specified once in section 12.3 and apply to every endpoint below.

Auth:
- `POST /api/auth/login` - exchange credentials for a session cookie.
- `POST /api/auth/logout`
- `GET /api/auth/me` - current session info, used by the UI to know if logged in.

Monitors:
- `GET /api/monitors` - list with current status, last latency, last check time.
- `POST /api/monitors` - create.
- `GET /api/monitors/{id}` - full detail.
- `PUT /api/monitors/{id}` - update.
- `DELETE /api/monitors/{id}` - delete (and its history/incidents, or cascade per decision).
- `POST /api/monitors/{id}/check` - run a check right now (manual trigger), useful for testing a new monitor. Returns the resulting check result. If a check for this monitor is already in flight, returns 409 (see 4.2).
- `GET /api/monitors/{id}/results?range=24h` - check history for the detail view / chart. Supports `range` (`24h`, `7d`, `30d`) and cursor pagination per 12.3.
- `GET /api/monitors/{id}/incidents` - incidents for this monitor. Paginated per 12.3.

Channels:
- `GET /api/channels` - list (secrets redacted).
- `POST /api/channels` - create.
- `PUT /api/channels/{id}` - update (secret fields write-only).
- `DELETE /api/channels/{id}`
- `POST /api/channels/{id}/test` - send a test notification.

Incidents:
- `GET /api/incidents?status=open` - global incident list for the incidents screen.

Health of Pulse itself:
- `GET /healthz` - unauthenticated liveness for the operator's own monitoring.

The frontend is a Lit SPA served by the Go binary; it talks only to these JSON endpoints.

## 8. Success metrics / acceptance criteria

v1 is done when:

1. A user can create an HTTP monitor through the UI, and within roughly one interval Pulse has checked it and shows a status.
2. When the monitored endpoint goes down (returns wrong code, times out, or refuses connection), a down notification arrives on every attached channel within ~2 check intervals, and an incident opens.
3. When it recovers, exactly one recovery notification arrives and the incident closes with the right duration.
4. `failure_threshold` works: with threshold 3, a single failed check does not alert; three in a row does.
5. The latency and body-contains assertions work: a 200 that's too slow or missing the substring is correctly marked unhealthy with the right reason.
6. All four channel types (Slack, Discord, generic webhook, SMTP email) deliver a test message and a real alert.
7. With ~200 monitors at 30-60s intervals on a 1 vCPU box, checks run on schedule without piling up and the box stays responsive.
8. The UI requires login; an unauthenticated request to any data/mutation endpoint is rejected.
9. Secrets are never sent back to the browser after saving, and secret columns in the DB are encrypted (not plaintext). The app refuses to start if `PULSE_SECRET_KEY` is missing or invalid.
10. After a restart, all monitors, channels, history, and open-incident state are intact and checking resumes automatically.

Soft product metrics to watch post-launch: time-to-first-monitor (should be a couple of minutes), and ratio of useful alerts to noise (are people setting failure_threshold up because we're too noisy?).

## 9. Key product decisions and open questions

Decisions made, with reasoning and trade-offs:

1. **Default alert rule = fire after 1 failure, recovery on, no re-notify.**
   Why: simplest thing that's actually useful. One down, one up, per incident. Trade-off: a single flaky check alerts you. Mitigation: `failure_threshold` is per-monitor so anyone bothered can set it to 2-3. Re-notify-while-down is a known phase-2 ask; default would be off.

2. **HTTP-only monitors in v1.** No TCP/ICMP/TLS-expiry/DNS.
   Why: HTTP covers the overwhelming majority of "is my service up" needs and keeps the check engine simple. Trade-off: can't watch a raw database port or cert expiry yet. The check engine and data model are built so adding a `type` to Monitor later is cheap.

3. **expected_status_codes accepts an explicit list and the `2xx/3xx/4xx/5xx` shorthand, default `200`.**
   Why: covers the common "200 or 204" case and "any 2xx is fine" without a regex or expression language. Trade-off: someone wanting "200 or 301 but not 302" has to list them; that's fine. We will not build a general assertion DSL in v1.

4. **Channels are reusable entities, attached many-to-many to monitors.**
   Why: you configure your Slack once and point everything at it. Trade-off: slightly more UI than inline-per-monitor config, but far less duplication and secret sprawl.

5. **Single shared admin auth in v1, not multi-user.**
   Why: target is a small team self-hosting one instance; full user management is a large feature for little v1 value. Trade-off: no per-person audit trail or permissions. The credential is modeled in a way that doesn't block adding users later.

6. **Retention default 30 days of raw check results, configurable, no rollups.**
   Why: 200 monitors at 60s for 30 days is well within SQLite's comfort zone, and raw data keeps the history view simple and accurate. Trade-off: longer retention or far more monitors will eventually want rollups; that's a known phase-2 path.

7. **Secrets are write-only over the API and redacted on read, and encrypted at rest.**
   Why: keeps secrets out of the browser and reduces blast radius of a leaked DB file. Trade-off: encryption adds key-management responsibility for the operator (lose the key, lose the secrets). Locked for v1: AES-256-GCM with a key from `PULSE_SECRET_KEY`, fail closed if missing. See section 11 and 12.6.

8. **SSRF protection is opt-in, default allow.**
   Why: it's a single-tenant self-host pointing at the operator's own infra, and blocking private ranges by default would break the common "monitor my internal service" case. Trade-off: if the UI is later shared with less-trusted people, allow-by-default is risky, so the config flag `PULSE_BLOCK_PRIVATE_NETWORKS` (default false) lets the operator block private/loopback/link-local targets. See section 11.

All earlier open questions are now decided. They are locked in section 11:

- **Delete cascade:** yes, cascade, with a confirm dialog.
- **Minimum interval:** hard floor of 30s, operator cannot go below it.
- **Re-notify while down:** off in v1, no config for it.
- **First-run setup:** admin credential from env vars at startup, no setup screen.
- **Editing assertions on a down monitor:** does not auto-close the open incident.

## 10. Phased delivery

### Phase 0 - thin vertical slice (the MVP that proves the loop)

The smallest thing that does register -> check -> detect -> notify end to end:

- Create an HTTP GET monitor with URL, expected status code (default 200), timeout, interval (via API; minimal UI or even seed config).
- Scheduler that runs enabled monitors on their interval, concurrently, with timeout.
- Health decision on status code + timeout + connection failure only.
- One notification channel: Slack webhook.
- Alert rule: fire after 1 failure, send recovery. Incident open/close.
- Store check results in SQLite.
- A bare monitors-list page showing up/down and last latency, behind a login.

If this works, the product works. Everything else is enrichment.

### Phase 1 - the real v1 (everything in sections 3-8)

- Full monitor fields: method, headers, body, multiple/range status codes, max_latency, body_contains, failure_threshold, enabled toggle.
- All four channels (Slack, Discord, generic webhook, SMTP) with test-send.
- Full UI: monitor CRUD, detail with history + latency chart, channels screen, incidents screen.
- Retention cleanup job.
- Secret redaction and at-rest encryption (both in v1, per section 11).
- Manual "check now" trigger.
- SSRF block flag.

### Phase 2 (deferred, not committed)

- TCP / TLS-cert-expiry / DNS / ICMP check types.
- Status pages (public).
- Maintenance windows / mute.
- Re-notify while down, escalation, more channels (SMS, PagerDuty, Telegram).
- Multi-user + roles.
- Hourly rollups for long retention and large monitor counts.
- Tags/groups, Prometheus metrics export, API tokens.
- Secret key rotation / re-encryption.
- In-UI password change and setup screen.

## 11. Locked decisions (final)

These were open or recommended before. They are now decided. Build to these, do not re-litigate in design.

1. **At-rest encryption of secrets: IN v1.** Secret fields (webhook URLs, SMTP password, secret request headers) are encrypted with AES-256-GCM. The key comes from env var `PULSE_SECRET_KEY`, which must be 32 bytes encoded as base64. If the key is missing or doesn't decode to 32 bytes at startup, the app refuses to start (fail closed); it never falls back to plaintext. Each encrypted value uses a fresh random nonce. Key rotation and re-encryption are out of scope for v1.

2. **First-run auth: env vars, no setup screen.** Admin username and password come from `PULSE_ADMIN_USER` and `PULSE_ADMIN_PASSWORD` at startup. The password is stored only as a bcrypt or argon2 hash. Login issues an httpOnly, secure, SameSite session cookie. There is no setup wizard and no in-UI password/credential change in v1. If the env vars are missing, the app refuses to start.

3. **Delete cascade: yes, with confirm dialog.** Deleting a monitor deletes its check results and incidents. The UI shows a confirm dialog before the destructive delete.

4. **Minimum interval: 30s hard floor.** `interval_seconds` cannot be set below 30 through any path (UI or API). The API rejects lower values with a validation error.

5. **Re-notify while down: off in v1.** One down notification and one recovery notification per incident. There is no config option for reminders in v1.

6. **SSRF: opt-in block.** Config flag `PULSE_BLOCK_PRIVATE_NETWORKS`, default false (allow). When true, checks whose target resolves to loopback, link-local, or private (RFC1918) ranges fail with reason `blocked_target` and the request is not sent.

7. **Editing a monitor's assertions does not auto-close an open incident.** When config changes on a monitor that currently has an open incident, the incident stays open. We do not re-run or re-evaluate past results against the new config. The next check uses the new config and its result drives state as normal (healthy closes + recovery alert, unhealthy keeps it open). This is intentional so an edit can't silently mask a real outage.

## 12. Detailed specs

### 12.1 Monitor current status values

A monitor's current status is one of exactly four values, derived (not free-typed):

- `disabled` - `enabled` is false. Takes priority over everything else. A disabled monitor shows `disabled` even if it has past results and even if it had an open incident (which was closed on disable).
- `pending` - `enabled` is true and the monitor has zero check results yet (never been checked, e.g. just created and the first scheduled check hasn't run). Shown until the first result lands.
- `down` - `enabled` is true, has at least one result, and there is an open incident for it. The monitor is in a confirmed down state per the alerting machine. Note: a single failing check before `failure_threshold` is reached does NOT make status `down`; status stays `up` until the incident opens.
- `up` - `enabled` is true, has at least one result, and there is no open incident.

Derivation order to evaluate top to bottom: disabled -> pending -> down -> up. "Last check time" is the timestamp of the most recent check result (null when `pending`). "Last latency" is the latency of the most recent check result (null when `pending` or when the last check had no latency, e.g. a connection error or blocked target).

### 12.2 How the monitors-list computes status efficiently

The list endpoint returns, per monitor: status (from 12.1), last_check_at, last_latency_ms, and whether an incident is open. This is derived from the latest check result plus the open-incident flag for each monitor. Implementation is free to keep a small cached current-status per monitor updated on each check, or to compute it with a query; either is fine as long as the values match 12.1. Status is never stored as a user-editable field.

### 12.3 API conventions

These apply to every JSON endpoint.

- **Timestamps:** all timestamps in requests and responses are RFC3339 with a UTC offset, e.g. `2026-06-21T14:03:00Z`. The server stores and computes in UTC. The frontend converts to the viewer's local timezone for display only; the API never emits local time. Durations (like incident length) are returned in whole seconds as an integer field, plus a human string is fine for convenience but the integer is the source of truth.

- **Error shape:** every non-2xx response returns a JSON body of the form:

  ```json
  {
    "error": {
      "code": "validation_error",
      "message": "interval_seconds must be at least 30",
      "fields": {
        "interval_seconds": "must be at least 30"
      }
    }
  }
  ```

  `code` is a short stable string (`validation_error`, `not_found`, `unauthorized`, `conflict`, `internal_error`). `message` is human-readable. `fields` is optional and present only for validation errors, mapping field name to a per-field message. HTTP status follows the obvious mapping: 400 validation, 401 unauthorized, 404 not found, 409 conflict (e.g. overlapping check now), 500 internal.

- **Pagination (list endpoints: results, incidents):** cursor-based. A list response is:

  ```json
  {
    "items": [ ... ],
    "next_cursor": "opaque-string-or-null"
  }
  ```

  Request takes `limit` (default 100, max 500) and `cursor` (opaque, omitted for the first page). When `next_cursor` is null there are no more pages. Results are ordered newest-first by check/incident time. The `range` filter on results (`24h`, `7d`, `30d`) bounds the window before pagination applies. Small fixed lists (monitors, channels) are returned in full without pagination in v1.

- **Redaction:** secret fields are never returned in any response. On read, a channel's config returns non-secret fields plus a boolean per secret field indicating whether it is set, e.g. `"smtp_password_set": true`, never the value. For monitors, headers marked secret (see 12.4) return their key and `"secret": true` with the value omitted; non-secret headers return key and value. Write requests accept the secret value; an update that omits a secret field leaves the stored value unchanged (so the UI doesn't have to re-enter secrets on every edit), and sending an explicit empty string clears it.

### 12.4 Per-field validation rules (monitors)

Enforced on create and update, server-side. UI mirrors them but the server is authoritative.

- `name`: required, non-empty after trim, max 200 chars.
- `url`: required, must parse as an absolute URL with scheme `http` or `https` only. Other schemes (`file`, `ftp`, `gopher`, etc.) are rejected. Must have a host.
- `method`: required, one of GET, POST, PUT, PATCH, DELETE, HEAD. Default GET.
- `headers`: optional list of {key, value, secret?}. Keys non-empty, no duplicate keys, reasonable count cap (e.g. max 50). `secret` is a per-header bool (default false); when true the value is encrypted at rest and redacted on read.
- `body`: optional string. Only allowed for methods that take a body (POST, PUT, PATCH). Rejected (must be empty) for GET, HEAD, DELETE. Size cap (e.g. 1 MB).
- `expected_status_codes`: required. Accepts a list of explicit codes (each 100..599) and/or the shorthands `2xx`, `3xx`, `4xx`, `5xx`. Must be non-empty. Default `200`.
- `timeout_seconds`: required, integer 1..60. Default 10.
- `interval_seconds`: required, integer, minimum 30 (hard floor), no enforced max in v1 but must be >= timeout_seconds. Default 60.
- `enabled`: required bool, default true.
- `max_latency_ms`: optional, if set must be a positive integer.
- `body_contains`: optional string, max length cap (e.g. 1000 chars). If set, the engine reads up to the 64 KB body cap to test it.
- `failure_threshold`: required, integer >= 1. Default 1.
- `notification_channel_ids`: optional list. Each id must reference an existing channel. Empty list is allowed (tracked but silent).

Validation errors return the 12.3 error shape with per-field messages.

### 12.5 Alerting state machine (acceptance table)

State per monitor: a running count of consecutive unhealthy checks and whether an incident is open. `T` = `failure_threshold`. Example below uses `T = 3`. A healthy check is `H`, unhealthy is `F`.

| Step | Check | Consecutive fails before | Action | Consecutive fails after | Incident | Status | Notification |
|------|-------|--------------------------|--------|--------------------------|----------|--------|--------------|
| 1 | H | 0 | none | 0 | none | up | none |
| 2 | F | 0 | count++ | 1 | none | up | none |
| 3 | H | 1 | reset (blip absorbed) | 0 | none | up | none |
| 4 | F | 0 | count++ | 1 | none | up | none |
| 5 | F | 1 | count++ | 2 | none | up | none |
| 6 | F | 2 | count++ reaches T, open incident | 3 | open (started_at = step 4 check time) | down | ONE down alert to each attached channel |
| 7 | F | 3 | stay down, no re-notify | 4 | open | down | none |
| 8 | H | 4 | close incident, reset count | 0 | closed (ended_at = step 8 check time, close_reason recovered) | up | ONE recovery alert to each attached channel |

With `T = 1`: a single `F` at step 2 immediately opens the incident and sends the down alert; the next `H` closes it and sends recovery.

Extra rules captured by the machine:
- `started_at` is the timestamp of the FIRST failing check in the run that opened the incident (step 4 above), not the check that crossed the threshold (step 6). Duration on recovery = `ended_at - started_at`.
- Any `H` while count > 0 but no incident open resets count to 0 with no notification (step 3).
- Disabling a down monitor: incident closes with `ended_at` = disable time, `close_reason = disabled`, NO recovery alert, status becomes `disabled`.
- Editing a down monitor's config: incident stays open, count is left as-is, next check (with new config) drives the transition normally.
- A monitor with zero attached channels still opens and closes incidents and changes status; it just sends no notifications.

### 12.6 Secret encryption details

- Algorithm: AES-256-GCM. Key: 32 bytes from `PULSE_SECRET_KEY` (base64-encoded in the env var).
- Startup check: decode the key, verify it is exactly 32 bytes. If missing or wrong size, log a clear error and exit non-zero before serving any request.
- Per value: generate a fresh random 96-bit nonce, store nonce + ciphertext + GCM tag together (e.g. base64 in the column). Each secret field is encrypted independently.
- What is encrypted: channel secret config fields (Slack/Discord webhook URLs, generic-webhook URL, SMTP password) and monitor headers flagged `secret`. Non-secret config and headers are stored plaintext.
- Out of scope v1: rotating the key or re-encrypting existing data under a new key. Document that losing the key means the stored secrets are unrecoverable and must be re-entered.

### 12.7 Notification payloads

#### Generic webhook (POST, Content-Type application/json)

Same envelope for both event types; `event` distinguishes them. Field names and types are fixed:

```json
{
  "event": "down",
  "monitor": {
    "id": "mon_123",
    "name": "Prod API health",
    "url": "https://api.example.com/health",
    "method": "GET"
  },
  "incident": {
    "id": "inc_456",
    "started_at": "2026-06-21T14:00:00Z",
    "ended_at": null
  },
  "check": {
    "checked_at": "2026-06-21T14:00:30Z",
    "healthy": false,
    "failure_reason": "status_mismatch",
    "status_code": 503,
    "latency_ms": 120,
    "error": null
  },
  "sent_at": "2026-06-21T14:00:31Z"
}
```

Field types:
- `event`: string, `"down"` or `"recovery"`.
- `monitor.id`, `monitor.name`, `monitor.url`, `monitor.method`: strings.
- `incident.id`: string. `incident.started_at`: RFC3339 string. `incident.ended_at`: RFC3339 string on recovery, `null` on down.
- `check.checked_at`: RFC3339 string. `check.healthy`: bool. `check.failure_reason`: string or null (one of the reasons in 4.2; null on recovery). `check.status_code`: integer or null. `check.latency_ms`: integer or null. `check.error`: short string or null.
- `sent_at`: RFC3339 string, when Pulse sent the notification.

For a recovery event: `event` is `"recovery"`, `incident.ended_at` is set, `check.healthy` is true, `check.failure_reason` is null. A `duration_seconds` integer is added at the top level on recovery only:

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

Generic-webhook custom headers configured on the channel are sent with the request.

#### Slack (incoming webhook)

POST to the configured webhook URL with a `text` field (Slack renders it). Down example:

```json
{ "text": ":red_circle: *DOWN* Prod API health\nhttps://api.example.com/health\nReason: status_mismatch (HTTP 503)\nDown since 2026-06-21 14:00:00 UTC" }
```

Recovery example:

```json
{ "text": ":large_green_circle: *RECOVERED* Prod API health\nhttps://api.example.com/health\nWas down for 10m 0s (since 2026-06-21 14:00:00 UTC)" }
```

#### Discord (incoming webhook)

POST with a `content` field (Discord's equivalent of `text`). Same human text as Slack, Discord-flavored:

```json
{ "content": "**DOWN** Prod API health\nhttps://api.example.com/health\nReason: status_mismatch (HTTP 503)\nDown since 2026-06-21 14:00:00 UTC" }
```

Recovery uses `**RECOVERED**` and the down-duration line. Emoji optional, plain markdown is fine.

#### Email (SMTP)

Subject: `[Pulse] DOWN: Prod API health` or `[Pulse] RECOVERED: Prod API health`. Body is plain text (HTML optional) with the same facts: monitor name, URL, reason + status/latency, when it went down, and on recovery the duration. Sent from the configured `from` address to the configured recipients over the configured host/port with TLS per the channel config.

All notification timestamps in human-readable bodies are shown in UTC with the `UTC` suffix so there's no ambiguity; the API/webhook fields use RFC3339 per 12.3.
