import { html, type TemplateResult } from "lit";
import { customElement, state } from "lit/decorators.js";
import { client } from "../api/client.js";
import { t, tDynamic } from "../i18n.js";
import {
  formatDateTime,
  formatDuration,
  formatLatency,
  formatStatusCode,
  secondsUntil,
} from "../format.js";
import type {
  CheckNowAccepted,
  CheckResult,
  CoverageStatus,
  FailureSnapshot,
  RegionState,
} from "../api/types.js";
import { fieldHelp } from "../icons.js";
import { MonitorDetailBase, FAILURE_LABEL } from "./monitor-detail-base.js";
import type { LatencyPoint } from "./latency-chart.js";
import type { DataColumn } from "./data-table.js";
import type { UptimeBar } from "./uptime-bar.js";

import "./region-chips.js";
import "./latency-chart.js";
import "./data-table.js";
import "./uptime-bar.js";

// How often the live region-state poll runs while the tab is visible.
const POLL_MS = 5000;

function percentile(values: number[], p: number): number | null {
  if (values.length === 0) return null;
  const sorted = [...values].sort((a, b) => a - b);
  const idx = Math.min(sorted.length - 1, Math.ceil((p / 100) * sorted.length) - 1);
  return sorted[Math.max(0, idx)];
}

// One scheduler tick, with every region that ran it folded in. scheduledAt is the
// tick key shared across a run's regions (checked_at is the real run time and
// differs per region, so it cannot group them). The Recent checks table shows one
// row per run; a multi-region run expands to its per-region detail.
interface CheckRun {
  scheduledAt: string;
  regions: CheckResult[]; // sorted by region name
  healthy: boolean; // a run is healthy only if every region passed
  latencyMin: number | null;
  latencyMax: number | null;
}

// The http monitor detail (RFC-013 section 7.2): derived stats (uptime, avg and
// p95 latency, last/next check), live per-region chips, the uptime + latency
// charts, the recent-checks table, the last-failure snapshot, and the incident
// timeline. The shared header / check-now / delete live in MonitorDetailBase.
@customElement("http-monitor-detail")
export class HttpMonitorDetail extends MonitorDetailBase {
  @state() private results: CheckResult[] = [];
  @state() private snapshot: FailureSnapshot | null = null;
  @state() private regionStates: RegionState[] = [];

  private pollTimer: number | null = null;
  private readonly onVisibility = () => this.syncPoll();

  override connectedCallback(): void {
    super.connectedCallback();
    document.addEventListener("visibilitychange", this.onVisibility);
  }

  override disconnectedCallback(): void {
    document.removeEventListener("visibilitychange", this.onVisibility);
    this.stopPoll();
    super.disconnectedCallback();
  }

