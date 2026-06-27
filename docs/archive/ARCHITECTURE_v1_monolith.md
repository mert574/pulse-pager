# Pulse - Technical Architecture (v1)

Status: Design, ready to build
Audience: engineers implementing Pulse v1
Source of truth for product: `docs/PRD.md`. This doc must conform to the PRD exactly; where the PRD left something underspecified there is an "Architecture decisions" subsection that records the call and why.

Tech is fixed: Go 1.26 backend (stdlib first, `net/http` with the 1.22+ `ServeMux` routing), SQLite storage, Lit + Vite + TypeScript frontend embedded into the Go binary. Single binary, single process, config via env vars.

---

## 1. System overview

Pulse is one process. Inside it, a handful of goroutines do the work and talk through small in-memory channels plus the shared SQLite store. There is no message broker, no external cache, no second process.

The long-lived pieces:

- **HTTP server** (`api` package) - serves `/api/*` JSON endpoints and the embedded Lit SPA + static assets. Runs on the main goroutine's `http.Server`. Handlers call into store, scheduler, alerting, notify, crypto, auth.
- **Scheduler** (`scheduler` package) - one goroutine owns a min-heap of "next run" times. When a monitor is due it hands the job to a bounded worker pool. It also owns the per-monitor "check now" locks and rebuilds the heap from the DB on startup. It listens on a small control channel for live schedule changes (create/edit/enable/disable/delete and manual check-now).
- **Check workers** (pool inside `scheduler`, using `checker`) - a fixed-size pool of goroutines, bounded by a semaphore. Each worker takes one job, runs the HTTP check via `checker`, writes the result to the store, then feeds the result into `alerting`. Alerting may decide to fire notifications, which it dispatches through `notify`.
- **Retention cleanup job** (`scheduler` or a tiny `retention` helper) - a ticker goroutine that wakes hourly and deletes `check_results` older than the retention window.

Text diagram:

```
                         +-----------------------------------------------+
                         |                  pulse (1 process)            |
                         |                                               |
   browser  <--HTTP-->   |  [api/http.Server]                            |
                         |     |  serves SPA (embed) + /api/* + /healthz  |
                         |     |  auth middleware on /api/* (not login)   |
                         |     v                                          |
                         |  store (SQLite, single *sql.DB)               |
                         |     ^         ^            ^                   |
                         |     |         |            |                  |
                         |  [scheduler goroutine]     |                  |
                         |     | min-heap of next-run |                  |
                         |     | control chan <-------+--- api (CRUD,    |
                         |     |                       |    check-now)   |
                         |     v dispatch              |                  |
                         |  [worker pool] --(semaphore bound, e.g. 50)--  |
                         |     | checker.Check(ctx, monitor)             |
                         |     | -> store.InsertResult                   |
                         |     | -> alerting.Apply(result, state)        |
                         |     |        -> store (incident open/close)   |
                         |     |        -> notify.Send (Slack/Discord/   |
                         |     |             webhook/SMTP, retry+backoff) |
                         |     v                                          |
                         |  [retention ticker] hourly -> store.DeleteOld  |
                         +-----------------------------------------------+
                                  | outbound HTTP (checks + webhooks) and SMTP
                                  v
                          monitored endpoints / Slack / Discord / SMTP server
```

Data flow for a scheduled check, end to end:

1. Scheduler pops a due monitor from the heap, reschedules its next run (now + interval), and sends the job to the worker pool.
2. A worker takes the per-monitor lock, calls `checker.Check`, which makes the HTTP request with timeout and applies all assertions, returning a `CheckResult`.
3. Worker writes the result via `store.InsertResult`.
4. Worker loads the monitor's current alert state (consecutive-fail count + open incident) from the store, calls `alerting.Apply(monitor, result, state)`, which returns a `Decision` (open incident / close incident / nothing) plus the notifications to send.
5. Worker persists the incident change and dispatches notifications through `notify`. `notify` does its own retry/backoff and records send failures.
6. Worker releases the per-monitor lock.

Everything that matters survives a restart because it lives in SQLite. The scheduler holds only the heap of next-run times, which it rebuilds on boot. Alert state (fail count + open incident) is read from the DB, not held only in memory, so a restart resumes correctly.

---

## 2. Repository / project layout

```
pulse/
  go.mod
  go.sum
  Makefile                      # build frontend then backend into one binary
  README.md
  docs/
    PRD.md
    ARCHITECTURE.md
    WORK_BREAKDOWN.md
  cmd/
    pulse/
      main.go                   # wire everything: config, store, crypto, scheduler, api; start server
  internal/
    config/
      config.go                 # load + validate env vars, fail-closed checks
    domain/
      domain.go                 # plain structs shared across packages (Monitor, Channel, CheckResult, Incident, ...)
      status.go                 # status derivation (12.1), failure-reason consts, enums
    store/
      store.go                  # Store interface + domain queries, opens *sql.DB, runs migrations
      monitors.go               # monitor queries
      channels.go               # channel queries
      results.go                # check_results queries (insert, list with range+cursor, delete old)
      incidents.go              # incident queries
      sessions.go               # session + admin credential queries
      migrate.go                # embedded SQL migration runner
      migrations/
        0001_init.sql
    crypto/
      crypto.go                 # AES-256-GCM Encrypt/Decrypt + LoadKey startup check (12.6)
    checker/
      checker.go                # Checker: run one HTTP check, apply assertions, return CheckResult
      ssrf.go                   # private-range resolver guard (PULSE_BLOCK_PRIVATE_NETWORKS)
      statuscodes.go            # parse + match expected_status_codes (list + 2xx/3xx shorthand)
    scheduler/
      scheduler.go              # min-heap, dispatch loop, control channel, startup rebuild
      pool.go                   # bounded worker pool (semaphore)
      locks.go                  # per-monitor in-flight locks + check-now 409 behavior
    alerting/
      alerting.go               # state machine (12.5): Apply(monitor, result, state) -> Decision
    notify/
      notify.go                 # Notifier interface, Manager (fan-out + retry/backoff), Decision->payload
      slack.go
      discord.go
      webhook.go
      smtp.go
      render.go                 # human text for Slack/Discord/email bodies (12.7)
    auth/
      auth.go                   # password hash/verify, session create/validate, middleware
    api/
      router.go                 # ServeMux setup, route table, middleware chain
      middleware.go             # auth, request logging, panic recovery, JSON error writer
      monitors.go               # monitor handlers
      channels.go               # channel handlers
      incidents.go              # incident handlers
      results.go                # results handlers
      authh.go                  # login/logout/me handlers
      health.go                 # /healthz
      errors.go                 # error shape (12.3), validation error helpers
      static.go                 # serve embedded SPA + assets, SPA fallback to index.html
  web/                          # frontend (Vite + Lit + TS)
    package.json
    tsconfig.json
    vite.config.ts
    index.html
    src/
      main.ts                   # app bootstrap, mount root component, start router
      router.ts                 # tiny client-side router config
      api/
        client.ts               # fetch wrapper: JSON, credentials: include, 401 -> login redirect
        types.ts                # TS types mirroring the JSON API
      state/
        session.ts              # current-user state (from /api/auth/me), simple reactive holder
      components/
        app-root.ts             # shell: nav + <router-outlet>
        app-nav.ts
        login-view.ts
        monitors-list-view.ts
        monitor-detail-view.ts
        monitor-form-view.ts
        channels-view.ts
        channel-form.ts
        incidents-view.ts
        status-badge.ts         # up/down/disabled/pending pill
        latency-chart.ts        # tiny custom SVG line chart
        sparkline.ts            # tiny inline up/down sparkline for the list
        confirm-dialog.ts       # delete confirm
      styles/
        tokens.css              # colors, spacing
        global.css
  internal/web/
    embed.go                    # //go:embed of web/dist, exposes fs.FS to api.static
```

