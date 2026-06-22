import { html, type TemplateResult } from "lit";
import { customElement, property, state, query } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { can } from "../state/can.js";
import { navigate } from "../router.js";
import { t, tDynamic, type MessageKey } from "../i18n.js";
import { toast } from "../toast.js";
import { toastCheckError } from "../check-now.js";
import {
  formatDateTime,
  formatDuration,
  formatLatency,
  formatStatusCode,
  secondsUntil,
} from "../format.js";
import type {
  CheckResult,
  CoverageStatus,
  FailureReason,
  FailureSnapshot,
  Incident,
  Monitor,
  RegionState,
} from "../api/types.js";
import { icon, fieldHelp } from "../icons.js";
import type { ConfirmDialog } from "./confirm-dialog.js";
import type { LatencyPoint } from "./latency-chart.js";

import "./status-badge.js";
import "./region-chips.js";
import "./relative-time.js";
import "./latency-chart.js";
import "./confirm-dialog.js";
import "./data-table.js";
import "./uptime-bar.js";
import type { DataColumn } from "./data-table.js";
import type { UptimeBar } from "./uptime-bar.js";

// How often the live region-state poll runs while the tab is visible.
const POLL_MS = 5000;

const FAILURE_LABEL: Record<FailureReason, MessageKey> = {
  connection_error: "failure.connection_error",
  timeout: "failure.timeout",
  status_mismatch: "failure.status_mismatch",
  latency_exceeded: "failure.latency_exceeded",
  body_assertion_failed: "failure.body_assertion_failed",
  blocked_target: "failure.blocked_target",
};

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

// Monitor detail (RFC-013 section 7.2). Loads the monitor, its recent check
// results, and its incidents in parallel, then shows derived stats (uptime, avg
// and p95 latency, last check), a latency chart, the results table, and the
// incident timeline. Check-now / edit / delete are member+ (the can() mirror).
@customElement("monitor-detail-view")
export class MonitorDetailView extends AppElement {
  // set from the route :id param
  @property({ type: String }) monitorId = "";

  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private monitor: Monitor | null = null;
  @state() private results: CheckResult[] = [];
  @state() private incidents: Incident[] = [];
  @state() private snapshot: FailureSnapshot | null = null;
  @state() private loading = false;
  @state() private error: string | null = null;
  @state() private checking = false;
  // live per-region check states for this monitor, refreshed by the poll.
  @state() private regionStates: RegionState[] = [];

  @query("confirm-dialog") private deleteDialog!: ConfirmDialog;

  private loadedKey: string | null = null;

  // The poll timer and the bound visibility handler. The poll runs only while the
  // tab is visible so a backgrounded detail page does no work (RFC-013 polish).
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

  private get orgId(): string | null {
    return this.ctx?.activeOrg?.org_id ?? null;
  }

  private get base(): string {
    return `/orgs/${this.orgId ?? ""}`;
  }

  override updated(): void {
    const orgId = this.orgId;
    const key = orgId && this.monitorId ? `${orgId}:${this.monitorId}` : null;
    if (key && key !== this.loadedKey) void this.load();
  }

