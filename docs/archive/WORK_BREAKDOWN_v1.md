# Pulse - Work Breakdown

How to split the build into independent, parallelizable work packages (WPs). The point is that once the foundation contracts exist (domain structs, the `Store` interface, crypto, migrations), several packages can be built at the same time by different people against stable seams, then wired together.

Read `docs/ARCHITECTURE.md` for the actual interface signatures each WP implements. The contracts there are the agreement between WPs.

## Dependency layers at a glance

```
Layer 0 (foundation, build first, blocks everything):
   WP1 module + config + domain
   WP2 crypto
   WP3 store interface + schema + migrations + impl

Layer 1 (parallel, each depends only on Layer 0 contracts):
   WP4 checker
   WP5 alerting
   WP6 notify
   WP7 auth

Layer 2 (depends on Layer 1):
   WP8 scheduler            (needs checker + alerting + notify + store)

Layer 3 (wiring, needs everything above):
   WP9 api + main wiring    (needs store, auth, scheduler, notify, checker, validation)

Layer 4 (frontend, depends only on the API contract from WP9, can start against a mock):
   WP10 frontend foundation (build, router, api client, auth)
   WP11 frontend monitors + channels + incidents views + chart

Layer 5:
   WP12 end-to-end integration + acceptance tests
```

What can run concurrently:
- Layer 0: WP2 and WP3 can go in parallel once WP1's domain structs exist; WP3 depends on WP2 (store uses crypto) so finish WP2's `crypto` signatures first (they're tiny).
- Layer 1: WP4, WP5, WP6, WP7 are fully independent of each other. Four people, four packages, in parallel.
- Layer 3 and Layer 4: once the API shapes are frozen (they're in the PRD + ARCHITECTURE), the frontend (WP10/WP11) can start against a mock API in parallel with WP8/WP9. They meet at integration (WP12).

---

## WP1 - Module, config, domain model

**Owns:** `go.mod`, `cmd/pulse/main.go` (skeleton only), `internal/config/`, `internal/domain/` (`domain.go`, `status.go`).

**Depends on:** nothing. This is the root.

**Scope:** Set up the Go module (Go 1.26), the directory skeleton, and the shared vocabulary every other package imports. Define all `domain` structs and enums exactly as in ARCHITECTURE 3.1 (Monitor, Header, Channel, ChannelType, CheckResult, FailureReason, Incident, CloseReason, AlertState, Status) and the pure `DeriveStatus` function (PRD 12.1). Implement `config` to read and validate every env var from ARCHITECTURE section 6, with the fail-closed behavior for required ones (return an error; main exits non-zero). `main.go` is a stub that loads config and exits for now. This unblocks everyone, so do it first and freeze the structs. Unit test: `DeriveStatus` order, config required/default/invalid cases.

---

## WP2 - Crypto (secret encryption)

**Owns:** `internal/crypto/`.

**Depends on:** WP1 (module only).

**Scope:** Implement AES-256-GCM per PRD 12.6 and ARCHITECTURE 3.7: `LoadKey(b64)` decodes and checks 32 bytes (fail closed), `Encrypt`/`Decrypt` with a fresh 96-bit nonce per value, output base64(nonce||ciphertext||tag). Small and self-contained. Get its signatures landed early because WP3 (store) imports it. Unit tests: round-trip, nonce uniqueness, tamper detection, bad-key rejection.

---

## WP3 - Store: interface, schema, migrations, impl

**Owns:** `internal/store/` (all files), `internal/store/migrations/0001_init.sql`.

**Depends on:** WP1 (domain), WP2 (crypto, for encrypting secret columns).

**Scope:** This is the keystone contract. First land the `Store` interface and query/struct types from ARCHITECTURE 3.2 so Layer 1 packages can compile against it (they only need the interface, not the impl). Then implement: open SQLite with `modernc.org/sqlite`, apply PRAGMAs (WAL, busy_timeout, foreign_keys), the hand-rolled embedded migration runner with `schema_migrations`, and every method against the schema in ARCHITECTURE section 4. Encrypt `monitor_headers.value` (when secret) and the secret keys inside `channels.config` via crypto on write, decrypt on read. Enforce one-open-incident via the partial unique index. Implement results pagination (range + cursor, newest-first) and retention delete. Integration tests against a real temp SQLite file: CRUD, cascade delete, pagination, retention, the open-incident invariant. Landing the interface early matters more than landing the impl; split into "interface PR" then "impl PR" if helpful.

---

## WP4 - Checker

**Owns:** `internal/checker/` (`checker.go`, `ssrf.go`, `statuscodes.go`).

**Depends on:** WP1 (domain). Independent of store/alerting/notify.

**Scope:** Implement `Checker.Check(ctx, monitor) *CheckResult` per ARCHITECTURE 3.3 and PRD 4.2: per-check timeout context, wall-clock latency, body cap (64 KB, only when `body_contains` set), assertion priority order, the failure reasons, and the SSRF guard (pre-resolve for the clean `blocked_target` reason plus a dialer `Control` re-check to close the TOCTOU gap) controlled by the block flag. `statuscodes.go` parses the `expected_status_codes` spec (explicit codes + `2xx/3xx/4xx/5xx`) into a matcher and exposes `ParseStatusCodes` (reused by api validation). Heavy unit tests with `httptest`: every reason, priority order, timeout cut-off, latency assertion, body cap edge, SSRF block-and-don't-send.

---

## WP5 - Alerting state machine

**Owns:** `internal/alerting/`.

**Depends on:** WP1 (domain). Independent of everything else.

**Scope:** Implement the pure `Engine.Apply(monitor, result, state) Decision` per PRD 12.5 and ARCHITECTURE 3.5: consecutive-fail counting, threshold crossing to open an incident, healthy-closes-incident with recovery event, `IncidentStartedAt` from `FirstFailAt`, no re-notify while down, count reset on healthy. No DB, no notify imports. The deliverable that proves it is the table-driven test that encodes the 12.5 acceptance table (T=3 and T=1 sequences) plus blip-reset. The disable-while-down and edit-while-down rules are not in `Apply` (they're handler-driven, built in WP9) but document them here so WP9 implements them right.

---

## WP6 - Notify

**Owns:** `internal/notify/` (`notify.go`, `slack.go`, `discord.go`, `webhook.go`, `smtp.go`, `render.go`).

**Depends on:** WP1 (domain). Independent of store/scheduler.

**Scope:** Implement the `Notifier` interface and the `Manager` per ARCHITECTURE 3.6: four notifiers (Slack `text`, Discord `content`, generic webhook envelope, SMTP email) with payloads byte-for-byte per PRD 12.7, the `render.go` human strings (UTC suffix), `Manager.Dispatch` fanning out per channel concurrently with retry/backoff and recording failures, and `Manager.Test` for the UI test-send. Channels arrive decrypted (store handles that), so notify just reads plaintext config. Unit tests against a fake HTTP server: assert exact payloads, `duration_seconds` only on recovery, `ended_at` null on down, custom headers on generic webhook, retry-then-give-up behavior. SMTP tested via an in-process capture or a seam.

---

## WP7 - Auth

**Owns:** `internal/auth/`.

**Depends on:** WP1 (domain), WP3 (Store interface, for sessions + admin).

**Scope:** Implement password hashing (bcrypt), session create/validate/delete, `SeedAdmin` (upsert from env, re-hash on password change per PRD 11.2 / 6), `Login`/`Logout`/`Current`, and the route-protecting `Middleware` that writes the 12.3 401 shape and does not redirect. Cookie: httpOnly, Secure (configurable), SameSite=Lax. Only needs the `Store` interface, so it can be built against WP3's interface before the impl is done. Unit tests: hash round-trip, middleware allow/deny, seed-admin upsert.

---

## WP8 - Scheduler

**Owns:** `internal/scheduler/` (`scheduler.go`, `pool.go`, `locks.go`).

**Depends on:** WP3 (Store), WP4 (checker), WP5 (alerting), WP6 (notify). This is the integrator of Layer 1.

**Scope:** Implement the min-heap dispatcher + bounded worker pool + per-monitor locks + control channel for live changes + startup rebuild, per ARCHITECTURE 3.4 and section 5. Implement `Start`, `Upsert`, `Remove`, `CheckNow` (with `ErrCheckInFlight`). The worker body ties checker -> store.InsertResult -> store.GetAlertState -> alerting.Apply -> persist incident + consecutive_fails + first_fail_at -> notify.Dispatch. Also owns the retention ticker (or a tiny helper) calling `store.DeleteResultsBefore`. Tests: heap ordering, cadence stability (nextRun += interval), per-monitor lock prevents overlap, CheckNow 409 path, worker-pool bound. Can use fakes for checker/notify to test scheduling in isolation, real ones in WP12.

---

## WP9 - API + main wiring

**Owns:** `internal/api/` (all handlers, middleware, errors, static), the real `cmd/pulse/main.go` wiring, request validation (PRD 12.4), redaction (12.3).

**Depends on:** WP3 (store), WP4 (checker `ParseStatusCodes` for validation), WP5 (only indirectly), WP6 (notify, for test-send), WP7 (auth middleware + handlers), WP8 (scheduler, for CheckNow + Upsert/Remove).

**Scope:** Wire everything. Implement the route table (ARCHITECTURE 3.9) with Go 1.22 ServeMux method+path patterns, the middleware chain (recover -> log -> base-path -> auth on `/api/*`), the 12.3 JSON error writer, all monitor/channel/incident/result/auth handlers, full 12.4 validation with per-field errors, redaction on read and the omit-keeps / empty-clears secret rule on write, cascade-delete confirm wiring, and the embedded SPA + static serving with SPA fallback. On create/edit/delete/enable, call `scheduler.Upsert`/`Remove`; implement the disable-while-down and edit-while-down incident rules here (per WP5's notes). `main.go` does the full startup order from ARCHITECTURE section 6 (fail-closed checks first). Tests: validation rules, redaction, status mapping, the CheckNow 409.

---

## WP10 - Frontend foundation

**Owns:** `web/` build setup (`package.json`, `vite.config.ts`, `tsconfig.json`, `index.html`), `src/main.ts`, `src/router.ts`, `src/api/client.ts`, `src/api/types.ts`, `src/state/session.ts`, `src/components/app-root.ts`, `app-nav.ts`, `login-view.ts`, `status-badge.ts`, `confirm-dialog.ts`, styles, and `internal/web/embed.go` + the Makefile embed step.

**Depends on:** the API contract (PRD section 7 + 12.3). Can start in parallel with WP8/WP9 against a mock API; only needs the real server for final integration.

**Scope:** Stand up Vite + Lit + TS, the hand-rolled History-API router honoring `PULSE_BASE_PATH`, the typed `fetch` client (credentials include, 401 -> login redirect, 12.3 error parsing), the session check via `GET /api/auth/me`, login/logout flow, the app shell + nav, and shared `status-badge` / `confirm-dialog`. Set up the `web/dist` -> `internal/web/dist` embed flow and the `embed.go`. This unblocks WP11.

---

## WP11 - Frontend feature views

**Owns:** `src/components/monitors-list-view.ts`, `monitor-detail-view.ts`, `monitor-form-view.ts`, `channels-view.ts`, `channel-form.ts`, `incidents-view.ts`, `latency-chart.ts`, `sparkline.ts`.

**Depends on:** WP10 (router, client, shell, shared components).

**Scope:** Build the feature screens from PRD 4.6: monitors list (status badge, last latency/check, enable toggle, sparkline), monitor detail (config, history table, latency chart, incidents), monitor create/edit form (all 4.1 fields, channel multi-select, client validation mirroring 12.4, secret fields write-only with "configured" state), channels CRUD with type-specific config and the "send test" button, and the global incidents list. Implement the custom SVG `latency-chart` and `sparkline` (no chart lib). Surface 12.3 field errors from the API on forms.

---

## WP12 - End-to-end integration & acceptance

**Owns:** `cmd/pulse` integration tests, an end-to-end harness, the acceptance checklist from PRD section 8.

**Depends on:** everything (WP1-WP11).

**Scope:** Wire the full app against a temp DB, a fake monitored endpoint (`httptest`), and a fake notification sink. Drive the PRD section 8 acceptance criteria: create monitor -> check -> status; failing endpoint -> incident opens + one down alert; recovery -> one recovery alert + incident closes with correct duration; `failure_threshold=3` needs three fails; latency and body-contains assertions; all four channel types deliver a test and a real alert; auth required on every endpoint; secrets never returned and encrypted at rest; restart preserves state and resumes checking. Use `CheckNow` to drive transitions without waiting real intervals. Fix integration bugs found across seams. Also a smoke pass building the real single binary (frontend embedded) and logging in through the served SPA.

---

## Suggested staffing / order

- **Sprint 1 (foundation):** one person on WP1 (fast), then WP2 + WP3 (WP3 is the big one; consider splitting interface-PR vs impl-PR so Layer 1 can start on the interface).
- **Sprint 2 (parallel packages):** WP4, WP5, WP6, WP7 in parallel (up to four people). WP10 frontend foundation can also start here against a mock.
- **Sprint 3 (integration):** WP8 (scheduler) once Layer 1 lands, then WP9 (api + main). WP11 frontend views in parallel with WP9.
- **Sprint 4:** WP12 end-to-end, bug-fix across seams, ship Phase 0 slice first (PRD section 10) then the rest of v1.

The Phase 0 vertical slice in PRD section 10 maps to a thin path through WP1-WP3 + a GET-only WP4 + minimal WP5 + Slack-only WP6 + WP8 + a bare WP9/WP10. Build that path end to end first to prove the loop, then fill out the rest of each WP for full v1.