Note on the embed: Go `embed` can only see files under the embedding package's directory tree. So the build copies `web/dist` to `internal/web/dist` (or the embed file lives at repo root). The Makefile handles this. See section 9.

Backend package one-liners:

- `config` - read and validate env, fail closed on missing/invalid required vars.
- `domain` - shared plain structs and enums, no behavior, no imports of other internal packages. The stable vocabulary every package speaks.
- `store` - the only package that touches SQLite. Owns the `*sql.DB`, runs migrations, exposes the `Store` interface.
- `crypto` - AES-256-GCM encrypt/decrypt for secret fields, plus the startup key check.
- `checker` - executes one HTTP check and applies assertions, pure-ish (takes a monitor, returns a result). Owns SSRF guard and status-code matching.
- `scheduler` - decides when checks run, dispatches to the worker pool, owns per-monitor locks and live schedule updates.
- `alerting` - the state machine. Given a result and current state, returns what to do (open/close incident, which notifications).
- `notify` - turns alert decisions into channel messages, sends them with retry/backoff, records failures.
- `auth` - password hashing, session lifecycle, the route-protecting middleware.
- `api` - HTTP handlers, middleware chain, static/SPA serving. Wires all the above together.

---

## 3. Backend package design with interface contracts

These are the seams. Implement against them so packages can be built in parallel. Structs live in `domain` so no two implementing packages need to import each other.

### 3.1 `domain` - shared model

```go
package domain

import "time"

type Method string // "GET","POST","PUT","PATCH","DELETE","HEAD"

type Status string // derived, see status.go

const (
	StatusDisabled Status = "disabled"
	StatusPending  Status = "pending"
	StatusDown     Status = "down"
	StatusUp       Status = "up"
)

type FailureReason string

const (
	ReasonConnectionError  FailureReason = "connection_error"
	ReasonTimeout          FailureReason = "timeout"
	ReasonStatusMismatch   FailureReason = "status_mismatch"
	ReasonLatencyExceeded  FailureReason = "latency_exceeded"
	ReasonBodyAssertion    FailureReason = "body_assertion_failed"
	ReasonBlockedTarget    FailureReason = "blocked_target"
)

type Header struct {
	Key    string `json:"key"`
	Value  string `json:"value"`  // decrypted in memory; redacted when serialized to API if Secret
	Secret bool   `json:"secret"`
}

type Monitor struct {
	ID                  int64
	Name                string
	URL                 string
	Method              Method
	Headers             []Header
	Body                string
	ExpectedStatusCodes string // stored raw, e.g. "200,204" or "2xx"; parsed by checker
	TimeoutSeconds      int
	IntervalSeconds     int
	Enabled             bool
	MaxLatencyMs        *int     // nil = no assertion
	BodyContains        *string  // nil = no assertion
	FailureThreshold    int
	ChannelIDs          []int64  // attached channels
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type ChannelType string

const (
	ChannelSlack   ChannelType = "slack"
	ChannelDiscord ChannelType = "discord"
	ChannelWebhook ChannelType = "webhook"
	ChannelSMTP    ChannelType = "smtp"
)

// Channel.Config is the decrypted type-specific config as a map or typed struct.
// store encrypts the secret keys on write and decrypts on read; the rest of the
// app sees plaintext values in memory.
type Channel struct {
	ID      int64
	Name    string
	Type    ChannelType
	Config  map[string]any // typed per Type; secret fields decrypted in memory
	Enabled bool
}

type CheckResult struct {
	ID            int64
	MonitorID     int64
	CheckedAt     time.Time
	Healthy       bool
	FailureReason *FailureReason // nil when healthy
	StatusCode    *int           // nil on connection error / timeout / blocked
	LatencyMs     *int           // nil when no latency (conn error, blocked)
	ErrorText     *string        // short, truncated
}

type CloseReason string

const (
	CloseRecovered CloseReason = "recovered"
	CloseDisabled  CloseReason = "disabled"
)

type Incident struct {
	ID            int64
	MonitorID     int64
	StartedAt     time.Time   // first failing check in the run that opened it
	EndedAt       *time.Time  // nil while open
	CauseReason   FailureReason
	CloseReason   *CloseReason // nil while open
	FirstResultID *int64       // link to the failing check
}

// AlertState is the per-monitor alerting state, derived from stored data.
type AlertState struct {
	ConsecutiveFails int
	OpenIncident     *Incident // nil if none open
}
```

`status.go` holds the derivation from 12.1 as a pure function so api and tests share it:

```go
// DeriveStatus follows 12.1 order: disabled -> pending -> down -> up.
func DeriveStatus(enabled bool, hasResults bool, openIncident bool) Status
```

### 3.2 `store` - data access

One interface, split across files. Every method takes `context.Context`. `store` is the only package importing `database/sql` and the SQLite driver.

```go
package store

type Store interface {
	// lifecycle
	Migrate(ctx context.Context) error
	Close() error

	// monitors
	CreateMonitor(ctx context.Context, m *domain.Monitor) (int64, error)
	GetMonitor(ctx context.Context, id int64) (*domain.Monitor, error)
	ListMonitors(ctx context.Context) ([]*domain.Monitor, error)
	UpdateMonitor(ctx context.Context, m *domain.Monitor) error
	DeleteMonitor(ctx context.Context, id int64) error // cascades results + incidents + join rows
	SetMonitorEnabled(ctx context.Context, id int64, enabled bool) error
	ListEnabledMonitors(ctx context.Context) ([]*domain.Monitor, error) // scheduler boot

	// list-view derived data (12.2). One query per monitor or a join; either is fine.
	ListMonitorsWithStatus(ctx context.Context) ([]MonitorListItem, error)

	// channels
	CreateChannel(ctx context.Context, c *domain.Channel) (int64, error)
	GetChannel(ctx context.Context, id int64) (*domain.Channel, error)
	ListChannels(ctx context.Context) ([]*domain.Channel, error)
	UpdateChannel(ctx context.Context, c *domain.Channel) error
	DeleteChannel(ctx context.Context, id int64) error
	GetChannelsForMonitor(ctx context.Context, monitorID int64) ([]*domain.Channel, error)

	// check results
	InsertResult(ctx context.Context, r *domain.CheckResult) (int64, error)
	LatestResult(ctx context.Context, monitorID int64) (*domain.CheckResult, error)
	ListResults(ctx context.Context, q ResultQuery) ([]*domain.CheckResult, string, error) // items, nextCursor
	HasResults(ctx context.Context, monitorID int64) (bool, error)
	DeleteResultsBefore(ctx context.Context, cutoff time.Time) (int64, error) // retention

	// incidents
	OpenIncident(ctx context.Context, inc *domain.Incident) (int64, error)
	CloseIncident(ctx context.Context, id int64, endedAt time.Time, reason domain.CloseReason) error
	GetOpenIncident(ctx context.Context, monitorID int64) (*domain.Incident, error)
	ListIncidentsForMonitor(ctx context.Context, q IncidentQuery) ([]*domain.Incident, string, error)
	ListIncidents(ctx context.Context, q IncidentQuery) ([]*domain.Incident, string, error)

	// alert state (read consecutive-fail count + open incident together)
	GetAlertState(ctx context.Context, monitorID int64) (*domain.AlertState, error)
	SetConsecutiveFails(ctx context.Context, monitorID int64, n int) error

	// sessions + admin credential
	UpsertAdmin(ctx context.Context, username, passwordHash string) error
	GetAdmin(ctx context.Context) (*Admin, error)
	CreateSession(ctx context.Context, s *Session) error
	GetSession(ctx context.Context, token string) (*Session, error)
	DeleteSession(ctx context.Context, token string) error
	DeleteExpiredSessions(ctx context.Context, now time.Time) error
}

type MonitorListItem struct {
	Monitor      *domain.Monitor
	Status       domain.Status
	LastCheckAt  *time.Time
	LastLatency  *int
	IncidentOpen bool
}

type ResultQuery struct {
	MonitorID int64
	Range     string // "24h","7d","30d"; bounds the window
	Limit     int    // default 100, max 500
	Cursor    string // opaque, omitted for first page
}

type IncidentQuery struct {
	MonitorID *int64 // nil for global list
	Status    string // "open","" (all)
	Limit     int
	Cursor    string
}

type Admin struct {
	Username     string
	PasswordHash string
}

type Session struct {
	Token     string
	ExpiresAt time.Time
	CreatedAt time.Time
}
```