  private async load(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId || !this.monitorId) return;
    this.loadedKey = `${orgId}:${this.monitorId}`;
    this.loading = true;
    this.error = null;
    try {
      const [monitor, results, incidents, snapshot] = await Promise.all([
        client.getMonitor(orgId, this.monitorId),
        client.listResults(orgId, this.monitorId, "24h"),
        client.listMonitorIncidents(orgId, this.monitorId),
        client.lastFailure(orgId, this.monitorId),
      ]);
      this.monitor = monitor;
      this.results = results.items ?? [];
      this.incidents = incidents.items ?? [];
      this.snapshot = snapshot;
      this.syncPoll();
    } catch (err) {
      this.error = err instanceof ApiError ? err.message : t("state.error");
    } finally {
      this.loading = false;
    }
  }

  // Start the poll when the tab is visible and we have a monitor to scope it to,
  // stop it otherwise. Called on connect, on visibility change, and after load.
  private syncPoll(): void {
    const shouldRun =
      !!this.orgId && !!this.monitorId && document.visibilityState === "visible";
    if (shouldRun && this.pollTimer === null) {
      void this.pollRegionStates();
      this.pollTimer = window.setInterval(() => void this.pollRegionStates(), POLL_MS);
    } else if (!shouldRun && this.pollTimer !== null) {
      this.stopPoll();
    }
  }

  private stopPoll(): void {
    if (this.pollTimer !== null) {
      window.clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
  }

  private async pollRegionStates(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId || !this.monitorId) return;
    try {
      const res = await client.getMonitorRegionStates(orgId, this.monitorId);
      this.regionStates = res.monitors?.[this.monitorId] ?? [];
    } catch {
      // a failed poll is non-fatal; the chips keep their last value and the next
      // tick tries again. We do not toast or surface poll errors.
    }
  }

  // The server accepts with 202 and returns every region in "scheduled"; we drop
  // those chips in right away and let the poll take over showing pinging / ok /
  // down, then pull fresh results once a region finishes. 409 and 429 surface as
  // toasts (see toastCheckError).
  private async onCheckNow(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId) return;
    this.checking = true;
    try {
      const accepted = await client.checkNow(orgId, this.monitorId);
      this.regionStates = accepted.regions;
      this.syncPoll();
      toast(t("monitor.checkQueued"), "info");
    } catch (err) {
      toastCheckError(err);
    } finally {
      this.checking = false;
    }
  }

  private async onDeleteConfirmed(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId) return;
    try {
      await client.deleteMonitor(orgId, this.monitorId);
      toast(t("monitor.deleted"), "success");
      navigate(this.base);
    } catch (err) {
      toast(err instanceof ApiError ? err.message : t("state.error"), "error");
    }
  }

  // Derived status for the header badge (Monitor has no status field).
  private currentStatus(): CoverageStatus {
    if (this.monitor && !this.monitor.enabled) return "disabled";
    const last = this.results[0];
    if (!last) return "pending";
    return last.healthy ? "up" : "down";
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

  // When the scheduler will next run this monitor: the last check plus the interval
  // (scheduler.go dispatchDue uses raw interval_seconds off the last check). null
  // when the monitor is disabled or has never been checked, so there is nothing due.
  private nextCheckAt(): string | null {
    const m = this.monitor;
    const last = this.results[0]?.checked_at ?? null;
    if (!m || !m.enabled || !last) return null;
    const next = new Date(last).getTime() + m.interval_seconds * 1000;
    if (Number.isNaN(next)) return null;
    return new Date(next).toISOString();
  }

  override render() {
    if (this.loading && !this.monitor) {
      return html`<div class="flex flex-col gap-6" aria-busy="true">
        <div class="skeleton h-9 w-64"></div>
        <div class="skeleton h-24 w-full"></div>
        <div class="skeleton h-56 w-full"></div>
        <div class="skeleton h-48 w-full"></div>
      </div>`;
    }
    if (this.error || !this.monitor) {
      return html`<div role="alert" class="alert alert-error">
        <span>${this.error ?? t("state.error")}</span>
        <button class="btn btn-sm" @click=${() => this.load()}>
          ${t("state.retry")}
        </button>
      </div>`;
    }
    // When the monitor is down, the two things an operator needs first are why it
    // is failing (the captured response) and since when / is it ongoing (the
    // incident). Both jump above the historical charts and the check table. When
    // it has recovered they drop back down: closed incidents and a past snapshot
    // are history, not a live problem, and should not read as one at the top.
    const snapshot = this.snapshotCard();
    const incidents = this.incidentsCard();
    const down = this.currentStatus() === "down";
    return html`
      <div class="flex flex-col gap-6">
        ${this.header()} ${this.statsRow()} ${this.regionsCard()}
        ${down ? html`${snapshot}${incidents}` : ""}
        ${this.chartCard()} ${this.resultsCard()}
        ${down ? "" : html`${incidents}${snapshot}`}
      </div>
      <confirm-dialog
        .heading=${t("monitor.deleteHeading")}
        .message=${t("monitor.deleteMessage")}
        .confirmLabel=${t("monitor.delete")}
        ?danger=${true}
        @confirm=${this.onDeleteConfirmed}
      ></confirm-dialog>
    `;
  }

  private header() {
    const m = this.monitor!;
    const role = this.ctx?.role ?? null;
    const member = can(role, "monitor.write");
    return html`
      <div
        class="flex flex-wrap items-start justify-between gap-3 pb-4 border-b border-base-300"
      >
        <div class="min-w-0">
          <div class="flex items-center gap-3">
            <h1 class="text-2xl font-bold truncate">${m.name}</h1>
            <status-badge .status=${this.currentStatus()}></status-badge>
          </div>
          <div class="text-base-content/60 text-sm mt-1 flex items-center gap-2">
            <span class="badge badge-ghost badge-sm">${m.method}</span>
            <span class="truncate">${m.url}</span>
          </div>
        </div>
        <div class="flex items-center gap-2">
          ${can(role, "monitor.test")
            ? html`<button
                class="btn btn-sm btn-primary gap-1.5"
                ?disabled=${this.checking}
                @click=${this.onCheckNow}
              >
                ${this.checking
                  ? html`<span class="loading loading-spinner loading-xs"></span>
                      ${t("monitor.checking")}`
                  : html`${icon("refresh", "size-4")}${t("monitor.checkNow")}`}
              </button>`
            : ""}
          ${member
            ? html`<a
                  class="btn btn-sm gap-1.5"
                  href=${`${this.base}/monitors/${m.id}/edit`}
                  >${icon("edit", "size-4")}${t("monitor.edit")}</a
                >
                <button
                  class="btn btn-sm btn-ghost text-error gap-1.5"
                  @click=${() => this.deleteDialog.open()}
                >
                  ${icon("trash", "size-4")}${t("monitor.delete")}
                </button>`
            : ""}
        </div>
      </div>
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
    const cadence = this.monitor?.interval_seconds
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

  // Live per-region status (RFC-013 multi-region), shown for every monitor, not
  // just multi-region ones. The chips come straight from the poll and update in
  // place as each region goes scheduled -> pinging -> ok / down. Before the first
  // poll lands there are no states yet, so we show "no checks yet".
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
    // chronological order for the chart (results come newest-first)
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
    // Show the per-region layout (regions count + expandable detail) only when a run
    // actually has more than one region. A single-region monitor falls back to the
    // plain time/result/code/latency columns, with no redundant region column.
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

  // Fold the flat per-region results into one row per scheduler tick. The results
  // arrive newest-first, so the Map keeps the runs in that order too.
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

  // Columns for a single-region monitor: each run is one region, so we read it
  // straight through and drop the region column entirely.
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

  // Columns for a multi-region monitor: one summary row per run (overall result,
  // how many regions ran, latency range), expandable to the per-region detail.
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

  // The whole-run badge: healthy only if every region passed, otherwise how many of
  // the run's regions are down. The per-region reasons show in the expanded detail.
  private runResultBadge(run: CheckRun) {
    if (run.healthy) {
      return html`<span class="badge badge-success badge-soft badge-sm"
        >${t("monitor.resultHealthy")}</span
      >`;
    }
    const failed = run.regions.filter((r) => !r.healthy).length;
    return html`<span class="badge badge-error badge-soft badge-sm"
      >${failed}/${run.regions.length} ${t("monitor.runDown")}</span
    >`;
  }

  private latencyRange(run: CheckRun) {
    if (run.latencyMin === null) return "";
    if (run.latencyMin === run.latencyMax) return formatLatency(run.latencyMax) ?? "";
    return `${run.latencyMin}-${formatLatency(run.latencyMax)}`;
  }

  // The expanded detail for a multi-region run: one row per region with its own
  // result, code, latency, and the moment it actually ran.
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
      ? html`<span class="badge badge-success badge-soft badge-sm"
          >${t("monitor.resultHealthy")}</span
        >`
      : html`<span class="badge badge-error badge-soft badge-sm"
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

  private incidentsCard() {
    return html`
      <div class="card bg-base-100 border border-base-300 shadow-sm">
        <div class="card-body gap-4 p-5">
          <h2 class="font-semibold flex items-center gap-1">
            ${t("monitor.incidentsTitle")}${fieldHelp(t("monitor.helpIncidents"))}
          </h2>
          ${this.incidents.length === 0
            ? html`<p class="text-base-content/60">${t("monitor.noIncidents")}</p>`
            : html`<ul class="flex flex-col gap-2">
                ${this.incidents.map((i) => this.incidentRow(i))}
              </ul>`}
        </div>
      </div>
    `;
  }

  // snapshotCard shows the captured response of the last failed check. The status
  // code, header names/values, and body are the monitored endpoint's OWN response,
  // i.e. attacker-controlled content. They are rendered ONLY through Lit text
  // bindings (${...} in child positions), which Lit HTML-escapes. We never use
  // unsafeHTML / innerHTML / attribute interpolation of this data, so a <script>
  // or <img onerror=...> in the body or a header is shown as literal text and
  // never parsed or executed.
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

  private incidentRow(i: Incident) {
    const ongoing = i.ended_at === null;
    return html`
      <li class="flex flex-wrap items-center gap-3 border-l-2 border-error pl-3 py-1">
        <span class="badge badge-sm ${ongoing ? "badge-error" : "badge-ghost"}">
          ${ongoing
            ? t("monitor.incidentOngoing")
            : formatDuration(i.duration_seconds)}
        </span>
        <span class="text-sm">
          ${t("monitor.incidentStarted")}:
          <relative-time .datetime=${i.started_at}></relative-time>
        </span>
        <span class="badge badge-ghost badge-sm">
          ${t(FAILURE_LABEL[i.cause_reason])}
        </span>
      </li>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "monitor-detail-view": MonitorDetailView;
  }
}
