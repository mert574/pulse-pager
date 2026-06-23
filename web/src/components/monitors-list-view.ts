import { html } from "lit";
import { customElement, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { can } from "../state/can.js";
import { t, tDynamic } from "../i18n.js";
import { toast } from "../toast.js";
import { toastCheckError } from "../check-now.js";
import { formatDuration, formatLatency, secondsUntil } from "../format.js";
import type { MonitorListItem, RegionState } from "../api/types.js";

import { icon } from "../icons.js";
import "./status-badge.js";
import "./region-chips.js";
import "./upsell-banner.js";
import "./data-table.js";
import "./relative-time.js";
import type { DataColumn } from "./data-table.js";

// How often the live region-state poll runs while the tab is visible.
const POLL_MS = 5000;

// Monitors list, the org home (RFC-013 section 7.1). Fetches the active org's
// monitors and renders the three required states: loading, empty (with the
// primary action), and error (with retry); plus the data table. "New monitor" is
// shown to member+ and disabled at the plan's monitor cap with an upsell, both
// mirroring the server (RFC-013 section 6.3, 10.2).
@customElement("monitors-list-view")
export class MonitorsListView extends AppElement {
  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private monitors: MonitorListItem[] | null = null;
  @state() private loading = false;
  @state() private error: string | null = null;
  // monitor id whose check-now or enable-toggle is in flight, so its row control
  // shows a spinner and cannot be double-fired.
  @state() private busyId: string | null = null;
  // live per-region check states, keyed by monitor id, refreshed by the poll.
  @state() private regionStates = new Map<string, RegionState[]>();
  private loadedOrgId: string | null = null;

  // The poll timer and the bound visibility handler. The poll runs only while the
  // tab is visible so a backgrounded list does no work (RFC-013 polish).
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

  // Load when the active org first appears or changes (e.g. via the switcher).
  override updated(): void {
    const orgId = this.ctx?.activeOrg?.org_id ?? null;
    if (orgId && orgId !== this.loadedOrgId) void this.load(orgId);
  }

  // Kick the live region-state poll. It runs only while a check is actually in
  // flight (some region in scheduled/running) and the tab is visible: once every
  // monitor settles there is nothing to refetch until the next check, so the poll
  // stops instead of hitting the endpoint every few seconds forever. A check-now
  // or reopening the tab kicks it off again. Called on visibility change, after
  // the first load, and after check-now.
  private syncPoll(): void {
    const orgId = this.ctx?.activeOrg?.org_id ?? null;
    if (!orgId || document.visibilityState !== "visible") {
      this.stopPoll();
      return;
    }
    // One fetch now; it self-schedules the next tick only while a check is live.
    void this.pollRegionStates();
  }

  private stopPoll(): void {
    if (this.pollTimer !== null) {
      window.clearTimeout(this.pollTimer);
      this.pollTimer = null;
    }
  }

  private async pollRegionStates(): Promise<void> {
    const orgId = this.ctx?.activeOrg?.org_id;
    if (!orgId) return;
    try {
      const res = await client.getMonitorRegionStates(orgId);
      this.regionStates = new Map(Object.entries(res.monitors ?? {}));
    } catch {
      // a failed poll is non-fatal; the table keeps its last chips and the next
      // tick tries again. We do not toast or surface poll errors.
    }
    this.scheduleNextPoll();
  }

  // Schedule the next tick only while some monitor has a check in flight and the
  // tab is visible, so a settled list stops polling instead of refetching.
  private scheduleNextPoll(): void {
    this.stopPoll();
    const live = Array.from(this.regionStates.values()).some((states) =>
      states.some((s) => s.state === "scheduled" || s.state === "running"),
    );
    if (live && this.ctx?.activeOrg?.org_id && document.visibilityState === "visible") {
      this.pollTimer = window.setTimeout(() => void this.pollRegionStates(), POLL_MS);
    }
  }

  private async load(orgId: string): Promise<void> {
    this.loadedOrgId = orgId;
    this.loading = true;
    this.error = null;
    try {
      this.monitors = await client.listMonitors(orgId);
      this.syncPoll();
    } catch (err) {
      this.error = err instanceof ApiError ? err.message : t("state.error");
      this.monitors = null;
    } finally {
      this.loading = false;
    }
  }

  private retry(): void {
    const orgId = this.ctx?.activeOrg?.org_id;
    if (orgId) void this.load(orgId);
  }

  // Run an on-demand check for one row. The server accepts with 202 and returns
  // every region in "scheduled"; we drop those chips on the row right away and
  // let the poll take over showing pinging / ok / down. 409 and 429 surface as
  // toasts (see toastCheckError).
  private async onCheckNow(m: MonitorListItem): Promise<void> {
    const orgId = this.ctx?.activeOrg?.org_id;
    if (!orgId || this.busyId) return;
    this.busyId = m.id;
    try {
      const accepted = await client.checkNow(orgId, m.id);
      // Show the scheduled chips right away, then let the poll confirm on its next
      // tick. Schedule (don't fetch now), so the optimistic chips aren't wiped by a
      // poll that races ahead of the server reflecting the scheduled state.
      const next = new Map(this.regionStates);
      next.set(accepted.monitor_id, accepted.regions);
      this.regionStates = next;
      this.scheduleNextPoll();
      toast(t("monitor.checkQueued"), "info");
    } catch (err) {
      toastCheckError(err);
    } finally {
      this.busyId = null;
    }
  }

  // Flip a monitor's enabled flag in place. The list endpoint gives only the row
  // summary, so we read the full monitor first and PUT it back with enabled
  // toggled (the API has no partial update).
  private async onToggleEnabled(m: MonitorListItem): Promise<void> {
    const orgId = this.ctx?.activeOrg?.org_id;
    if (!orgId || this.busyId) return;
    this.busyId = m.id;
    const next = !m.enabled;
    try {
      const full = await client.getMonitor(orgId, m.id);
      await client.updateMonitor(orgId, m.id, {
        name: full.name,
        url: full.url,
        method: full.method,
        headers: full.headers,
        body: full.body,
        expected_status_codes: full.expected_status_codes,
        timeout_seconds: full.timeout_seconds,
        interval_seconds: full.interval_seconds,
        enabled: next,
        max_latency_ms: full.max_latency_ms,
        body_contains: full.body_contains,
        failure_threshold: full.failure_threshold,
        notification_channel_ids: full.notification_channel_ids,
        regions: full.regions,
        down_policy: full.down_policy,
      });
      this.monitors = (this.monitors ?? []).map((row) =>
        row.id === m.id
          ? { ...row, enabled: next, status: next ? row.status : "disabled" }
          : row,
      );
      toast(t(next ? "monitors.enabled" : "monitors.disabled"), "success");
    } catch (err) {
      toast(err instanceof ApiError ? err.message : t("state.error"), "error");
    } finally {
      this.busyId = null;
    }
  }

  private get base(): string {
    return `/orgs/${this.ctx?.activeOrg?.org_id ?? ""}`;
  }

  private get atCap(): boolean {
    const e = this.ctx?.entitlements;
    return !!e && e.monitors_used >= e.monitors_cap;
  }

  private newMonitorButton() {
    if (!can(this.ctx?.role ?? null, "monitor.write")) return "";
    if (this.atCap) {
      return html`<button class="btn btn-primary btn-sm gap-1.5" disabled>
        ${icon("plus", "size-4")}${t("monitors.new")}
      </button>`;
    }
    return html`<a
      class="btn btn-primary btn-sm gap-1.5"
      href=${`${this.base}/monitors/new`}
    >
      ${icon("plus", "size-4")}${t("monitors.new")}
    </a>`;
  }

  override render() {
    return html`
      <div class="flex flex-col gap-4">
        <div class="flex items-center justify-between">
          <h1 class="text-2xl font-bold">${t("monitors.heading")}</h1>
          ${this.newMonitorButton()}
        </div>
        ${this.atCap && can(this.ctx?.role ?? null, "monitor.write")
          ? html`<upsell-banner
              .upgradeHref=${`${this.base}/billing`}
            ></upsell-banner>`
          : ""}
        ${this.body()}
      </div>
    `;
  }

  private body() {
    if (this.loading && this.monitors === null) {
      return html`<div class="flex flex-col gap-2" aria-busy="true">
        ${Array.from({ length: 6 }).map(
          () => html`<div class="skeleton h-12 w-full"></div>`,
        )}
      </div>`;
    }

    if (this.error) {
      return html`<div role="alert" class="alert alert-error">
        <span>${this.error}</span>
        <button class="btn btn-sm" @click=${this.retry}>
          ${t("state.retry")}
        </button>
      </div>`;
    }

    if (!this.monitors || this.monitors.length === 0) {
      return html`<div
        class="rounded-box border border-dashed border-base-300 p-12 flex flex-col items-center text-center gap-3"
      >
        <span class="text-primary/70">${icon("activity", "size-10")}</span>
        <div>
          <p class="font-semibold text-lg">${t("monitors.empty")}</p>
          <p class="text-base-content/60 mt-1">${t("monitors.emptyHint")}</p>
        </div>
        ${can(this.ctx?.role ?? null, "monitor.write") && !this.atCap
          ? html`<a
              class="btn btn-primary btn-sm gap-1.5"
              href=${`${this.base}/monitors/new`}
              >${icon("plus", "size-4")}${t("monitors.new")}</a
            >`
          : ""}
      </div>`;
    }

    return html`<data-table
      .columns=${this.columns()}
      .data=${this.monitors}
      .pageSize=${15}
    ></data-table>`;
  }

  // The "Next check" cell: a relative countdown from next_check_at ("in 4m" /
  // "due now"), with the cadence ("every 60s") underneath from interval_seconds.
  // A null next_check_at (disabled, never scheduled) shows only the cadence.
  private nextCheckCell(m: MonitorListItem) {
    const secs = secondsUntil(m.next_check_at);
    const when =
      secs === null
        ? null
        : secs === 0
          ? t("monitors.nextCheckDue")
          : tDynamic("monitors.nextCheckIn", "", { when: formatDuration(secs) });
    const cadence = m.interval_seconds
      ? tDynamic("monitors.everyInterval", "", {
          interval: formatDuration(m.interval_seconds),
        })
      : "";
    return html`<div class="flex flex-col leading-tight">
      <span>${when ?? "—"}</span>
      ${cadence
        ? html`<span class="text-base-content/50 text-xs">${cadence}</span>`
        : ""}
    </div>`;
  }

  private columns(): DataColumn[] {
    const base = this.base;
    const canWrite = can(this.ctx?.role ?? null, "monitor.write");
    const canCheck = can(this.ctx?.role ?? null, "monitor.test");
    const cols: DataColumn[] = [
      {
        id: "name",
        header: t("monitors.colName"),
        accessor: (r) => (r as MonitorListItem).name,
        sortable: true,
        cell: (r) => {
          const m = r as MonitorListItem;
          return html`<a
              class="link link-hover font-medium"
              href=${`${base}/monitors/${m.id}`}
              >${m.name}</a
            >${m.incident_open
              ? html`<span class="badge badge-error badge-sm ml-2"
                  >${t("monitors.incident")}</span
                >`
              : ""}
            <div class="text-base-content/50 text-xs">${m.url}</div>`;
        },
      },
      {
        id: "status",
        header: t("monitors.colStatus"),
        accessor: (r) => (r as MonitorListItem).status,
        sortable: true,
        cell: (r) =>
          html`<status-badge
            .status=${(r as MonitorListItem).status}
          ></status-badge>`,
      },
      {
        id: "regions",
        header: t("monitors.colRegions"),
        cell: (r) => {
          const states = this.regionStates.get((r as MonitorListItem).id);
          if (!states || states.length === 0)
            return html`<span class="text-base-content/40">—</span>`;
          return html`<region-chips .states=${states}></region-chips>`;
        },
      },
      {
        id: "nextCheck",
        header: t("monitors.colNextCheck"),
        accessor: (r) => secondsUntil((r as MonitorListItem).next_check_at) ?? Infinity,
        sortable: true,
        class: "text-base-content/70 whitespace-nowrap",
        cell: (r) => this.nextCheckCell(r as MonitorListItem),
      },
      {
        id: "lastCheck",
        header: t("monitors.colLastCheck"),
        accessor: (r) => (r as MonitorListItem).last_check_at ?? "",
        sortable: true,
        class: "text-base-content/70",
        cell: (r) => {
          const v = (r as MonitorListItem).last_check_at;
          return v
            ? html`<relative-time .datetime=${v}></relative-time>`
            : t("monitors.never");
        },
      },
      {
        id: "latency",
        header: t("monitors.colLatency"),
        accessor: (r) => (r as MonitorListItem).last_latency_ms ?? 0,
        sortable: true,
        class: "text-base-content/70",
        cell: (r) => formatLatency((r as MonitorListItem).last_latency_ms) ?? "",
      },
    ];

    // The enable toggle and check-now action are member+ only; viewers see a
    // read-only list (the server re-checks both).
    if (canWrite) {
      cols.push({
        id: "enabled",
        header: t("monitors.colEnabled"),
        accessor: (r) => ((r as MonitorListItem).enabled ? 1 : 0),
        sortable: true,
        cell: (r) => {
          const m = r as MonitorListItem;
          return html`<input
            type="checkbox"
            class="toggle toggle-sm"
            aria-label=${t("monitors.toggleEnabled")}
            .checked=${m.enabled}
            ?disabled=${this.busyId === m.id}
            @change=${() => this.onToggleEnabled(m)}
          />`;
        },
      });
    }

    if (canCheck) {
      cols.push({
        id: "actions",
        header: t("monitors.colActions"),
        cell: (r) => {
          const m = r as MonitorListItem;
          const busy = this.busyId === m.id;
          return html`<button
            class="btn btn-xs btn-ghost gap-1"
            ?disabled=${busy}
            title=${t("monitor.checkNow")}
            @click=${() => this.onCheckNow(m)}
          >
            ${busy
              ? html`<span class="loading loading-spinner loading-xs"></span>`
              : icon("refresh", "size-3.5")}${t("monitor.checkNow")}
          </button>`;
        },
      });
    }

    return cols;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "monitors-list-view": MonitorsListView;
  }
}