How `store` handles the consecutive-fail count: it lives as a column on `monitors` (`consecutive_fails`). `GetAlertState` reads that column plus the open incident in one go. `SetConsecutiveFails` updates the column. This keeps alert state derived from stored data per NFR (survives restart) without a separate table.

How `store` handles encryption: `store` calls into `crypto` when writing channel secret fields and `secret` headers, and decrypts on read. So `store` depends on `crypto`. The rest of the app always sees plaintext `Channel.Config` and `Header.Value` in memory; redaction for the API happens in `api`, not `store` (store returns full data, api strips secrets before serializing). This keeps "what's secret" knowledge in two clear places: `crypto`/`store` for at-rest, `api` for over-the-wire.

### 3.3 `checker` - execute one check

```go
package checker

type Config struct {
	BlockPrivateNetworks bool          // PULSE_BLOCK_PRIVATE_NETWORKS
	BodyCapBytes         int64         // 64 * 1024
	MaxErrorTextLen      int           // truncate ErrorText, e.g. 500
}

type Checker struct {
	cfg    Config
	client *http.Client // shared, with a custom Transport; no per-request redirect surprises
}

func New(cfg Config) *Checker

// Check runs the monitor's HTTP request, applies assertions in PRD 4.2 priority
// order, and returns a CheckResult with CheckedAt set to the request start time.
// ctx carries cancellation; Check also enforces monitor.TimeoutSeconds internally.
func (c *Checker) Check(ctx context.Context, m *domain.Monitor) *domain.CheckResult
```

Implementation contract for `Check` (so it matches the PRD exactly):

- **Timeout:** build a per-check context with `context.WithTimeout(ctx, time.Duration(m.TimeoutSeconds)*time.Second)`. Use it on the request. A context-deadline error becomes `ReasonTimeout`. The shared `http.Client` has no global `Timeout` set; we rely on the per-check context so the deadline is exact and includes body read.
- **Latency:** `start := time.Now()` right before `client.Do`, measure `time.Since(start)` when the response (and the capped body, if we read it) is in. That wall-clock value is `LatencyMs`. `CheckedAt = start` (UTC).
- **SSRF block:** if `cfg.BlockPrivateNetworks`, resolve the host first (see `ssrf.go`) and if any resolved IP is loopback, link-local, or RFC1918 private, return immediately with `ReasonBlockedTarget`, no request sent, `StatusCode`/`LatencyMs` nil. To avoid a TOCTOU gap between our resolve and the dialer's resolve, the `http.Client`'s `Transport.DialContext` wraps a dialer whose `Control` func re-checks the actual connected IP and refuses blocked ranges. So the guard is enforced at dial time, and the pre-resolve gives us the clean `blocked_target` reason without sending bytes.
- **Body cap:** only read the body when `m.BodyContains != nil`. Read with `io.LimitReader(resp.Body, cfg.BodyCapBytes)`. Substring match against what we read. If the body is bigger than the cap and the needle would only appear past it, the assertion fails (documented in UI help). When `BodyContains` is nil, drain a little and close so the connection can be reused, but do not keep the body.
- **Assertion priority (4.2):** `blocked_target` (handled before send) -> connection error / timeout -> status mismatch -> latency exceeded -> body assertion. First failing one is the recorded `FailureReason`. `StatusCode` and `LatencyMs` are filled whenever we have them, even on a failure, for context.
- **Status matching:** `statuscodes.go` parses `m.ExpectedStatusCodes` once into a matcher (explicit ints plus `2xx/3xx/4xx/5xx` ranges) and tests the response code.

`Check` returns a fully populated `*domain.CheckResult` but does not write it; the caller (worker) persists and feeds alerting. This keeps `checker` free of store and alerting imports, so it's trivially unit-testable with `httptest`.

```go
// statuscodes.go
type StatusMatcher interface{ Matches(code int) bool }
func ParseStatusCodes(spec string) (StatusMatcher, error) // also used by validation in api
```

### 3.4 `scheduler` - when checks run

```go
package scheduler

type Deps struct {
	Store    store.Store
	Checker  *checker.Checker
	Alerting *alerting.Engine
	Notify   *notify.Manager
	Workers  int           // pool size / max in-flight, e.g. 50
	Logger   *slog.Logger
}

type Scheduler struct { /* heap, control chan, locks, pool */ }

func New(d Deps) *Scheduler

// Start loads enabled monitors from the DB, seeds the heap (first run jittered
// across the interval to avoid a thundering herd), and runs the dispatch loop.
// Returns when ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context) error

// Live schedule changes from the api package. Non-blocking, sent over the control chan.
func (s *Scheduler) Upsert(m *domain.Monitor) // create or edit: (re)insert into heap or remove if disabled
func (s *Scheduler) Remove(monitorID int64)   // delete or disable: drop from heap, no-op if absent

// CheckNow runs an immediate check for a monitor, honoring the per-monitor lock.
// If a check is already in flight, returns ErrCheckInFlight (api maps to 409) along
// with the latest stored result so the handler can return it per 4.2.
func (s *Scheduler) CheckNow(ctx context.Context, monitorID int64) (*domain.CheckResult, error)

var ErrCheckInFlight = errors.New("check already in flight")
```

Design (full reasoning in section 5):

