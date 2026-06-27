import { html } from "lit";
import { customElement, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { can } from "../state/can.js";
import { t, tDynamic, type MessageKey } from "../i18n.js";
import { toast, toastError } from "../toast.js";
import { toastCheckError } from "../check-now.js";
import { formatDuration, formatLatency, secondsUntil } from "../format.js";
import type { MonitorListItem, MonitorType } from "../api/types.js";

import { icon } from "../icons.js";
import "./status-badge.js";
import "./upsell-banner.js";
import "./data-table.js";
import "./relative-time.js";
import type { DataColumn } from "./data-table.js";

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
  // client-side filter text, driven by the masthead field (focused with ⌘K)
  @state() private filter = "";
  private loadedOrgId: string | null = null;

  override connectedCallback(): void {
    super.connectedCallback();
    document.addEventListener("keydown", this.onKeydown);
  }

  override disconnectedCallback(): void {
    document.removeEventListener("keydown", this.onKeydown);
    super.disconnectedCallback();
  }

  // ⌘K / Ctrl-K focuses the filter field, like the command affordance in the masthead.
  private readonly onKeydown = (e: KeyboardEvent): void => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
      const input = this.querySelector<HTMLInputElement>("#monitor-filter");
      if (input) {
        e.preventDefault();
        input.focus();
      }
    }
  };

  // The list narrowed by the filter text (name or url, case-insensitive). The hero
  // stats stay on the full fleet; only the rows below are filtered.
  private filtered(): MonitorListItem[] {
    const all = this.monitors ?? [];
    const q = this.filter.trim().toLowerCase();
    if (!q) return all;
    return all.filter(
      (m) =>
        m.name.toLowerCase().includes(q) || m.url.toLowerCase().includes(q),
    );
  }

  // Load when the active org first appears or changes (e.g. via the switcher).
  override updated(): void {
    const orgId = this.ctx?.activeOrg?.org_id ?? null;
    if (orgId && orgId !== this.loadedOrgId) void this.load(orgId);
  }

  private async load(orgId: string): Promise<void> {
    this.loadedOrgId = orgId;
    this.loading = true;
    this.error = null;
    try {
      this.monitors = await client.listMonitors(orgId);
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

  // Run an on-demand check for one row. The server accepts with 202; we just
  // confirm it was queued. 409 and 429 surface as toasts (see toastCheckError).
  private async onCheckNow(m: MonitorListItem): Promise<void> {
    const orgId = this.ctx?.activeOrg?.org_id;
    if (!orgId || this.busyId) return;
    this.busyId = m.id;
    try {
      await client.checkNow(orgId, m.id);
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
      toastError(err, t("state.error"));
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
      return html`<button class="pulse-btn" disabled>
        ${icon("plus", "size-4")}${t("monitors.new")}
      </button>`;
    }
    return html`<a class="pulse-btn" href=${`${this.base}/monitors/new`}>
      ${icon("plus", "size-4")}${t("monitors.new")}
    </a>`;
  }

  // The full-bleed sections break out of the shell's padded content column
  // (-mx/-my match its px-6 lg:px-10 / py-7) so the masthead meets the folio and the
  // hero bands span edge to edge, broadsheet style.
  override render() {
    const ms = this.monitors;
    return html`
      <div class="-mx-6 lg:-mx-10 -my-7">
        ${this.masthead()} ${ms && ms.length ? this.hero(ms) : ""}
        <div class="px-6 lg:px-10 py-7 flex flex-col gap-6">
          ${this.atCap && can(this.ctx?.role ?? null, "monitor.write")
            ? html`<upsell-banner
                .upgradeHref=${`${this.base}/billing`}
              ></upsell-banner>`
            : ""}
          ${ms && ms.some((m) => m.incident_open)
            ? this.incidentBanner(ms)
            : ""}
          ${this.body()}
        </div>
      </div>
    `;
  }

  private masthead() {
    return html`<div
      class="flex flex-wrap items-end justify-between gap-5 px-6 lg:px-10 pt-7 lg:pt-[30px] pb-6 lg:pb-[26px] border-b border-line"
    >
      <h1
        class="m-0 font-disp font-black uppercase tracking-[-0.045em] leading-[0.82] text-[40px] lg:text-[52px]"
      >
        ${t("monitors.heading")}
      </h1>
      <div class="flex items-center gap-3">
        <label class="pulse-cmdk w-full sm:w-[260px]">
          <input
            id="monitor-filter"
            type="text"
            class="min-w-0 flex-1 border-0 bg-transparent p-0 font-sans text-ink outline-none placeholder:text-ink3"
            placeholder=${t("monitors.filterPlaceholder")}
            .value=${this.filter}
            @input=${(e: Event) =>
              (this.filter = (e.target as HTMLInputElement).value)}
          />
          <span class="pulse-kbd">⌘K</span>
        </label>
        ${this.newMonitorButton()}
      </div>
    </div>`;
  }

  // Fleet glance from real counts only (the list payload has no 30-day uptime or a
  // latency series, so nothing is fabricated): total, the up/down/degraded split,
  // open incidents, and certs expiring within two weeks.
  private hero(ms: MonitorListItem[]) {
    const up = ms.filter((m) => m.status === "up").length;
    const down = ms.filter((m) => m.status === "down").length;
    const degraded = ms.filter((m) => m.status === "coverage-degraded").length;
    const incidents = ms.filter((m) => m.incident_open).length;
    const sslSoon = ms.filter((m) => {
      if (!m.cert_expires_at) return false;
      const days = Math.floor(
        (new Date(m.cert_expires_at).getTime() - Date.now()) /
          (24 * 60 * 60 * 1000),
      );
      return days >= 0 && days <= 14;
    }).length;

    const mini = (label: string, value: number, cls = "") => html`<div>
      <div class="font-mono text-[10px] tracking-[0.1em] uppercase text-ink3">
        ${label}
      </div>
      <div
        class="font-disp font-extrabold text-[26px] tracking-[-0.03em] mt-1.5 ${cls}"
      >
        ${String(value).padStart(2, "0")}
      </div>
    </div>`;

    return html`<div
      class="grid grid-cols-1 lg:grid-cols-[1.05fr_1fr] border-b border-line"
    >
      <div
        class="relative overflow-hidden bg-paper px-6 lg:px-10 pt-[34px] pb-[30px] border-line lg:border-r"
      >
        <div
          class="absolute -right-9 -bottom-9 size-36 rotate-45 bg-brand opacity-[0.12]"
          aria-hidden="true"
        ></div>
        <div class="relative">
          <div
            class="font-mono text-[10.5px] tracking-[0.16em] uppercase text-ink3"
          >
            ${t("monitors.fleetLabel")}
          </div>
          <div
            class="font-disp font-black leading-[0.84] tracking-[-0.05em] text-[64px] lg:text-[96px] mt-2.5"
          >
            ${ms.length}<span class="text-[30px] text-ink3">
              ${t("monitors.monitorsUnit")}</span
            >
          </div>
          <div
            class="font-mono text-[11px] mt-[18px] text-ink2 flex flex-wrap gap-x-[18px] gap-y-1"
          >
            <span class="text-up">${up} ${t("monitors.statUp")}</span>
            <span class=${down ? "text-down" : ""}
              >${down} ${t("monitors.statDown")}</span
            >
            <span class=${degraded ? "text-deg" : ""}
              >${degraded} ${t("monitors.statDegraded")}</span
            >
          </div>
        </div>
      </div>
      <div class="flex items-center px-6 lg:px-10 py-[30px]">
        <div class="grid w-full grid-cols-2 sm:grid-cols-4 gap-8">
          ${mini(t("monitors.statDown"), down, down ? "text-down" : "")}
          ${mini(
            t("monitors.statDegraded"),
            degraded,
            degraded ? "text-deg" : "",
          )}
          ${mini(
            t("monitors.statIncidents"),
            incidents,
            incidents ? "text-down" : "",
          )}
          ${mini(t("monitors.statSslSoon"), sslSoon, sslSoon ? "text-deg" : "")}
        </div>
      </div>
    </div>`;
  }

  private incidentBanner(ms: MonitorListItem[]) {
    const n = ms.filter((m) => m.incident_open).length;
    return html`<div class="pulse-hazard flex items-center gap-[18px]">
      <span class="font-disp font-black text-[32px] leading-none text-down"
        >!</span
      >
      <div class="flex-1">
        <div
          class="font-disp font-extrabold text-[16px] uppercase tracking-[-0.01em]"
        >
          ${tDynamic("monitors.incidentBanner", "", { n })}
        </div>
      </div>
      <a class="pulse-btn pulse-btn-ghost" href=${`${this.base}/incidents`}
        >${t("monitors.incidentBannerCta")}</a
      >
    </div>`;
  }

  private body() {
    if (this.loading && this.monitors === null) {
      return html`<div class="flex flex-col gap-px" aria-busy="true">
        ${Array.from({ length: 6 }).map(
          () => html`<div class="h-12 w-full bg-paper animate-pulse"></div>`,
        )}
      </div>`;
    }

    if (this.error) {
      return html`<div
        role="alert"
        class="flex items-center justify-between gap-3 border border-down px-4 py-3 text-down"
      >
        <span>${this.error}</span>
        <button class="pulse-btn pulse-btn-ghost pulse-btn-sm" @click=${this.retry}>
          ${t("state.retry")}
        </button>
      </div>`;
    }

    if (!this.monitors || this.monitors.length === 0) {
      return html`<div
        class="border border-dashed border-hair p-12 flex flex-col items-center text-center gap-3"
      >
        <span class="text-brand">${icon("activity", "size-10")}</span>
        <div>
          <p
            class="font-disp font-extrabold text-lg uppercase tracking-[-0.02em]"
          >
            ${t("monitors.empty")}
          </p>
          <p class="text-ink3 mt-1">${t("monitors.emptyHint")}</p>
        </div>
        ${can(this.ctx?.role ?? null, "monitor.write") && !this.atCap
          ? html`<a class="pulse-btn" href=${`${this.base}/monitors/new`}
              >${icon("plus", "size-4")}${t("monitors.new")}</a
            >`
          : ""}
      </div>`;
    }

    // Group by check type so http and ssl monitors read as distinct sections, each
    // with its own table (and its own sort/paging). The filter narrows the rows
    // first; a filter that matches nothing shows a short note instead of the
    // create-your-first empty state.
    const visible = this.filtered();
    if (visible.length === 0) {
      return html`<p class="font-mono text-[12px] text-ink3 py-6">
        ${tDynamic("monitors.noMatch", "No monitors match your filter.")}
      </p>`;
    }
    const groups = TYPE_ORDER.map((type) => ({
      type,
      rows: visible.filter((m) => m.type === type),
    })).filter((g) => g.rows.length > 0);

    return html`<div class="flex flex-col gap-6">
      ${groups.map(
        (g) => html`<section class="flex flex-col gap-2.5">
          <div
            class="flex items-baseline justify-between border-b border-line pb-[11px]"
          >
            <h2
              class="pulse-section-title m-0"
            >
              ${t(TYPE_LABEL[g.type])}
            </h2>
            <span class="font-mono text-[11px] text-ink3"
              >${String(g.rows.length).padStart(2, "0")}</span
            >
          </div>
          <data-table
            .columns=${this.columns(g.type, g.rows)}
            .data=${g.rows}
            .pageSize=${15}
            .indexed=${true}
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
    return html`<div class="flex flex-col leading-tight font-mono">
      <span>${when ?? "—"}</span>
      ${cadence
        ? html`<span class="text-ink3 text-xs">${cadence}</span>`
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
      class: "text-ink2",
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
    if (!m.cert_expires_at) return html`<span class="text-ink3">—</span>`;
    const days = Math.floor(
      (new Date(m.cert_expires_at).getTime() - Date.now()) / (24 * 60 * 60 * 1000),
    );
    if (days < 0) {
      return html`<span
        class="font-mono text-xs font-bold uppercase text-down whitespace-nowrap"
        >${t("monitor.certExpired")}</span
      >`;
    }
    const cls = days <= 7 ? "text-deg" : "text-up";
    return html`<span class="font-mono text-xs whitespace-nowrap ${cls}"
      >${days} ${t("monitor.certDaysLeft")}</span
    >`;
  }

  // The columns each check type shows in its own table. The shared identity/status
  // columns lead and the member-only enable/actions trail; the type only decides the
  // middle. Picked by lookup (not a type branch): http shows regions / next-check /
  // latency, which mean nothing for a daily cert check, and ssl shows the cert expiry.
  private readonly typeColumns: Record<MonitorType, () => DataColumn[]> = {
    http: () => [
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
            class="font-disp font-extrabold text-[17px] tracking-[-0.025em] text-ink hover:text-brand hover:no-underline"
            href=${`${base}/monitors/${m.id}`}
            >${m.name}</a
          >${m.incident_open
            ? html`<span
                class="ml-2 inline-flex items-center gap-1 align-middle text-[10px] font-bold uppercase tracking-[0.04em] text-down"
                ><span class="inline-block size-2 bg-down"></span
                >${t("monitors.incident")}</span
              >`
            : ""}
          <div class="text-ink3 text-[11.5px] font-mono mt-[3px]">${m.url}</div>`;
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

  private nextCheckColumn(): DataColumn {
    return {
      id: "nextCheck",
      header: t("monitors.colNextCheck"),
      accessor: (r) => secondsUntil((r as MonitorListItem).next_check_at) ?? Infinity,
      sortable: true,
      class: "text-ink2 whitespace-nowrap",
      cell: (r) => this.nextCheckCell(r as MonitorListItem),
    };
  }

  private latencyColumn(): DataColumn {
    return {
      id: "latency",
      header: t("monitors.colLatency"),
      accessor: (r) => (r as MonitorListItem).last_latency_ms ?? 0,
      sortable: true,
      class: "font-mono text-ink2",
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
            class="size-4 cursor-pointer align-middle accent-brand disabled:opacity-40"
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
            class="inline-flex items-center gap-1 pulse-tag hover:text-ink disabled:opacity-40"
            ?disabled=${busy}
            title=${t("monitor.checkNow")}
            @click=${() => this.onCheckNow(m)}
          >
            ${busy
              ? html`<span
                  class="inline-block size-3 animate-spin rounded-full border border-current border-t-transparent"
                ></span>`
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
