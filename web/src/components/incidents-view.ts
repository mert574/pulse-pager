import { html } from "lit";
import { customElement, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { t, type MessageKey } from "../i18n.js";
import { formatDuration } from "../format.js";
import type { FailureReason, Incident, MonitorListItem } from "../api/types.js";
import { icon } from "../icons.js";

import "./data-table.js";
import "./relative-time.js";
import type { DataColumn } from "./data-table.js";

const FAILURE_LABEL: Record<FailureReason, MessageKey> = {
  connection_error: "failure.connection_error",
  timeout: "failure.timeout",
  status_mismatch: "failure.status_mismatch",
  latency_exceeded: "failure.latency_exceeded",
  body_assertion_failed: "failure.body_assertion_failed",
  blocked_target: "failure.blocked_target",
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

  override render() {
    return html`
      <div class="flex flex-col gap-4">
        <h1 class="text-2xl font-bold">${t("incidents.heading")}</h1>
        ${this.body()}
      </div>
    `;
  }

  private body() {
    if (this.loading && this.incidents === null) {
      return html`<div class="flex flex-col gap-2" aria-busy="true">
        ${Array.from({ length: 6 }).map(
          () => html`<div class="skeleton h-12 w-full"></div>`,
        )}
      </div>`;
    }

    if (this.error && this.incidents === null) {
      return html`<div role="alert" class="alert alert-error">
        <span>${this.error}</span>
        <button class="btn btn-sm" @click=${this.retry}>
          ${t("state.retry")}
        </button>
      </div>`;
    }

    if (!this.incidents || this.incidents.length === 0) {
      return html`<div
        class="rounded-box border border-dashed border-base-300 p-12 flex flex-col items-center text-center gap-3"
      >
        <span class="text-primary/70">${icon("incident", "size-10")}</span>
        <div>
          <p class="font-semibold text-lg">${t("incidents.empty")}</p>
          <p class="text-base-content/60 mt-1">${t("incidents.emptyHint")}</p>
        </div>
      </div>`;
    }

    return html`
      <data-table
        .columns=${this.columns()}
        .data=${this.incidents}
        .pageSize=${15}
      ></data-table>
      ${this.nextCursor
        ? html`<div class="flex justify-center mt-2">
            <button
              class="btn btn-sm btn-ghost"
              ?disabled=${this.loadingMore}
              @click=${this.loadMore}
            >
              ${this.loadingMore
                ? html`<span class="loading loading-spinner loading-xs"></span>`
                : ""}${t("incidents.loadMore")}
            </button>
          </div>`
        : ""}
    `;
  }

  private monitorName(id: string): string {
    return this.monitorNames.get(id) ?? id;
  }

  private columns(): DataColumn[] {
    const base = this.base;
    return [
      {
        id: "monitor",
        header: t("incidents.colMonitor"),
        accessor: (r) => this.monitorName((r as Incident).monitor_id),
        sortable: true,
        cell: (r) => {
          const i = r as Incident;
          return html`<a
            class="link link-hover font-medium"
            href=${`${base}/incidents/${i.id}`}
            >${this.monitorName(i.monitor_id)}</a
          >`;
        },
      },
      {
        id: "started",
        header: t("incidents.colStarted"),
        accessor: (r) => (r as Incident).started_at,
        sortable: true,
        class: "whitespace-nowrap text-base-content/70",
        cell: (r) =>
          html`<relative-time
            .datetime=${(r as Incident).started_at}
          ></relative-time>`,
      },
      {
        id: "duration",
        header: t("incidents.colDuration"),
        accessor: (r) => (r as Incident).duration_seconds ?? Number.MAX_SAFE_INTEGER,
        sortable: true,
        cell: (r) => {
          const i = r as Incident;
          return i.ended_at === null
            ? html`<span class="badge badge-error badge-sm"
                >${t("incidents.ongoing")}</span
              >`
            : formatDuration(i.duration_seconds);
        },
      },
      {
        id: "cause",
        header: t("incidents.colCause"),
        accessor: (r) => (r as Incident).cause_reason,
        sortable: true,
        cell: (r) =>
          html`<span class="badge badge-ghost badge-sm"
            >${t(FAILURE_LABEL[(r as Incident).cause_reason])}</span
          >`,
      },
      {
        id: "status",
        header: t("incidents.colStatus"),
        accessor: (r) => ((r as Incident).ended_at === null ? 0 : 1),
        sortable: true,
        cell: (r) =>
          (r as Incident).ended_at === null
            ? html`<span class="badge badge-error badge-soft badge-sm"
                >${t("incidents.statusOpen")}</span
              >`
            : html`<span class="badge badge-ghost badge-sm"
                >${t("incidents.statusClosed")}</span
              >`,
      },
    ];
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "incidents-view": IncidentsView;
  }
}
