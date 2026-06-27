import { html, nothing } from "lit";
import { customElement, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { t, tDynamic, type MessageKey } from "../i18n.js";
import { formatDuration } from "../format.js";
import type { FailureReason, Incident, MonitorListItem } from "../api/types.js";
import { icon } from "../icons.js";
import {
  pageHeader,
  errorBox,
  emptyState,
  skeletonRows,
  spinner,
} from "./ui.js";

import "./relative-time.js";
import "./pulse-ledger.js";

const FAILURE_LABEL: Record<FailureReason, MessageKey> = {
  connection_error: "failure.connection_error",
  timeout: "failure.timeout",
  status_mismatch: "failure.status_mismatch",
  latency_exceeded: "failure.latency_exceeded",
  body_assertion_failed: "failure.body_assertion_failed",
  blocked_target: "failure.blocked_target",
  cert_expired: "failure.cert_expired",
  cert_expiring_soon: "failure.cert_expiring_soon",
  cert_invalid: "failure.cert_invalid",
};

// Org-wide incidents list (PRD-002 4). It shows every monitor's incidents newest
// first (all of them, the status column marks open vs closed). The Incident wire
// shape carries only monitor_id, so the monitor list is pulled alongside to resolve
// a display name per row. Cursor paging appends more rows via a "load more" button
// when next_cursor is present.
@customElement("incidents-view")
export class IncidentsView extends AppElement {
  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private incidents: Incident[] | null = null;
  @state() private monitorNames = new Map<string, string>();
  @state() private loading = false;
  @state() private error: string | null = null;
  @state() private nextCursor: string | null = null;
  @state() private loadingMore = false;

  // org id the current rows were loaded for, so an org switch triggers a fresh load.
  private loadedKey: string | null = null;

  private get orgId(): string | null {
    return this.ctx?.activeOrg?.org_id ?? null;
  }

  private get base(): string {
    return `/orgs/${this.orgId ?? ""}`;
  }

  override updated(): void {
    const orgId = this.orgId;
    if (orgId && orgId !== this.loadedKey) void this.load();
  }

  private async load(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId) return;
    this.loadedKey = orgId;
    this.loading = true;
    this.error = null;
    try {
      const [page, monitors] = await Promise.all([
        client.listIncidents(orgId),
        client.listMonitors(orgId),
      ]);
      this.incidents = page.items;
      this.nextCursor = page.next_cursor;
      this.monitorNames = new Map(
        monitors.map((m: MonitorListItem) => [m.id, m.name]),
      );
    } catch (err) {
      this.error = err instanceof ApiError ? err.message : t("state.error");
      this.incidents = null;
    } finally {
      this.loading = false;
    }
  }

  private retry(): void {
    this.loadedKey = null;
    void this.load();
  }

  private async loadMore(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId || !this.nextCursor || this.loadingMore) return;
    this.loadingMore = true;
    try {
      const page = await client.listIncidents(orgId, undefined, this.nextCursor);
      this.incidents = [...(this.incidents ?? []), ...page.items];
      this.nextCursor = page.next_cursor;
    } catch {
      // a failed "load more" leaves the existing rows in place; the user can retry
      this.error = t("state.error");
    } finally {
      this.loadingMore = false;
    }
  }

  private get openCount(): number {
    return (this.incidents ?? []).filter((i) => i.ended_at === null).length;
  }

  private monitorName(id: string): string {
    return this.monitorNames.get(id) ?? id;
  }

  // Full-bleed editorial ledger: the masthead, a slim operational-status strip (calm
  // when nothing is open, the hazard band with the active count when something is),
  // then indexed incident rows. Each row is the headline (the affected monitor and
  // its cause), the running or final duration, and an open/closed marker; open
  // incidents are escalated with a red index and red edge.
  override render() {
    const open = this.openCount;
    const count = this.incidents?.length ?? 0;
    return html`
      <div class="-mx-6 lg:-mx-10 -my-7">
        ${pageHeader(
          t("incidents.heading"),
          count
            ? html`<span
                class="font-mono text-[11px] uppercase tracking-[0.1em] text-ink3"
                >${tDynamic("incidents.shown", "{n} shown", { n: count })}</span
              >`
            : nothing,
        )}
        ${this.statusStrip(open)}
        <div class="px-6 lg:px-10 py-7">${this.body()}</div>
      </div>
    `;
  }

  // The slim status strip under the masthead. Nothing shows until the list has
  // loaded (so it is not a premature "all clear" while loading, or on an error).
  // Once loaded it is the hazard band when any incident is open, or a thin calm
  // "all operational" band when none are.
  private statusStrip(open: number) {
    if (this.incidents === null) return nothing;
    return open > 0 ? this.hazardBand(open) : this.operationalBand();
  }

  // A thin calm band: a small up-colored square and "All systems operational".
  private operationalBand() {
    return html`<div
      class="flex items-center gap-3 px-6 lg:px-10 py-3.5 border-b border-line"
    >
      <span class="size-2.5 shrink-0 bg-up" aria-hidden="true"></span>
      <span
        class="font-disp font-extrabold text-[14px] uppercase tracking-[-0.01em] text-up"
        >${tDynamic("incidents.allOperational", "All systems operational", {})}</span
      >
    </div>`;
  }

  private hazardBand(n: number) {
    return html`<div
      class="pulse-hazard flex items-center gap-4 px-6 lg:px-10 pb-6 border-b border-line"
    >
      <span class="font-disp font-black text-[40px] leading-none text-down"
        >${String(n).padStart(2, "0")}</span
      >
      <div>
        <div
          class="font-disp font-extrabold text-[15px] uppercase tracking-[-0.01em]"
        >
          ${tDynamic("incidents.hazardTitle", "Open incidents", {})}
        </div>
        <div class="font-mono text-[12px] text-ink2 mt-0.5">
          ${tDynamic("incidents.hazardSub", "Affecting your monitors now", {})}
        </div>
      </div>
    </div>`;
  }

  private body() {
    if (this.loading && this.incidents === null) {
      return skeletonRows();
    }

    if (this.error && this.incidents === null) {
      return errorBox(this.error, () => this.retry(), t("state.retry"));
    }

    if (!this.incidents || this.incidents.length === 0) {
      return emptyState(
        icon("incident", "size-10"),
        t("incidents.empty"),
        t("incidents.emptyHint"),
      );
    }

    return html`
      <pulse-ledger
        .items=${this.incidents}
        .renderRow=${(item: unknown, i: number) => this.row(item as Incident, i)}
      ></pulse-ledger>
      ${this.nextCursor
        ? html`<div class="flex justify-center mt-6">
            <button
              class="pulse-btn pulse-btn-ghost pulse-btn-sm"
              ?disabled=${this.loadingMore}
              @click=${this.loadMore}
            >
              ${this.loadingMore ? spinner() : ""}${t("incidents.loadMore")}
            </button>
          </div>`
        : ""}
    `;
  }

  private row(inc: Incident, i: number) {
    const open = inc.ended_at === null;
    const n = String(i + 1).padStart(2, "0");
    return html`<a
      href=${`${this.base}/incidents/${inc.id}`}
      data-incident-row
      class="grid grid-cols-[44px_1fr_auto] items-center gap-5 px-6 lg:px-10 py-5 border-b border-hair hover:no-underline ${open
        ? "border-l-2 border-l-down"
        : ""}"
    >
      <span
        class="font-mono text-[12px] font-medium ${open
          ? "text-down"
          : "text-brand"}"
        >${n}</span
      >
      <div class="min-w-0">
        <div class="flex items-center gap-2.5 flex-wrap">
          <span
            class="font-disp font-extrabold text-[17px] tracking-[-0.025em] text-ink truncate"
            >${this.monitorName(inc.monitor_id)}</span
          >
          <span class="pulse-tag">${t(FAILURE_LABEL[inc.cause_reason])}</span>
        </div>
        <div class="font-mono text-[11.5px] text-ink3 mt-1">
          ${tDynamic("incidents.sinceLabel", "since", {})}
          <relative-time .datetime=${inc.started_at}></relative-time>
        </div>
      </div>
      <div class="text-right">
        ${open
          ? html`<span class="pulse-state text-down justify-end"
              ><span class="pulse-state-sq bg-down"></span
              >${t("incidents.ongoing")}</span
            >`
          : html`<div class="font-mono text-[15px] text-ink">
                ${formatDuration(inc.duration_seconds)}
              </div>
              <div class="pulse-tag mt-0.5 text-ink3">
                ${t("incidents.statusClosed")}
              </div>`}
      </div>
    </a>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "incidents-view": IncidentsView;
  }
}
