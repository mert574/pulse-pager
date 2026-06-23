import { html } from "lit";
import { customElement, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { can } from "../state/can.js";
import { t, tDynamic, type MessageKey } from "../i18n.js";
import { toast } from "../toast.js";
import { toastCheckError } from "../check-now.js";
import { formatDuration, formatLatency, secondsUntil } from "../format.js";
import type { MonitorListItem, MonitorType, RegionState } from "../api/types.js";

import { icon } from "../icons.js";
import "./status-badge.js";
import "./region-chips.js";
import "./upsell-banner.js";
import "./data-table.js";
import "./relative-time.js";
import type { DataColumn } from "./data-table.js";

// How often the live region-state poll runs while the tab is visible.
const POLL_MS = 5000;

// The list groups monitors by check type, in this order, each under its own
// heading. A type with no monitors is skipped.
const TYPE_ORDER: MonitorType[] = ["http", "ssl"];
const TYPE_LABEL: Record<MonitorType, MessageKey> = {
  http: "monitorForm.typeHttp",
  ssl: "monitorForm.typeSsl",
};

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
        type: full.type,
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

    // Group by check type so http and ssl monitors read as distinct sections, each
    // with its own table (and its own sort/paging). Single-group lists still get a
    // heading, which keeps the page consistent as more types land.
    const groups = TYPE_ORDER.map((type) => ({
      type,
      rows: this.monitors!.filter((m) => m.type === type),
    })).filter((g) => g.rows.length > 0);

    return html`<div class="flex flex-col gap-6">
      ${groups.map(
        (g) => html`<section class="flex flex-col gap-2">
          <h2 class="text-sm font-semibold text-base-content/60">
            ${t(TYPE_LABEL[g.type])} (${g.rows.length})
          </h2>
          <data-table
            .columns=${this.columns(g.type, g.rows)}
            .data=${g.rows}
            .pageSize=${15}
          ></data-table>
        </section>`,
      )}
    </div>`;
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

  // The last-check cell, shared by both types.
  private lastCheckColumn(): DataColumn {
    return {
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
    };
  }

  // The cert-expiry cell for an ssl monitor: a days-to-expiry badge (warning inside
  // a week, error once expired), sortable by the soonest expiry.
  private expiryCell(m: MonitorListItem) {
    if (!m.cert_expires_at) return html`<span class="text-base-content/40">—</span>`;
    const days = Math.floor(
      (new Date(m.cert_expires_at).getTime() - Date.now()) / (24 * 60 * 60 * 1000),
    );
    if (days < 0) {
      return html`<span class="badge badge-error badge-soft badge-sm"
        >${t("monitor.certExpired")}</span
      >`;
    }
    const cls = days <= 7 ? "badge-warning" : "badge-success";
    return html`<span class="badge ${cls} badge-soft badge-sm"
      >${days} ${t("monitor.certDaysLeft")}</span
    >`;
  }

  // The columns each check type shows in its own table. The shared identity/status
  // columns lead and the member-only enable/actions trail; the type only decides the
  // middle. Picked by lookup (not a type branch): http shows regions / next-check /
  // latency, which mean nothing for a daily cert check, and ssl shows the cert expiry.
  private readonly typeColumns: Record<MonitorType, () => DataColumn[]> = {
    http: () => [
      this.regionsColumn(),
      this.nextCheckColumn(),
      this.lastCheckColumn(),
      this.latencyColumn(),
    ],
    ssl: () => [this.expiryColumn(), this.lastCheckColumn()],
  };

  // Whether a manual check-now is useful for a row. An http check is always worth
  // re-running on demand; a daily ssl cert check is not, except to confirm a fix
  // and clear an open incident. Picked by row type, not a branch.
  private readonly checkNowVisible: Record<MonitorType, (m: MonitorListItem) => boolean> = {
    http: () => true,
    ssl: (m) => m.incident_open,
  };

  private columns(type: MonitorType, rows: MonitorListItem[]): DataColumn[] {
    return [
      this.nameColumn(),
      this.statusColumn(),
      ...this.typeColumns[type](),
      ...this.actionColumns(rows),
    ];
  }

  private nameColumn(): DataColumn {
    const base = this.base;
    return {
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
    };
  }

  private statusColumn(): DataColumn {
    return {
      id: "status",
      header: t("monitors.colStatus"),
      accessor: (r) => (r as MonitorListItem).status,
      sortable: true,
      cell: (r) =>
        html`<status-badge .status=${(r as MonitorListItem).status}></status-badge>`,
    };
  }

  private regionsColumn(): DataColumn {
    return {
      id: "regions",
      header: t("monitors.colRegions"),
      cell: (r) => {
        const states = this.regionStates.get((r as MonitorListItem).id);
        if (!states || states.length === 0)
          return html`<span class="text-base-content/40">—</span>`;
        return html`<region-chips .states=${states}></region-chips>`;
      },
    };
  }

  private nextCheckColumn(): DataColumn {
    return {
      id: "nextCheck",
      header: t("monitors.colNextCheck"),
      accessor: (r) => secondsUntil((r as MonitorListItem).next_check_at) ?? Infinity,
      sortable: true,
      class: "text-base-content/70 whitespace-nowrap",
      cell: (r) => this.nextCheckCell(r as MonitorListItem),
    };
  }

  private latencyColumn(): DataColumn {
    return {
      id: "latency",
      header: t("monitors.colLatency"),
      accessor: (r) => (r as MonitorListItem).last_latency_ms ?? 0,
      sortable: true,
      class: "text-base-content/70",
      cell: (r) => formatLatency((r as MonitorListItem).last_latency_ms) ?? "",
    };
  }

  private expiryColumn(): DataColumn {
    return {
      id: "expiry",
      header: t("monitors.colExpiry"),
      accessor: (r) => (r as MonitorListItem).cert_expires_at ?? "9999",
      sortable: true,
      cell: (r) => this.expiryCell(r as MonitorListItem),
    };
  }

  // The member-only enable toggle and check-now action; viewers get neither (the
  // server re-checks both). The check-now column is dropped when no row in the table
  // would show the button (e.g. an ssl group with no open incidents), so there is no
  // empty Actions column.
  private actionColumns(rows: MonitorListItem[]): DataColumn[] {
    const cols: DataColumn[] = [];
    if (can(this.ctx?.role ?? null, "monitor.write")) {
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
    if (
      can(this.ctx?.role ?? null, "monitor.test") &&
      rows.some((m) => this.checkNowVisible[m.type](m))
    ) {
      cols.push({
        id: "actions",
        header: t("monitors.colActions"),
        cell: (r) => {
          const m = r as MonitorListItem;
          if (!this.checkNowVisible[m.type](m)) return "";
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
