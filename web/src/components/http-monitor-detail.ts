import { html } from "lit";
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
    // The uptime headline and the latency chart live in the dominant band above
    // (instrumentBand). Here: the region strip, then the recent-checks history. When
    // down, why it is failing (snapshot) and since when (incident) jump above the
    // history; when recovered they drop back below it.
    const snapshot = this.snapshotCard();
    const incidents = this.incidentsCard();
    const down = this.currentStatus() === "down";
    return html`
      ${this.regionsCard()}
      ${down ? html`${snapshot}${incidents}` : ""}
      ${this.resultsCard()}
      ${down ? "" : html`${incidents}${snapshot}`}
    `;
  }

  // The dominant figure band, full-bleed under the header: the 24h uptime as one
  // very large Archivo numeral on the left, the latency chart as a wide band beside
  // it, and the secondary numbers (avg, p95, last/next check) as a quiet mono spec
  // strip across the bottom. Uptime and p95 are computed client-side from the loaded
  // 24h results; a windowed uptime / server p95 over a longer range would come from
  // future backend fields.
  protected override instrumentBand() {
    const s = this.stats();
    const blank = "—";
    const fails = s.total - s.healthy;
    // keep the % glued to the numeral (one token) and small, so the giant Archivo
    // figure still reads as a percentage at a glance.
    const uptimeValue =
      s.uptime === null
        ? html`<span class="text-ink3">${blank}</span>`
        : html`${s.uptime}<span class="text-[0.3em] align-top text-ink3">%</span>`;

    const nextSecs = secondsUntil(this.nextCheckAt());
    const nextCheck =
      nextSecs === null
        ? blank
        : nextSecs === 0
          ? t("monitors.nextCheckDue")
          : tDynamic("monitors.nextCheckIn", "", { when: formatDuration(nextSecs) });
    const spec: { label: string; value: unknown; tone?: string }[] = [
      { label: t("monitor.statAvgLatency"), value: formatLatency(s.avg) ?? blank },
      { label: t("monitor.statP95Latency"), value: formatLatency(s.p95) ?? blank },
      {
        label: t("monitor.statLastCheck"),
        value: s.last
          ? html`<relative-time .datetime=${s.last.checked_at}></relative-time>`
          : t("monitors.never"),
      },
      {
        label: t("monitor.statNextCheck"),
        value: nextCheck,
        tone: nextSecs === 0 ? "text-deg" : "",
      },
    ];

    return html`
      <section class="border-b border-line">
        <div class="grid grid-cols-1 lg:grid-cols-[minmax(0,1fr)_1.45fr]">
          <div
            class="flex flex-col justify-center gap-4 px-6 lg:px-10 pt-8 pb-7 border-line lg:border-r"
          >
            <div>
              <div class="pulse-label">${t("monitor.statUptime")} · 24h</div>
              <div
                class="font-disp font-black leading-[0.82] tracking-[-0.05em] text-7xl lg:text-8xl mt-2.5"
              >
                ${uptimeValue}
              </div>
            </div>
            ${s.total
              ? html`<div
                  class="font-mono text-[11.5px] text-ink2 flex flex-wrap gap-x-[18px] gap-y-1"
                >
                  <span class="text-up"
                    >${s.healthy} ${t("monitor.resultHealthy")}</span
                  >
                  ${fails
                    ? html`<span class="text-down"
                        >${fails} ${t("monitor.runDown")}</span
                      >`
                    : ""}
                  <span>${s.total} ${t("monitor.checks")}</span>
                </div>`
              : ""}
          </div>
          ${this.latencyPanel()}
        </div>
        <div
          class="grid grid-cols-2 sm:grid-cols-4 gap-x-8 gap-y-3 px-6 lg:px-10 py-4 border-t border-hair"
        >
          ${spec.map(
            (c) => html`<div class="min-w-0">
              <div class="pulse-label">${c.label}</div>
              <div class="font-mono text-[14px] mt-1 truncate ${c.tone ?? ""}">
                ${c.value}
              </div>
            </div>`,
          )}
        </div>
      </section>
    `;
  }

  // Region status as a horizontal strip of region markers.
  private regionsCard() {
    return html`
      <div class="pulse-panel p-5 flex flex-col gap-3.5">
        <h2 class="m-0 pulse-section-title flex items-center gap-1">
          ${t("monitor.regionsTitle")}${fieldHelp(t("monitor.helpRegions"))}
        </h2>
        ${this.regionStates.length === 0
          ? html`<p class="font-mono text-[12px] text-ink3">
              ${t("monitor.noResults")}
            </p>`
          : html`<region-chips .states=${this.regionStates}></region-chips>`}
      </div>
    `;
  }

  // The right column of the dominant band: the wide latency chart with the uptime
  // bar as a quieter strip underneath. The chart needs at least two points; with
  // fewer (a brand-new monitor) it shows the "no checks yet" note instead, and the
  // bar only appears once there is at least one check to draw.
  private latencyPanel() {
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
      <div class="flex flex-col gap-5 px-6 lg:px-10 py-7 bg-paper min-w-0">
        <div class="flex items-baseline justify-between gap-3">
          <h2 class="m-0 pulse-section-title flex items-center gap-1">
            ${t("monitor.latencyTitle")}${fieldHelp(t("monitor.helpLatencyChart"))}
          </h2>
          <span class="font-mono text-[11px] text-ink3"
            >${points.length} ${t("monitor.checks")}</span
          >
        </div>
        ${points.length < 2
          ? html`<p class="font-mono text-[12px] text-ink3">
              ${t("monitor.noResults")}
            </p>`
          : html`<latency-chart .points=${points}></latency-chart>`}
        ${bars.length
          ? html`<h3
                class="m-0 mt-1 pulse-section-title text-ink2 flex items-center gap-1"
              >
                ${t("monitor.uptimeTitle")}${fieldHelp(t("monitor.helpUptimeBar"))}
              </h3>
              <uptime-bar .bars=${bars}></uptime-bar>`
          : ""}
      </div>
    `;
  }

  private resultsCard() {
    const runs = this.groupRuns();
    const multiRegion = runs.some((run) => run.regions.length > 1);
    return html`
      <div class="pulse-panel p-5 flex flex-col gap-4">
        <div class="flex items-baseline justify-between gap-3">
          <h2 class="m-0 pulse-section-title flex items-center gap-1">
            ${t("monitor.resultsTitle")}${fieldHelp(t("monitor.helpRecentChecks"))}
          </h2>
          <span class="font-mono text-[11px] text-ink3"
            >${String(runs.length).padStart(2, "0")}</span
          >
        </div>
        ${runs.length === 0
          ? html`<p class="font-mono text-[12px] text-ink3">
              ${t("monitor.noResults")}
            </p>`
          : html`<data-table
                .columns=${multiRegion ? this.runColumns() : this.singleRegionColumns()}
                .data=${runs}
                .pageSize=${10}
                .renderDetail=${multiRegion
                  ? (row: unknown) => this.runDetail(row as CheckRun)
                  : undefined}
              ></data-table>`}
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
      return html`<span class="pulse-state text-up whitespace-nowrap"
        ><span class="pulse-state-sq bg-up"></span
        >${t("monitor.resultHealthy")}</span
      >`;
    }
    const failed = run.regions.filter((r) => !r.healthy).length;
    return html`<span class="pulse-state text-down whitespace-nowrap"
      ><span class="pulse-state-sq bg-down"></span>${failed}/${run.regions.length}
      ${t("monitor.runDown")}</span
    >`;
  }

  private latencyRange(run: CheckRun) {
    if (run.latencyMin === null) return "";
    if (run.latencyMin === run.latencyMax) return formatLatency(run.latencyMax) ?? "";
    return `${run.latencyMin}-${formatLatency(run.latencyMax)}`;
  }

  private runDetail(run: CheckRun) {
    return html`
      <table class="w-full text-sm">
        <thead>
          <tr
            class="text-left font-mono text-[11px] uppercase tracking-[0.04em] text-ink3 border-b border-hair"
          >
            <th class="font-normal py-1.5 pr-4">${t("monitor.colRegion")}</th>
            <th class="font-normal py-1.5 pr-4">${t("monitor.colResult")}</th>
            <th class="font-normal py-1.5 pr-4">${t("monitor.colCode")}</th>
            <th class="font-normal py-1.5 pr-4">${t("monitors.colLatency")}</th>
            <th class="font-normal py-1.5">${t("monitor.colTime")}</th>
          </tr>
        </thead>
        <tbody>
          ${run.regions.map(
            (r) => html`<tr class="border-b border-hair">
              <td class="font-medium py-1.5 pr-4">${r.region}</td>
              <td class="py-1.5 pr-4">${this.resultBadge(r)}</td>
              <td class="py-1.5 pr-4">${this.codeCell(r.status_code)}</td>
              <td class="py-1.5 pr-4">${formatLatency(r.latency_ms) ?? ""}</td>
              <td class="whitespace-nowrap text-ink3 py-1.5">
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
      ? html`<span class="pulse-state text-up whitespace-nowrap"
          ><span class="pulse-state-sq bg-up"></span
          >${t("monitor.resultHealthy")}</span
        >`
      : html`<span class="pulse-state text-down whitespace-nowrap"
          ><span class="pulse-state-sq bg-down"></span
          >${c.failure_reason
            ? t(FAILURE_LABEL[c.failure_reason])
            : t("monitor.resultFailed")}</span
        >`;
  }

  private codeCell(code: number | null) {
    if (code === null) return "";
    return html`<span class="font-medium">${code}</span>
      <span class="text-ink3"
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
      <div class="border border-down">
        <div class="p-5 flex flex-col gap-4">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <h2
              class="m-0 pulse-section-title text-down flex items-center gap-1"
            >
              ${t("monitor.lastFailureTitle")}${fieldHelp(t("monitor.helpLastFailure"))}
            </h2>
            <span class="text-sm text-ink3">
              <relative-time .datetime=${s.checked_at}></relative-time>${s.status_code !=
              null
                ? html` · ${formatStatusCode(s.status_code)}`
                : ""}
            </span>
          </div>

          <div>
            <h3
              class="m-0 mb-1 pulse-section-title text-ink2"
            >
              ${t("monitor.lastFailureHeaders")}
            </h3>
            ${headers.length === 0
              ? html`<p class="text-ink3 text-sm">–</p>`
              : html`<dl
                  class="text-xs grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1"
                >
                  ${headers.map(
                    ([key, values]) => html`
                      <dt class="font-mono text-ink2">${key}</dt>
                      <dd class="font-mono break-all">${values.join(", ")}</dd>
                    `,
                  )}
                </dl>`}
          </div>

          <div>
            <h3
              class="m-0 mb-1 pulse-section-title text-ink2 flex items-center gap-2"
            >
              ${t("monitor.lastFailureBody")}
              ${s.truncated
                ? html`<span
                    class="pulse-tag"
                    >${t("monitor.lastFailureTruncated")}</span
                  >`
                : ""}
            </h3>
            <pre
              class="bg-paper p-3 text-xs overflow-auto max-h-80 whitespace-pre-wrap break-all"
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
