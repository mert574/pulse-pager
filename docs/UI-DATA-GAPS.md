# UI data gaps (design overhaul)

Living list of data points the redesigned web UI wants to show but the API does not
provide yet. Filled in as each view is migrated to the Swiss design. The rule during
the overhaul: never fabricate a number. If the data is not in the contract, the UI
either drops that element or shows an honest derived value, and the gap is logged here.

How to close a gap: it is a contract-first change (RFC-012). Add the field/endpoint to
`api/openapi/v1.yaml`, run `make gen`, then wire the handler in `internal/api` and the
store query in `internal/store`. Do not hand-edit the generated files.

Status legend: `OPEN` not started, `SPEC` added to v1.yaml, `DONE` served by the API.

---

## Monitors list (`monitors-list-view`)

The flagship screen. `GET /orgs/{orgId}/monitors` returns `MonitorListItem`
(`api/openapi/v1.yaml`), which today carries: id, type, name, url, enabled, status,
last_check_at, next_check_at, interval_seconds, last_latency_ms, incident_open,
cert_expires_at. What the design wants on top of that:

- **OPEN — per-monitor uptime %.** The mockup row shows an uptime figure per monitor
  (e.g. 99.98%). `MonitorListItem` has no uptime field. The status page already has
  `PublicUptime` (uptime_24h / uptime_7d / uptime_90d) for the public projection; the
  org-side list needs an equivalent. Proposal: add `uptime_24h` (number, nullable) to
  `MonitorListItem`, computed from check results over the last 24h. The row currently
  omits the uptime column.
- **OPEN — per-monitor latency sparkline.** The mockup draws a small recent-latency
  trend per row. No latency history is in the list payload. Proposal: a compact series
  (say the last N latency samples) either inlined on `MonitorListItem` as
  `latency_spark: number[]` or via a separate lightweight endpoint
  `GET /orgs/{orgId}/monitors/{id}/latency?window=24h`. The row currently omits the
  sparkline.
- **OPEN — fleet uptime over 30 days.** The hero's headline number in the mockup. No
  org-level aggregate endpoint exists. Proposal: `GET /orgs/{orgId}/summary` returning
  fleet uptime windows (24h/7d/30d), counts by status, and checks/min. The hero
  currently shows honest live counts (total, up/down/degraded, open incidents,
  ssl<=14d) derived client-side from the list, not a 30-day uptime.
- **OPEN — fleet latency series (last 24h).** The hero's line chart in the mockup.
  Needs the same `summary` endpoint (or a dedicated metrics one) to return a fleet
  latency series. Currently not rendered.
- **OPEN — checks per minute.** A mockup mini-stat. Derivable server-side from the
  effective intervals; belongs on the proposed `summary` endpoint. Currently not shown.

### Folio (shell marquee, `app-root`)

The dateline marquee shows honest facts only (product, active org name, brand lines).
The mockup version also listed region count, total monitors, a build hash, fleet
uptime, and active-incident count. Region count and monitor count would come from the
proposed `summary` endpoint; a build hash would need a build-info value exposed to the
SPA. Left out of the folio for now rather than faked.

---

## Monitor detail (`http-monitor-detail`, `ssl-monitor-detail`)

The redesign leads with a dominant uptime numeral and a wide latency panel.
`GET /orgs/{orgId}/monitors/{id}` plus the recent check results feed it. What the
design wants beyond the loaded 24h results window:

- **OPEN.** windowed uptime (7d / 30d). The big numeral is computed client-side from
  the loaded 24h results, so it is labeled "Uptime · 24h" to stay honest about the
  window. A real 7d/30d figure needs the server to aggregate over a longer range than
  the results page carries. Proposal: reuse the `summary`-style aggregate (or add
  `uptime_7d` / `uptime_30d` to the monitor detail payload). The numeral shows "—" when
  there are no results.
- **OPEN.** server-side p95 latency. The spec strip shows avg and p95 derived
  client-side from the 24h results sample, which drifts from a true percentile over a
  longer window. Proposal: a server-computed `p95_latency_ms` (windowed) on the detail
  payload. Falls back to the client estimate today.

SSL days-left is derived from the existing `cert.not_after`, no gap.

## Admin overview (`admin-view`)

The control-room gauge cluster reads org/user/monitor/channel counts, activation rate,
active-7d, and open incidents from the existing admin metrics. The **open-incidents**
gauge sums `incidents_open` across the returned org rows; that field is already declared
on the admin org row type but is not always populated, so the gauge shows "—" when no
row carries it. Closing it means having the admin orgs query always return
`incidents_open`.

<!-- Append a section per view as it is migrated. -->