- **Min-heap of next-run times.** Each heap item is `{monitorID, nextRun time.Time, intervalSeconds}`. The dispatch loop sleeps until the earliest `nextRun` (via a single `time.Timer`), pops all due items, reschedules each as `nextRun = nextRun + interval` (cadence-stable, not now+interval, so a slow check doesn't drift the schedule), and dispatches them to the worker pool.
- **Bounded worker pool.** A buffered semaphore channel of size `Workers`. Dispatch acquires a slot (or, if full, the job waits briefly; we never spawn unbounded goroutines). This is the "bound on max in-flight" from the NFR. Outbound checks run concurrently up to `Workers`.
- **Per-monitor lock.** A `map[int64]*sync.Mutex` (or a `map[int64]bool` "in flight" guarded by one mutex) so only one check per monitor runs at a time. A scheduled tick for a monitor that's still running its previous check is skipped (no pile-up) and just waits for its next slot. `CheckNow` tries to take the same lock; if taken, returns `ErrCheckInFlight`.
- **Control channel for live changes.** `Upsert`/`Remove`/`CheckNow` push onto a channel the dispatch loop selects on, alongside the timer. So edits take effect on the next loop iteration without restarting anything. This avoids locking the heap from multiple goroutines: only the dispatch goroutine mutates the heap.
- **Startup rebuild.** `Start` calls `store.ListEnabledMonitors`, seeds the heap, and jitters the first run so 200 monitors created at the same second don't all fire together.

The worker body (pseudo) shows how scheduler ties checker + store + alerting + notify:

```go
func (s *Scheduler) runOne(ctx context.Context, m *domain.Monitor) {
	if !s.locks.tryLock(m.ID) { return } // still running, skip this tick
	defer s.locks.unlock(m.ID)

	res := s.deps.Checker.Check(ctx, m)
	id, _ := s.deps.Store.InsertResult(ctx, res)
	res.ID = id

	state, _ := s.deps.Store.GetAlertState(ctx, m.ID)
	dec := s.deps.Alerting.Apply(m, res, state)
	s.applyDecision(ctx, m, dec) // persists incident change + consecutive_fails, then notify
}
```

### 3.5 `alerting` - the state machine (PRD 12.5)

Pure decision function. No store, no notify imports. Takes the monitor, the new result, and current state; returns what changed. The caller persists and notifies. This makes the whole 12.5 table a table-driven unit test.

```go
package alerting

type Engine struct{}
func New() *Engine

type Action int
const (
	ActionNone        Action = iota
	ActionOpenIncident
	ActionCloseIncident
)

type Decision struct {
	Action            Action
	NewConsecutive    int             // value to persist for consecutive_fails
	// when ActionOpenIncident:
	IncidentStartedAt time.Time       // first failing check time in this run
	CauseReason       domain.FailureReason
	// when ActionCloseIncident:
	IncidentEndedAt   time.Time
	CloseReason       domain.CloseReason
	// notifications the caller should send (empty if monitor has no channels or no event)
	Notify            *NotifyEvent    // nil for ActionNone or when no event fires
}

type NotifyEventType string
const (
	EventDown     NotifyEventType = "down"
	EventRecovery NotifyEventType = "recovery"
)

type NotifyEvent struct {
	Type     NotifyEventType
	// everything notify needs to render, filled from monitor + result + incident
}

// Apply implements PRD 12.5 exactly.
//   - On a healthy result: reset consecutive to 0. If an incident is open, close it
//     (recovered) and emit a recovery event. Else ActionNone.
//   - On an unhealthy result: consecutive++. If no incident open and consecutive
//     reaches FailureThreshold, open an incident with StartedAt = the first failing
//     check time of this run, and emit a down event. If an incident is already open,
//     stay down, no re-notify (ActionNone for notification, but persist consecutive).
func (e *Engine) Apply(m *domain.Monitor, res *domain.CheckResult, state *domain.AlertState) Decision
```

The tricky bit: `started_at` is the FIRST failing check of the run, not the threshold-crossing check (12.5 step 4 vs 6). `Apply` does not query history. So we make `started_at` derivable from state: track `firstFailAt` alongside `consecutiveFails`. Decision: store the first-fail timestamp.

Architecture decision (recorded below in 3.5a): add a `first_fail_at` nullable column to `monitors`, set when consecutive goes 0 -> 1, cleared on any healthy check. Then `AlertState` carries `FirstFailAt *time.Time` and `Apply` uses it for `IncidentStartedAt`. This keeps `alerting` pure and avoids a history scan to find the run's first failure.

#### 3.5a Disable / edit interactions (not driven by a check)

`Apply` only handles check-driven transitions. Two transitions are NOT check-driven and are handled in the relevant api handler calling store directly, per 12.5:

- **Disable a down monitor:** `SetMonitorEnabled(false)` and, if an incident is open, `CloseIncident(endedAt = now, reason = disabled)`. No recovery notification. The scheduler `Remove`s it. This logic lives in the monitor-update/disable handler, documented so it's not lost.
- **Edit a down monitor:** incident stays open, `consecutive_fails` and `first_fail_at` left as-is. Next check runs new config and `Apply` drives it normally. The edit handler just updates fields and calls `scheduler.Upsert`.

### 3.6 `notify` - send notifications

```go
package notify

type Notifier interface {
	// Send delivers one rendered event to one channel. It returns nil on success.
	// Retry/backoff is handled by the Manager, not the Notifier, so each Notifier
	// is a single attempt and stays simple/testable.
	Send(ctx context.Context, ch *domain.Channel, ev Event) error
	// TestMessage sends a "this is a test from Pulse" message to validate config.
	TestMessage(ctx context.Context, ch *domain.Channel) error
}

type Event struct {
	EventType  string // "down" | "recovery"
	Monitor    domain.Monitor
	Incident   domain.Incident
	Check      domain.CheckResult
	DurationSeconds *int // set on recovery only
	SentAt     time.Time
}

type Manager struct {
	notifiers map[domain.ChannelType]Notifier
	client    *http.Client
	logger    *slog.Logger
	maxRetries int           // e.g. 3 attempts total
	backoff    func(attempt int) time.Duration
}

func NewManager(client *http.Client, logger *slog.Logger) *Manager

// Dispatch fans the event out to every attached channel concurrently. Per channel
// it retries with backoff up to maxRetries, then gives up and records the failure
// (logs + a visible marker the UI can surface). One channel failing does not block
// the others. PRD NFR: a missed alert is worse than a late one, but no full queue.
func (mgr *Manager) Dispatch(ctx context.Context, ev Event, channels []*domain.Channel)

// Test is the UI "send test message" entrypoint (api -> here).
func (mgr *Manager) Test(ctx context.Context, ch *domain.Channel) error
```

Implementations (one per file), payloads exactly per PRD 12.7:

- `slack.go` - POST `{"text": ...}` to the webhook URL. Render via `render.go`. Success = 2xx.
- `discord.go` - POST `{"content": ...}`. Success = 2xx (Discord returns 204).
- `webhook.go` - POST the fixed JSON envelope (12.7), `Content-Type: application/json`, plus the channel's custom headers. `duration_seconds` present only on recovery; `incident.ended_at` null on down. Success = 2xx.
- `smtp.go` - connect to host/port, STARTTLS or implicit TLS per channel config, auth with username/password, send subject `[Pulse] DOWN: <name>` / `[Pulse] RECOVERED: <name>` and a plain-text body. Use stdlib `net/smtp` plus `crypto/tls`.

`render.go` produces the human strings (Slack/Discord/email), all timestamps in UTC with the `UTC` suffix (12.7 closing note). The webhook envelope is rendered separately (RFC3339, machine format).

Retry/backoff: `Dispatch` runs each channel in its own goroutine; per channel it loops `maxRetries` attempts, sleeping `backoff(attempt)` (e.g. 1s, 4s, 9s, capped) between failures, respecting `ctx`. After the last failure it logs at error level and stores a "last notification failed" marker (a small column or a log the UI reads). v1 does not persist a retry queue.

### 3.7 `crypto` - secret encryption (PRD 12.6)

```go
package crypto

type Cipher struct{ aead cipher.AEAD }

// LoadKey decodes PULSE_SECRET_KEY (base64), checks it is exactly 32 bytes, and
// returns a Cipher. On any problem it returns an error; main exits non-zero
// (fail closed, never plaintext fallback). Called once at startup.
func LoadKey(b64 string) (*Cipher, error)

// Encrypt returns base64(nonce || ciphertext || gcmTag). Fresh random 96-bit nonce per call.
func (c *Cipher) Encrypt(plaintext string) (string, error)

// Decrypt reverses Encrypt. Used by store when reading secret columns.
func (c *Cipher) Decrypt(encoded string) (string, error)
```

`store` holds a `*crypto.Cipher` and uses it for channel secret fields and `secret` headers. `crypto` imports nothing from the rest of the app.

### 3.8 `auth` - login, sessions, middleware (PRD 11.2)

```go
package auth

type Service struct {
	store       store.Store
	sessionTTL  time.Duration // e.g. 7 days
	cookieName  string        // "pulse_session"
	secureCookie bool         // true unless explicitly dev
	basePath    string        // for cookie Path
}

func New(store store.Store, opts Options) *Service

// SeedAdmin upserts the admin row from env (PRD: username change -> upsert,
// password change -> re-hash). Called at startup after migrations.
func (s *Service) SeedAdmin(ctx context.Context, username, password string) error

func HashPassword(plain string) (string, error) // bcrypt (golang.org/x/crypto/bcrypt)
func VerifyPassword(hash, plain string) bool

// Login verifies credentials, creates a session, returns the cookie to set.
func (s *Service) Login(ctx context.Context, username, password string) (*http.Cookie, error)
func (s *Service) Logout(ctx context.Context, token string) (*http.Cookie, error) // expired cookie
func (s *Service) Current(ctx context.Context, r *http.Request) (*store.Session, bool)

// Middleware protects everything except login + /healthz. On no/invalid session
// it writes the 401 error shape (12.3). It does not redirect; the SPA handles
// redirect-to-login on 401.
func (s *Service) Middleware(next http.Handler) http.Handler
```

Decision on the hash: bcrypt via `golang.org/x/crypto/bcrypt`. It's the one non-stdlib crypto dependency, battle-tested, simpler than wiring argon2 params. The PRD allows bcrypt or argon2.

Session tokens: 32 random bytes from `crypto/rand`, base64url, stored in the `sessions` table with an expiry. Cookie is httpOnly, Secure, SameSite=Lax (Lax not Strict so a top-level navigation to the app after following a link still carries the cookie; the API is same-origin so CSRF surface is low, and all mutations are JSON POST/PUT/DELETE which SameSite=Lax already blocks cross-site).

### 3.9 `api` - HTTP wiring

Router uses the Go 1.22 `ServeMux` method+path patterns. One handler struct holds dependencies; methods are the handlers.

```go
package api

type Server struct {
	store     store.Store
	scheduler *scheduler.Scheduler
	notify    *notify.Manager
	auth      *auth.Service
	checker   *checker.Checker
	logger    *slog.Logger
	assets    fs.FS // embedded SPA
	basePath  string
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// unauthenticated
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)

	// authenticated API (wrapped by auth middleware below)
	api := http.NewServeMux()
	api.HandleFunc("POST /api/auth/logout", s.handleLogout)
	api.HandleFunc("GET /api/auth/me", s.handleMe)

	api.HandleFunc("GET /api/monitors", s.handleListMonitors)
	api.HandleFunc("POST /api/monitors", s.handleCreateMonitor)
	api.HandleFunc("GET /api/monitors/{id}", s.handleGetMonitor)
	api.HandleFunc("PUT /api/monitors/{id}", s.handleUpdateMonitor)
	api.HandleFunc("DELETE /api/monitors/{id}", s.handleDeleteMonitor)
	api.HandleFunc("POST /api/monitors/{id}/check", s.handleCheckNow)
	api.HandleFunc("GET /api/monitors/{id}/results", s.handleListResults)
	api.HandleFunc("GET /api/monitors/{id}/incidents", s.handleMonitorIncidents)

	api.HandleFunc("GET /api/channels", s.handleListChannels)
	api.HandleFunc("POST /api/channels", s.handleCreateChannel)
	api.HandleFunc("PUT /api/channels/{id}", s.handleUpdateChannel)
	api.HandleFunc("DELETE /api/channels/{id}", s.handleDeleteChannel)
	api.HandleFunc("POST /api/channels/{id}/test", s.handleTestChannel)

	api.HandleFunc("GET /api/incidents", s.handleListIncidents)

	mux.Handle("/api/", s.auth.Middleware(api))

	// SPA + static assets: everything else
	mux.Handle("/", s.staticHandler())

	// outer middleware chain
	return s.recover(s.logRequests(withBasePath(s.basePath, mux)))
}
```

Middleware chain order (outer to inner): panic recovery -> request logging -> base-path strip -> route mux -> (for `/api/*`) auth. The JSON error writer (`errors.go`) produces the 12.3 shape for every non-2xx.

Static serving (`static.go`): serve files from the embedded `fs.FS`. For any path that isn't a real asset and isn't `/api/` or `/healthz`, return `index.html` so the client router handles deep links (SPA fallback). Set long cache headers on hashed asset filenames, no-cache on `index.html`.

Redaction lives here (12.3): handlers load full data from `store` (decrypted in memory) and strip secrets before serializing. Channels return `"<field>_set": true/false` for each secret field, never the value. Monitor headers with `secret: true` return key + `"secret": true`, value omitted. On write, an omitted secret field leaves the stored value unchanged; an explicit empty string clears it. The handler reads the existing channel/monitor to know whether a secret was omitted vs cleared.

Validation (12.4) lives in `api` (request DTO -> validate -> domain struct). `ParseStatusCodes` from `checker` is reused to validate `expected_status_codes`. Validation failures return the 12.3 error shape with `fields`.

Wiring on create/edit/delete/enable: the handler updates the store, then calls `scheduler.Upsert` or `scheduler.Remove` so the live schedule tracks the change. Delete cascades in the store (FK `ON DELETE CASCADE`).

---

## 4. Database schema

SQLite, one file. DDL below is the initial migration `0001_init.sql`. Times stored as TEXT in RFC3339 UTC (`strftime`-comparable, human-readable, sorts lexicographically which matches chronological for fixed-format UTC). Booleans as INTEGER 0/1. Encrypted columns hold base64(nonce||ciphertext||tag).

```sql
PRAGMA foreign_keys = ON;

CREATE TABLE monitors (
  id                    INTEGER PRIMARY KEY AUTOINCREMENT,
  name                  TEXT    NOT NULL,
  url                   TEXT    NOT NULL,
  method                TEXT    NOT NULL DEFAULT 'GET',
  body                  TEXT    NOT NULL DEFAULT '',
  expected_status_codes TEXT    NOT NULL DEFAULT '200',  -- raw spec, e.g. "200,204" or "2xx"
  timeout_seconds       INTEGER NOT NULL DEFAULT 10,
  interval_seconds      INTEGER NOT NULL DEFAULT 60,
  enabled               INTEGER NOT NULL DEFAULT 1,
  max_latency_ms        INTEGER,                          -- NULL = no assertion
  body_contains         TEXT,                             -- NULL = no assertion
  failure_threshold     INTEGER NOT NULL DEFAULT 1,
  consecutive_fails     INTEGER NOT NULL DEFAULT 0,       -- alert state, survives restart
  first_fail_at         TEXT,                             -- first fail of current run, NULL when count=0
  created_at            TEXT    NOT NULL,
  updated_at            TEXT    NOT NULL
);

-- headers as their own rows so a header can be flagged secret and encrypted per value
CREATE TABLE monitor_headers (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  monitor_id INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  key        TEXT    NOT NULL,
  value      TEXT    NOT NULL,   -- encrypted when is_secret = 1, plaintext otherwise
  is_secret  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_headers_monitor ON monitor_headers(monitor_id);

CREATE TABLE channels (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  name    TEXT    NOT NULL,
  type    TEXT    NOT NULL,      -- slack|discord|webhook|smtp
  config  TEXT    NOT NULL,      -- JSON; secret fields inside are individually encrypted
  enabled INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE monitor_channels (
  monitor_id INTEGER NOT NULL REFERENCES monitors(id)  ON DELETE CASCADE,
  channel_id INTEGER NOT NULL REFERENCES channels(id)  ON DELETE CASCADE,
  PRIMARY KEY (monitor_id, channel_id)
);
CREATE INDEX idx_mc_channel ON monitor_channels(channel_id);

CREATE TABLE check_results (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  monitor_id     INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  checked_at     TEXT    NOT NULL,
  healthy        INTEGER NOT NULL,
  failure_reason TEXT,            -- NULL when healthy
  status_code    INTEGER,         -- NULL on conn error/timeout/blocked
  latency_ms     INTEGER,         -- NULL when no latency
  error_text     TEXT             -- short, truncated
);
-- the hot index: history view (monitor + newest-first) and retention scan
CREATE INDEX idx_results_monitor_time ON check_results(monitor_id, checked_at DESC);
CREATE INDEX idx_results_checked_at   ON check_results(checked_at); -- retention delete

CREATE TABLE incidents (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  monitor_id      INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  started_at      TEXT    NOT NULL,           -- first failing check of the run
  ended_at        TEXT,                       -- NULL while open
  cause_reason    TEXT    NOT NULL,
  close_reason    TEXT,                       -- recovered|disabled, NULL while open
  first_result_id INTEGER REFERENCES check_results(id) ON DELETE SET NULL
);
CREATE INDEX idx_incidents_monitor_time ON incidents(monitor_id, started_at DESC);
-- enforce at most one open incident per monitor
CREATE UNIQUE INDEX uniq_open_incident ON incidents(monitor_id) WHERE ended_at IS NULL;
-- global incidents list, open-first
CREATE INDEX idx_incidents_open ON incidents(ended_at, started_at DESC);

CREATE TABLE admin (
  id            INTEGER PRIMARY KEY CHECK (id = 1), -- single row
  username      TEXT NOT NULL,
  password_hash TEXT NOT NULL
);

CREATE TABLE sessions (
  token      TEXT    PRIMARY KEY,
  expires_at TEXT    NOT NULL,
  created_at TEXT    NOT NULL
);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);
```

Encrypted columns: `monitor_headers.value` (when `is_secret = 1`), and the secret keys inside `channels.config` JSON (Slack/Discord/webhook URL, SMTP password). Everything else is plaintext.

The `consecutive_fails` and `first_fail_at` columns on `monitors` hold the alerting state so it survives a restart (NFR: in-flight incident state derived from stored data). The open incident is found via `incidents WHERE monitor_id=? AND ended_at IS NULL`, which the partial unique index keeps to one row.

SQLite settings applied on open (in `store`): `PRAGMA journal_mode=WAL` (concurrent reads while a check writes), `PRAGMA busy_timeout=5000` (wait instead of `SQLITE_BUSY` under contention), `PRAGMA foreign_keys=ON`, `PRAGMA synchronous=NORMAL` (good durability/throughput balance with WAL). Writes go through a single `*sql.DB`; with the pure-Go driver we set `SetMaxOpenConns(1)` for writes safety or rely on WAL + busy_timeout. Decision: keep one writer by funneling all writes through the store with `busy_timeout`, which is simplest and fine at our scale.

### Migration mechanism

Minimal embedded SQL migrations, no `golang-migrate` dependency. `internal/store/migrations/*.sql` is embedded with `//go:embed`. A `schema_migrations(version INTEGER PRIMARY KEY)` table tracks applied versions. On startup `store.Migrate` reads the embedded files in lexical order, and for each version not yet recorded, runs the file in a transaction and inserts the version. Forward-only (no down migrations) which is plenty for a single-binary self-host app. This is ~50 lines and keeps the single-binary, pure-Go, zero-extra-dep goal.

Architecture decision: chose a hand-rolled embedded runner over `golang-migrate` because golang-migrate pulls a CLI-oriented dependency tree and we only need forward apply of embedded files. If we ever need down-migrations or out-of-band tooling we can switch; the contract (`store.Migrate(ctx)`) stays the same.

---

## 5. Concurrency & scheduling design

### Chosen approach: single min-heap dispatcher + bounded worker pool

One scheduler goroutine owns a min-heap keyed by `nextRun`. It sleeps on a `time.Timer` set to the earliest due time, wakes, pops all due monitors, reschedules each (`nextRun += interval`), and sends each as a job to a worker pool bounded by a semaphore of size `Workers` (default ~50). Per-monitor locks stop two checks of the same monitor overlapping.

### Why this over the alternatives

Two real options for ~200 monitors at a 30s floor on a 1-2 vCPU box:

- **Option A: one ticker goroutine per monitor.** Simple to write (`time.NewTicker(interval)` per monitor, each fires into the worker pool). Precedent: common in small Go schedulers. Downsides at our scale: 200 goroutines parked on timers is fine memory-wise, but every create/edit/disable has to find and stop the right ticker, and you still need a separate bound on in-flight checks anyway. Live edits mean tracking a ticker per monitor in a map and stopping/replacing it. It works but the bookkeeping for live changes and the "don't pile up" rule is messier.

- **Option B (chosen): single min-heap dispatcher + worker pool.** One goroutine mutates the heap, so no locking on the schedule itself. Live changes arrive on a control channel the same goroutine selects on, so create/edit/enable/disable/delete are just heap insert/remove with no race. The worker-pool semaphore is the single, obvious place that bounds max in-flight (the NFR's "bound on max in-flight requests"). Cadence stays stable because we reschedule `nextRun += interval`, not `now + interval`, so a slow check can't drift a monitor's schedule. "No pile-up" falls out of the per-monitor lock: if a monitor's prior check is still running when its tick comes, we skip that tick.

Option B wins on: one clear in-flight bound, clean live edits via a channel, no per-monitor goroutine sprawl, stable cadence. The heap is `container/heap` from the stdlib, so no dependency. At 200 monitors the heap ops are trivial.

### How live schedule changes are picked up

The api handlers call `scheduler.Upsert(monitor)` / `scheduler.Remove(id)` after a store write. These push a message onto the scheduler's control channel. The dispatch loop's `select` has three arms: the timer firing (run due monitors), a control message (mutate the heap), and `ctx.Done()` (shutdown). Because only the dispatch goroutine touches the heap, there's no lock on it. A disabled or deleted monitor is removed from the heap; an edited monitor's interval change updates its `nextRun`. `CheckNow` also goes through the control path (or directly grabs the per-monitor lock and runs once via the pool) and does not change the monitor's scheduled `nextRun`, per 4.2.

### Bounding and back-pressure

`Workers` (default 50, configurable via env if needed) caps concurrent outbound checks. If a burst of monitors comes due and all workers are busy, dispatch blocks on acquiring a slot, which naturally spaces the work without dropping checks. At 200 monitors / 30s, average is well under 10 checks/sec, so 50 workers is generous headroom even with some slow endpoints.

### Retention job

A separate ticker goroutine (hourly) calls `store.DeleteResultsBefore(now - retention)`. It runs independent of the scheduler. The delete uses `idx_results_checked_at`. Hourly is fine; the PRD says "periodically (e.g. hourly)".

### Shutdown

`main` cancels the root context on SIGTERM/SIGINT. The HTTP server does graceful `Shutdown`. The scheduler stops dispatching new work and waits for in-flight workers (a `sync.WaitGroup`) up to a deadline. In-flight checks either finish and persist, or are cut by their own timeout; either way state is consistent because each worker persists atomically.

---

## 6. Config / env vars

Loaded by `internal/config`. Required vars fail closed (log a clear error, exit non-zero) before serving any request. Defaults shown.

| Env var | Required | Default | Meaning |
|---|---|---|---|
| `PULSE_SECRET_KEY` | yes | none | base64 of exactly 32 bytes; AES-256-GCM key. Invalid/missing -> exit (12.6). |
| `PULSE_ADMIN_USER` | yes | none | admin username, seeded at startup (11.2). |
| `PULSE_ADMIN_PASSWORD` | yes | none | admin password, hashed at startup, never stored plaintext (11.2). |
| `PULSE_DB_PATH` | no | `./pulse.db` | SQLite file path. |
| `PULSE_LISTEN_ADDR` | no | `:8080` | HTTP listen address. |
| `PULSE_BASE_PATH` | no | `` (root) | URL base path when behind a reverse proxy at a sub-path, e.g. `/pulse`. |
| `PULSE_BLOCK_PRIVATE_NETWORKS` | no | `false` | SSRF block flag (11.6). When true, checks to loopback/link-local/RFC1918 fail with `blocked_target`. |
| `PULSE_RETENTION_DAYS` | no | `30` | days of `check_results` to keep before cleanup (9.6). |
| `PULSE_WORKERS` | no | `50` | max concurrent outbound checks (worker pool / in-flight bound). |
| `PULSE_SESSION_TTL_HOURS` | no | `168` (7d) | session cookie / row lifetime. |
| `PULSE_SECURE_COOKIE` | no | `true` | set the cookie Secure flag. Allow `false` for local non-TLS dev. |
| `PULSE_LOG_LEVEL` | no | `info` | slog level. |

Startup order in `main`: load config -> if any required missing, exit. Load crypto key (`crypto.LoadKey`) -> invalid, exit. Open store + migrate. Seed admin from env. Build checker, notify, alerting, scheduler. Start scheduler goroutine + retention ticker. Start HTTP server. This ordering guarantees we never serve or store anything before the fail-closed checks pass.

---

## 7. Frontend architecture

Lit web components + Vite + TypeScript. Built to static assets, embedded in the Go binary, served by `api`. The SPA talks only to `/api/*`.

### Component tree

```
<app-root>                       app shell, owns router outlet + session check
  <app-nav>                      top nav: Monitors / Channels / Incidents / logout
  (router outlet)
    <login-view>                 username/password form -> POST /api/auth/login
    <monitors-list-view>         GET /api/monitors; rows with <status-badge>, <sparkline>, enable toggle
    <monitor-detail-view>        GET /api/monitors/{id} + results + incidents; <latency-chart>
    <monitor-form-view>          create/edit; all 4.1 fields, channel multi-select, client validation
    <channels-view>              list channels, create/edit via <channel-form>, "send test" button
    <channel-form>               type-specific config fields, secret fields write-only
    <incidents-view>             GET /api/incidents; global list, filter open/all
  <confirm-dialog>               reusable delete confirm
```

Shared small components: `<status-badge>` (up/down/disabled/pending pill), `<latency-chart>` (custom SVG), `<sparkline>` (inline mini up/down strip).

### Routing

Recommendation: `@lit-labs/router` (the official Lit URL router, small, integrates with Lit's reactive lifecycle) or, if we want zero extra deps, a ~60-line hand-rolled router on the History API that maps `location.pathname` to a component and re-renders the outlet on `popstate` and link clicks. Decision: hand-rolled tiny router. The route set is small and fixed (7 views), the SPA fallback is already handled server-side, and avoiding a dep keeps the bundle tiny. It exposes `navigate(path)` and a `<route-outlet>` that renders the matched view. Routes:

```
/                       -> monitors-list-view
/monitors/new           -> monitor-form-view (create)
/monitors/:id           -> monitor-detail-view
/monitors/:id/edit      -> monitor-form-view (edit)
/channels               -> channels-view
/incidents              -> incidents-view
/login                  -> login-view
```

Routes honor `PULSE_BASE_PATH`: the router reads a `<base href>` injected into `index.html` (or a build-time constant) so deep links work behind a sub-path proxy.

### API client pattern

`src/api/client.ts` is a thin `fetch` wrapper:

- always `credentials: 'include'` so the session cookie rides along.
- sets `Content-Type: application/json` for bodies, parses JSON responses.
- on `401`, clears session state and routes to `/login` (the central place that handles auth expiry). On other non-2xx, throws an error carrying the 12.3 `error` object so views can show `message` / per-field `fields`.
- typed methods: `listMonitors()`, `getMonitor(id)`, `createMonitor(dto)`, etc., returning the `types.ts` shapes that mirror the API.

### Auth / session handling

Cookie-based, set by the server (httpOnly so JS can't read it). On app load, `<app-root>` calls `GET /api/auth/me`. If 200, render the app; if 401, route to login. After login, re-fetch `me` and go to `/`. Logout calls `POST /api/auth/logout` and routes to login. Because the cookie is httpOnly, the SPA never stores a token; "am I logged in" is just whether `me` succeeds. The 401 interceptor in the client covers session expiry mid-session.

### State management

No store library. Keep it simple:

- Each view component fetches its own data in `connectedCallback` / on route param change and holds it in reactive `@state()` properties. Lit re-renders on change.
- A single tiny `session.ts` reactive holder for current-user info shared by `app-root`/`app-nav` (a small event-emitter or a Lit `@property`-driven context). No Redux/MobX/etc.
- Mutations re-fetch the affected list rather than trying to patch local state cleverly. At this data size that's fine and avoids cache-invalidation bugs.

### Latency chart

Recommendation: a tiny custom SVG line chart component (`<latency-chart>`), no charting library. Input is the results array (`checked_at`, `latency_ms`); it computes min/max, maps points to an SVG `<polyline>`, draws a couple of axis labels, and colors unhealthy points red. This is ~100-150 lines, has zero dependency weight, renders crisply, and is plenty for "latency over time for one monitor". Avoids pulling Chart.js/uPlot/d3. The list-view `<sparkline>` is an even smaller version showing the last N checks as up/down bars.

Decision: custom SVG over uPlot/Chart.js. The PRD explicitly wants a lightweight option and one chart type. A library is more bytes and API surface than the single view needs.

### Vite build -> Go embed flow

Vite builds `web/` to `web/dist` (hashed asset filenames + `index.html`). The Makefile copies `web/dist` into `internal/web/dist` (the embed root), and `internal/web/embed.go` does `//go:embed all:dist` exposing an `fs.FS`. `api.static` serves from it with SPA fallback. So `go build ./cmd/pulse` produces one binary containing the API and the built UI. During frontend dev, run Vite's dev server with a proxy to the Go server's `/api` so you get HMR without rebuilding the binary.

---

## 8. Testing strategy

### Unit tests (the bulk)

- **`checker`** with `httptest.Server`: a server that returns various status codes, slow responses (to test timeout + `max_latency_ms`), bodies (to test `body_contains` and the 64 KB cap), and connection drops. Assert the right `FailureReason` and priority order. Test latency is measured and `CheckedAt` set. Test SSRF: with the block flag on, a URL resolving to `127.0.0.1` returns `blocked_target` and sends nothing (assert the test server got no request). Status-code matcher gets its own table test (lists + `2xx`/etc.).
- **`alerting`** table-driven test that encodes PRD 12.5 step by step (the H/F sequence, `T=3` and `T=1`), asserting `Action`, `NewConsecutive`, incident open/close, `IncidentStartedAt = first fail`, and which `NotifyEvent` fires. This table IS the acceptance test for the state machine. Add the disable-while-down and edit-while-down cases (driven by the handler logic, tested where that logic lives).
- **`crypto`** round-trip: encrypt then decrypt returns the original; two encrypts of the same plaintext differ (fresh nonce); a tampered ciphertext fails to decrypt; `LoadKey` rejects missing / wrong-length / non-base64 keys.
- **`notify`** against a fake HTTP server (`httptest`): assert Slack `text`, Discord `content`, and the generic webhook envelope match 12.7 exactly (field names, `duration_seconds` only on recovery, `ended_at` null on down, custom headers sent). Test retry/backoff by having the fake server fail twice then succeed, and assert it gave up after `maxRetries` and recorded the failure. SMTP can be tested against a tiny in-process SMTP capture (or an interface seam mocked); assert subject/body and TLS option handling.
- **`auth`**: hash/verify round-trip, middleware returns 401 (with the 12.3 shape) when no/invalid session and passes through with a valid one. `SeedAdmin` upsert behavior (username change, password re-hash).
- **`api` validation**: each 12.4 rule produces the right field error (bad URL scheme, interval < 30, interval < timeout, body on GET, empty name, unknown channel id, etc.). Redaction: a created channel's GET never returns the secret, returns `*_set` booleans; omitting a secret on update keeps it, empty string clears it.

### Integration tests

- **`store`** against a real SQLite file (temp dir, `t.TempDir()`), running migrations: CRUD for each entity, cascade delete (delete a monitor wipes its results/incidents/headers/join rows), the partial unique index rejects a second open incident, results pagination with `range` + cursor returns newest-first and a working `next_cursor`, retention delete removes only old rows. Because the pure-Go driver needs no cgo, these run anywhere.
- **End-to-end loop** (the acceptance criteria in section 8 of the PRD): spin up the full app against a temp DB with a fake "monitored" `httptest` server and a fake notification sink. Create a monitor pointing at the fake server, flip it to failing, advance through checks (call `CheckNow` to avoid waiting real intervals), assert an incident opens and one down notification lands; flip back to healthy, assert one recovery notification and the incident closes with correct duration. Test `failure_threshold=3` needs three fails. This validates the wiring across scheduler/checker/alerting/notify/store together.

### What we don't test heavily in v1

- Frontend: a few component smoke tests (does the form validate, does the list render statuses) with `@open-wc/testing` if time allows, but the API contract tests carry most of the weight. No heavy e2e browser suite in v1.

The seam design (pure `checker`, pure `alerting`, `Notifier` interface, `Store` interface) is what makes most of this fast unit tests instead of slow integration tests.

---

## 9. Build & run

### SQLite driver choice

Use **`modernc.org/sqlite`** (pure Go), not `mattn/go-sqlite3` (cgo). Reasons: pure Go means `CGO_ENABLED=0` builds and trivial cross-compilation to a single static binary for whatever VM/container the operator runs, which is the whole point of "single self-hosted binary". No C toolchain in the build, no glibc/musl surprises in containers. The throughput difference vs the cgo driver is irrelevant at ~200 monitors. `mattn/go-sqlite3` is faster and more battle-tested but its cgo requirement fights the single-static-binary goal. So pure-Go wins here.

### Dependencies (kept tiny)

- `modernc.org/sqlite` - storage driver.
- `golang.org/x/crypto/bcrypt` - password hashing.
- Optionally `@lit-labs/router` on the frontend, but the plan is hand-rolled (zero).
- Everything else is stdlib: `net/http`, `database/sql`, `crypto/aes`+`crypto/cipher`, `crypto/rand`, `container/heap`, `embed`, `log/slog`, `net/smtp`, `crypto/tls`.

### Build steps

```
# 1. build the frontend
cd web && npm ci && npm run build        # -> web/dist

# 2. stage it for embed (Makefile does this)
rm -rf internal/web/dist && cp -r web/dist internal/web/dist

# 3. build the single binary (pure Go, static)
CGO_ENABLED=0 go build -o pulse ./cmd/pulse
```

A `Makefile` target `make build` chains these. `make dev` runs Vite dev server + `go run ./cmd/pulse` with the Vite proxy pointed at the Go `/api`.

### Run locally

```
export PULSE_SECRET_KEY=$(head -c 32 /dev/urandom | base64)
export PULSE_ADMIN_USER=admin
export PULSE_ADMIN_PASSWORD=changeme
export PULSE_SECURE_COOKIE=false       # local http, no TLS
./pulse
# open http://localhost:8080, log in with admin / changeme
```

For production, the operator puts TLS in front (reverse proxy), keeps `PULSE_SECURE_COOKIE=true`, sets a real `PULSE_SECRET_KEY` they store safely (losing it loses the secrets), and sets `PULSE_BASE_PATH` if serving under a sub-path.

---

## Architecture decisions (PRD ambiguities resolved)

These are calls made where the PRD left room. Reasoning included.

1. **First-fail timestamp storage.** 12.5 needs `started_at` = first failing check of the run, but `alerting.Apply` is a pure function with no history access. Decision: add `first_fail_at` (and keep `consecutive_fails`) as columns on `monitors`. Set `first_fail_at` when the count goes 0 -> 1, clear it on any healthy check, read it into `AlertState`. Keeps the state machine pure and avoids a history scan to find the run's first failure.

2. **Headers as rows, not JSON.** The PRD says "monitor_headers or headers-as-JSON". Decision: a `monitor_headers` table. Because a single header can be `secret` (encrypted) while siblings aren't, per-row storage makes per-value encryption and the `secret` flag clean. JSON-in-a-column would force encrypting the whole blob or hand-rolling per-field handling inside JSON.

3. **Channel config as JSON with per-field encryption.** Channels have heterogeneous config (Slack URL vs SMTP host/port/user/pass). Decision: store `config` as a JSON blob, but encrypt the specific secret keys inside it before serializing, and decrypt on read in `store`. Avoids a column-per-type schema. The set of secret keys per type is a small known map.

4. **Consecutive-fail count on `monitors`, no separate state table.** Alert state is tiny (one int + one timestamp per monitor) and 1:1 with a monitor, so columns beat a table. Survives restart, read in one query with the open incident.

5. **At-most-one-open-incident enforced in the DB.** A partial unique index (`WHERE ended_at IS NULL`) makes "one open incident per monitor" a hard invariant rather than relying on app logic. Cheap insurance.

6. **Session storage in SQLite, not in-memory.** So logins survive a restart (operator restarts the box, you stay logged in) and it's one less special case. Expired-session cleanup is a tiny periodic delete (folded into the retention ticker or a `DeleteExpiredSessions` call on login).

7. **SameSite=Lax cookie.** Strict would drop the cookie on first navigation from an external link; Lax keeps it for top-level GET navigations while still blocking cross-site state-changing requests. The API is same-origin and all mutations are non-GET, so CSRF surface stays low without adding a token scheme in v1.

8. **Redaction in `api`, encryption in `store`/`crypto`.** Two distinct concerns: over-the-wire (never send secrets to the browser) lives in handlers; at-rest (AES-GCM) lives in store/crypto. The rest of the app sees plaintext in memory. Clear separation, no leaking secret-ness knowledge across every layer.

9. **Hand-rolled migration runner over golang-migrate.** Forward-only embedded SQL with a `schema_migrations` table is ~50 lines and keeps the dependency tree and single-binary story clean. The `store.Migrate(ctx)` contract is stable if we ever swap it.

10. **`CheckNow` returns the latest result on 409.** The PRD allows "409 with a short message, or return the latest result". Decision: return 409 with the 12.3 conflict error shape AND include the latest stored result in the body so the UI can still show something useful. Best of both.