  protected override async loadData(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId) return;
    const [results, snapshot] = await Promise.all([
      client.listResults(orgId, this.monitor.id, "24h"),
      client.lastFailure(orgId, this.monitor.id),
    ]);
    this.results = results.items ?? [];
    this.snapshot = snapshot;
    this.syncPoll();
  }

  protected override currentStatus(): CoverageStatus {
    if (!this.monitor.enabled) return "disabled";
    const last = this.results[0];
    if (!last) return "pending";
    return last.healthy ? "up" : "down";
  }

  protected override afterCheckAccepted(accepted: CheckNowAccepted): void {
    // Show the scheduled chips right away, then let the poll confirm on its next
    // tick. Schedule (don't fetch now) so the optimistic chips aren't wiped by a
    // poll that races ahead of the server reflecting the scheduled state.
    this.regionStates = accepted.regions;
    this.scheduleNextPoll();
  }

  // Kick the live region-state poll. It runs only while a check is in flight (a
  // region in scheduled/running) and the tab is visible; once every region settles
  // there is nothing to refetch until the next check, so the poll stops.
  private syncPoll(): void {
    if (document.visibilityState !== "visible" || !this.orgId) {
      this.stopPoll();
      return;
    }
    void this.pollRegionStates();
  }

  private stopPoll(): void {
    if (this.pollTimer !== null) {
      window.clearTimeout(this.pollTimer);
      this.pollTimer = null;
    }
  }

  private async pollRegionStates(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId) return;
    try {
      const res = await client.getMonitorRegionStates(orgId, this.monitor.id);
      this.regionStates = res.monitors?.[this.monitor.id] ?? [];
    } catch {
      // a failed poll is non-fatal; chips keep their last value, the next tick retries.
    }
    this.scheduleNextPoll();
  }

  private scheduleNextPoll(): void {
    this.stopPoll();
    const live = this.regionStates.some(
      (s) => s.state === "scheduled" || s.state === "running",
    );
    if (live && document.visibilityState === "visible" && this.orgId) {
      this.pollTimer = window.setTimeout(() => void this.pollRegionStates(), POLL_MS);
    }
  }

  private stats() {
    const total = this.results.length;
    const healthy = this.results.filter((r) => r.healthy).length;
    const uptime = total ? Math.round((healthy / total) * 1000) / 10 : null;
    const lats = this.results
      .map((r) => r.latency_ms)
      .filter((n): n is number => n !== null);
    const avg = lats.length
      ? Math.round(lats.reduce((a, b) => a + b, 0) / lats.length)
      : null;
    const p95 = percentile(lats, 95);
    const min = lats.length ? Math.min(...lats) : null;
    const max = lats.length ? Math.max(...lats) : null;
    return { uptime, avg, p95, min, max, total, healthy, last: this.results[0] ?? null };
  }

  // When the scheduler will next run this monitor: the last check plus the interval.
  // null when disabled or never checked, so there is nothing due.
  private nextCheckAt(): string | null {
    const m = this.monitor;
    const last = this.results[0]?.checked_at ?? null;
    if (!m.enabled || !last) return null;
    const next = new Date(last).getTime() + m.interval_seconds * 1000;
    if (Number.isNaN(next)) return null;
    return new Date(next).toISOString();
  }

  protected override body() {
    // When down, why it is failing (snapshot) and since when (incident) jump above
    // the historical charts; when recovered they drop back below as history.
    const snapshot = this.snapshotCard();
    const incidents = this.incidentsCard();
    const down = this.currentStatus() === "down";
    return html`
      ${this.statsRow()} ${this.regionsCard()}
      ${down ? html`${snapshot}${incidents}` : ""}
      ${this.chartCard()} ${this.resultsCard()}
      ${down ? "" : html`${incidents}${snapshot}`}
    `;
  }

  private statsRow() {
    const s = this.stats();
    const blank = "–";
    const stat = (
      title: string,
      value: string | TemplateResult,
      hint: string,
      desc?: string,
    ) => html`
      <div class="stat gap-1">
        <div class="stat-title text-xs uppercase tracking-wide flex items-center gap-1">
          ${title}${fieldHelp(hint)}
        </div>
        <div class="stat-value text-2xl">${value}</div>
        ${desc ? html`<div class="stat-desc">${desc}</div>` : ""}
      </div>
    `;
    const range =
      s.min !== null && s.max !== null
        ? `${formatLatency(s.min)} / ${formatLatency(s.max)}`
        : undefined;
    const nextSecs = secondsUntil(this.nextCheckAt());
    const nextCheck =
      nextSecs === null
        ? blank
        : nextSecs === 0
          ? t("monitors.nextCheckDue")
          : tDynamic("monitors.nextCheckIn", "", { when: formatDuration(nextSecs) });
    const cadence = this.monitor.interval_seconds
      ? tDynamic("monitors.everyInterval", "", {
          interval: formatDuration(this.monitor.interval_seconds),
        })
      : undefined;
    return html`
      <div
        class="stats stats-vertical sm:stats-horizontal w-full border border-base-300 bg-base-100 shadow-sm overflow-visible"
      >
        ${stat(
          t("monitor.statUptime"),
          s.uptime === null ? blank : `${s.uptime}%`,
          t("monitor.helpUptime"),
          s.total ? `${s.healthy}/${s.total} ${t("monitor.checks")}` : undefined,
        )}
        ${stat(t("monitor.statAvgLatency"), formatLatency(s.avg) ?? blank, t("monitor.helpAvgLatency"), range)}
        ${stat(t("monitor.statP95Latency"), formatLatency(s.p95) ?? blank, t("monitor.helpP95Latency"))}
        ${stat(
          t("monitor.statLastCheck"),
          s.last
            ? html`<relative-time .datetime=${s.last.checked_at}></relative-time>`
            : t("monitors.never"),
          t("monitor.helpLastCheck"),
          s.last?.region,
        )}
        ${stat(t("monitor.statNextCheck"), nextCheck, t("monitor.helpNextCheck"), cadence)}
      </div>
    `;
  }

  private regionsCard() {
    return html`
      <div class="card bg-base-100 border border-base-300 shadow-sm">
        <div class="card-body gap-4 p-5">
          <h2 class="font-semibold flex items-center gap-1">
            ${t("monitor.regionsTitle")}${fieldHelp(t("monitor.helpRegions"))}
          </h2>
          ${this.regionStates.length === 0
            ? html`<p class="text-base-content/60">${t("monitor.noResults")}</p>`
            : html`<region-chips .states=${this.regionStates}></region-chips>`}
        </div>
      </div>
    `;
  }

  private chartCard() {
    const points: LatencyPoint[] = [...this.results].reverse().map((r) => {
      const reason = r.healthy
        ? t("monitor.resultHealthy")
        : r.failure_reason
          ? t(FAILURE_LABEL[r.failure_reason])
          : t("monitor.resultFailed");
      const code = r.status_code != null ? ` · ${r.status_code}` : "";
      return {
        latency: r.latency_ms,
        healthy: r.healthy,
        label: formatDateTime(r.checked_at) ?? "",
        result: `${reason}${code}`,
      };
    });
    if (points.length < 2) return "";
    const bars: UptimeBar[] = [...this.results].reverse().map((r) => {
      const reason = r.healthy
        ? t("monitor.resultHealthy")
        : r.failure_reason
          ? t(FAILURE_LABEL[r.failure_reason])
          : t("monitor.resultFailed");
      return {
        healthy: r.healthy,
        title: `${formatDateTime(r.checked_at) ?? ""} · ${reason}`,
      };
    });
    return html`
      <div class="card bg-base-100 border border-base-300 shadow-sm">
        <div class="card-body gap-4 p-5">
          <h2 class="text-sm font-semibold text-base-content/70 flex items-center gap-1">
            ${t("monitor.uptimeTitle")}${fieldHelp(t("monitor.helpUptimeBar"))}
          </h2>
          <uptime-bar .bars=${bars}></uptime-bar>
          <h2 class="text-sm font-semibold text-base-content/70 mt-2 flex items-center gap-1">
            ${t("monitor.latencyTitle")}${fieldHelp(t("monitor.helpLatencyChart"))}
          </h2>
          <latency-chart .points=${points}></latency-chart>
        </div>
      </div>
    `;
  }

  private resultsCard() {
    const runs = this.groupRuns();
    const multiRegion = runs.some((run) => run.regions.length > 1);
    return html`
      <div class="card bg-base-100 border border-base-300 shadow-sm">
        <div class="card-body gap-4 p-5">
          <h2 class="font-semibold flex items-center gap-1">
            ${t("monitor.resultsTitle")}${fieldHelp(t("monitor.helpRecentChecks"))}
          </h2>
          ${runs.length === 0
            ? html`<p class="text-base-content/60">${t("monitor.noResults")}</p>`
            : html`<data-table
                .columns=${multiRegion ? this.runColumns() : this.singleRegionColumns()}
                .data=${runs}
                .pageSize=${10}
                .renderDetail=${multiRegion
                  ? (row: unknown) => this.runDetail(row as CheckRun)
                  : undefined}
              ></data-table>`}
        </div>
      </div>
    `;
  }

  // Fold the flat per-region results into one row per scheduler tick. Results arrive
  // newest-first, so the Map keeps the runs in that order too.
  private groupRuns(): CheckRun[] {
    const byTick = new Map<string, CheckResult[]>();
    for (const r of this.results) {
      const arr = byTick.get(r.scheduled_at);
      if (arr) arr.push(r);
      else byTick.set(r.scheduled_at, [r]);
    }
    return [...byTick.entries()].map(([scheduledAt, regions]) => {
      const sorted = [...regions].sort((a, b) => a.region.localeCompare(b.region));
      const lats = sorted
        .map((r) => r.latency_ms)
        .filter((n): n is number => n !== null);
      return {
        scheduledAt,
        regions: sorted,
        healthy: sorted.every((r) => r.healthy),
        latencyMin: lats.length ? Math.min(...lats) : null,
        latencyMax: lats.length ? Math.max(...lats) : null,
      };
    });
  }

  private singleRegionColumns(): DataColumn[] {
    const one = (r: unknown) => (r as CheckRun).regions[0];
    return [
      {
        id: "time",
        header: t("monitor.colTime"),
        headerHint: t("monitor.helpColTime"),
        accessor: (r) => (r as CheckRun).scheduledAt,
        sortable: true,
        class: "whitespace-nowrap",
        cell: (r) =>
          html`<relative-time
            .datetime=${(r as CheckRun).scheduledAt}
          ></relative-time>`,
      },
      {
        id: "result",
        header: t("monitor.colResult"),
        headerHint: t("monitor.helpColResult"),
        cell: (r) => this.resultBadge(one(r)),
      },
      {
        id: "code",
        header: t("monitor.colCode"),
        headerHint: t("monitor.helpColCode"),
        accessor: (r) => one(r).status_code ?? 0,
        cell: (r) => this.codeCell(one(r).status_code),
      },
      {
        id: "latency",
        header: t("monitors.colLatency"),
        headerHint: t("monitor.helpColLatency"),
        accessor: (r) => one(r).latency_ms ?? 0,
        sortable: true,
        cell: (r) => formatLatency(one(r).latency_ms) ?? "",
      },
    ];
  }

  private runColumns(): DataColumn[] {
    return [
      {
        id: "time",
        header: t("monitor.colTime"),
        headerHint: t("monitor.helpColTime"),
        accessor: (r) => (r as CheckRun).scheduledAt,
        sortable: true,
        class: "whitespace-nowrap",
        cell: (r) =>
          html`<relative-time
            .datetime=${(r as CheckRun).scheduledAt}
          ></relative-time>`,
      },
      {
        id: "result",
        header: t("monitor.colResult"),
        headerHint: t("monitor.helpColResult"),
        cell: (r) => this.runResultBadge(r as CheckRun),
      },
      {
        id: "regions",
        header: t("monitor.colRegions"),
        headerHint: t("monitor.helpColRegions"),
        accessor: (r) => (r as CheckRun).regions.length,
        cell: (r) => String((r as CheckRun).regions.length),
      },
      {
        id: "latency",
        header: t("monitors.colLatency"),
        headerHint: t("monitor.helpColLatency"),
        accessor: (r) => (r as CheckRun).latencyMax ?? 0,
        sortable: true,
        cell: (r) => this.latencyRange(r as CheckRun),
      },
    ];
  }

  private runResultBadge(run: CheckRun) {
    if (run.healthy) {
      return html`<span class="badge badge-success badge-soft badge-sm whitespace-nowrap"
        >${t("monitor.resultHealthy")}</span
      >`;
    }
    const failed = run.regions.filter((r) => !r.healthy).length;
    return html`<span class="badge badge-error badge-soft badge-sm whitespace-nowrap"
      >${failed}/${run.regions.length} ${t("monitor.runDown")}</span
    >`;
  }

  private latencyRange(run: CheckRun) {
    if (run.latencyMin === null) return "";
    if (run.latencyMin === run.latencyMax) return formatLatency(run.latencyMax) ?? "";
    return `${run.latencyMin}-${formatLatency(run.latencyMax)}`;
  }

  private runDetail(run: CheckRun) {
    return html`
      <table class="table table-sm">
        <thead>
          <tr class="text-xs uppercase tracking-wide">
            <th>${t("monitor.colRegion")}</th>
            <th>${t("monitor.colResult")}</th>
            <th>${t("monitor.colCode")}</th>
            <th>${t("monitors.colLatency")}</th>
            <th>${t("monitor.colTime")}</th>
          </tr>
        </thead>
        <tbody>
          ${run.regions.map(
            (r) => html`<tr>
              <td class="font-medium">${r.region}</td>
              <td>${this.resultBadge(r)}</td>
              <td>${this.codeCell(r.status_code)}</td>
              <td>${formatLatency(r.latency_ms) ?? ""}</td>
              <td class="whitespace-nowrap text-base-content/60">
                <relative-time .datetime=${r.checked_at}></relative-time>
              </td>
            </tr>`,
          )}
        </tbody>
      </table>
    `;
  }

  private resultBadge(c: CheckResult) {
    return c.healthy
      ? html`<span class="badge badge-success badge-soft badge-sm whitespace-nowrap"
          >${t("monitor.resultHealthy")}</span
        >`
      : html`<span class="badge badge-error badge-soft badge-sm whitespace-nowrap"
          >${c.failure_reason
            ? t(FAILURE_LABEL[c.failure_reason])
            : t("monitor.resultFailed")}</span
        >`;
  }

  private codeCell(code: number | null) {
    if (code === null) return "";
    return html`<span class="font-medium">${code}</span>
      <span class="text-base-content/60"
        >${formatStatusCode(code).replace(`${code} `, "")}</span
      >`;
  }

  // snapshotCard shows the captured response of the last failed check. The status
  // code, header names/values, and body are the monitored endpoint's OWN response,
  // i.e. attacker-controlled content. They are rendered ONLY through Lit text
  // bindings (${...} in child positions), which Lit HTML-escapes. We never use
  // unsafeHTML / innerHTML / attribute interpolation of this data, so a <script>
  // or <img onerror=...> in the body or a header is shown as literal text.
  private snapshotCard() {
    const s = this.snapshot;
    if (!s) return "";
    const headers = Object.entries(s.headers ?? {});
    return html`
      <div class="card bg-base-100 border border-error/40 shadow-sm">
        <div class="card-body gap-4 p-5">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <h2 class="font-semibold text-error flex items-center gap-1">
              ${t("monitor.lastFailureTitle")}${fieldHelp(t("monitor.helpLastFailure"))}
            </h2>
            <span class="text-sm text-base-content/60">
              <relative-time .datetime=${s.checked_at}></relative-time>${s.status_code !=
              null
                ? html` · ${formatStatusCode(s.status_code)}`
                : ""}
            </span>
          </div>

          <div>
            <h3 class="text-sm font-semibold text-base-content/70 mb-1">
              ${t("monitor.lastFailureHeaders")}
            </h3>
            ${headers.length === 0
              ? html`<p class="text-base-content/60 text-sm">–</p>`
              : html`<dl
                  class="text-xs grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1"
                >
                  ${headers.map(
                    ([key, values]) => html`
                      <dt class="font-mono text-base-content/70">${key}</dt>
                      <dd class="font-mono break-all">${values.join(", ")}</dd>
                    `,
                  )}
                </dl>`}
          </div>

          <div>
            <h3 class="text-sm font-semibold text-base-content/70 mb-1">
              ${t("monitor.lastFailureBody")}
              ${s.truncated
                ? html`<span class="badge badge-ghost badge-sm ml-2"
                    >${t("monitor.lastFailureTruncated")}</span
                  >`
                : ""}
            </h3>
            <pre
              class="bg-base-200 rounded p-3 text-xs overflow-auto max-h-80 whitespace-pre-wrap break-all"
            >${s.body}</pre>
          </div>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "http-monitor-detail": HttpMonitorDetail;
  }
}
