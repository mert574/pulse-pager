import { html, type TemplateResult } from "lit";
import { property, state, query } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { can } from "../state/can.js";
import { navigate } from "../router.js";
import { t, type MessageKey } from "../i18n.js";
import { toast, toastError } from "../toast.js";
import { toastCheckError } from "../check-now.js";
import { formatDuration } from "../format.js";
import type {
  CheckNowAccepted,
  CoverageStatus,
  FailureReason,
  Incident,
  Monitor,
} from "../api/types.js";
import { icon, fieldHelp } from "../icons.js";
import type { ConfirmDialog } from "./confirm-dialog.js";

import "./status-badge.js";
import "./relative-time.js";
import "./confirm-dialog.js";

// Failure-reason labels, shared by the incident timeline here and the per-check
// badges in the http view.
export const FAILURE_LABEL: Record<FailureReason, MessageKey> = {
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

// MonitorDetailBase is the shared shell behind the per-type detail views
// (http-monitor-detail, ssl-monitor-detail). It owns everything that does not
// depend on the check type: the loaded monitor (passed in by the dispatcher), the
// incident timeline, the header with check-now / edit / delete, and the delete
// dialog. Each subclass loads its own type-specific data in loadData() and renders
// the type-specific middle in body(). This is NOT a registered element.
export abstract class MonitorDetailBase extends AppElement {
  // The monitor is loaded once by monitor-detail-view (the route element) and
  // handed down, so a subclass never refetches it.
  @property({ attribute: false }) monitor!: Monitor;

  @consume({ context: appContext, subscribe: true })
  protected ctx!: AppContext;

  @state() protected incidents: Incident[] = [];
  @state() protected checking = false;
  // Errors from the subclass/incident data load (the monitor itself is loaded by
  // the dispatcher, which shows its own error state).
  @state() protected loadError: string | null = null;

  @query("confirm-dialog") private deleteDialog!: ConfirmDialog;

  private loadedId: string | null = null;

  // The detail page auto-refreshes its data every 10s while the tab is visible, so
  // new checks / incident changes show without a manual reload. Paused when hidden.
  private refreshTimer: number | null = null;
  private readonly onRefreshVisibility = () => this.syncRefresh();

  override connectedCallback(): void {
    super.connectedCallback();
    document.addEventListener("visibilitychange", this.onRefreshVisibility);
    this.syncRefresh();
  }

  override disconnectedCallback(): void {
    document.removeEventListener("visibilitychange", this.onRefreshVisibility);
    this.stopRefresh();
    super.disconnectedCallback();
  }

  private syncRefresh(): void {
    this.stopRefresh();
    if (document.visibilityState === "visible") {
      this.refreshTimer = window.setInterval(() => void this.loadShared(), 10_000);
    }
  }

  private stopRefresh(): void {
    if (this.refreshTimer !== null) {
      window.clearInterval(this.refreshTimer);
      this.refreshTimer = null;
    }
  }

  protected get orgId(): string | null {
    return this.ctx?.activeOrg?.org_id ?? null;
  }
  protected get base(): string {
    return `/orgs/${this.orgId ?? ""}`;
  }

  // Reload when the monitor changes (first mount, or navigating between monitors
  // where the dispatcher reuses this element with a new monitor property).
  override updated(): void {
    if (this.monitor && this.monitor.id !== this.loadedId) {
      this.loadedId = this.monitor.id;
      void this.loadShared();
    }
  }

  private async loadShared(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId || !this.monitor) return; // the auto-refresh can fire before the prop lands
    this.loadError = null;
    try {
      const incidents = await client.listMonitorIncidents(orgId, this.monitor.id);
      this.incidents = incidents.items ?? [];
      await this.loadData();
    } catch (err) {
      this.loadError = err instanceof ApiError ? err.message : t("state.error");
    }
  }

  // Subclass hook: load the type-specific data (results, snapshot, region poll for
  // http; nothing extra for ssl). Runs after the shared incident load.
  protected loadData(): void | Promise<void> {}

  // The status badge in the header. http derives it from the latest check; ssl from
  // the open incident. Subclasses decide.
  protected abstract currentStatus(): CoverageStatus;

  // The type-specific middle of the page, between the header and the delete dialog.
  protected abstract body(): TemplateResult | string;

  // The line under the title. http shows the method + url; ssl overrides to a TLS
  // badge + host.
  protected headerSubtitle(): TemplateResult {
    const m = this.monitor;
    return html`<span class="badge badge-ghost badge-sm">${m.method}</span>
      <span class="truncate">${m.url}</span>`;
  }

  // The server accepts a check with 202; the subclass may react (http shows the
  // optimistic per-region chips and starts polling). 409/429 surface as toasts.
  protected async onCheckNow(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId) return;
    this.checking = true;
    try {
      const accepted = await client.checkNow(orgId, this.monitor.id);
      this.afterCheckAccepted(accepted);
      toast(t("monitor.checkQueued"), "info");
    } catch (err) {
      toastCheckError(err);
    } finally {
      this.checking = false;
    }
  }

  // Subclass hook fired after a check-now is accepted (http kicks the poll).
  protected afterCheckAccepted(_accepted: CheckNowAccepted): void {}

  // Whether the header offers a manual check-now (gated further by the test role).
  // Always for http; ssl overrides to only while an incident is open, since a daily
  // cert check is not worth re-running on demand otherwise.
  protected showCheckNow(): boolean {
    return true;
  }

  private async onDeleteConfirmed(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId) return;
    try {
      await client.deleteMonitor(orgId, this.monitor.id);
      toast(t("monitor.deleted"), "success");
      navigate(this.base);
    } catch (err) {
      toastError(err, t("state.error"));
    }
  }

  override render() {
    return html`
      <div class="flex flex-col gap-6">${this.header()} ${this.body()}</div>
      <confirm-dialog
        .heading=${t("monitor.deleteHeading")}
        .message=${t("monitor.deleteMessage")}
        .confirmLabel=${t("monitor.delete")}
        ?danger=${true}
        @confirm=${this.onDeleteConfirmed}
      ></confirm-dialog>
    `;
  }

  protected header() {
    const m = this.monitor;
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
            ${this.headerSubtitle()}
          </div>
        </div>
        <div class="flex items-center gap-2">
          ${member
            ? html`<button
                class="btn btn-sm btn-ghost text-error gap-1.5"
                @click=${() => this.deleteDialog.open()}
              >
                ${icon("trash", "size-4")}${t("monitor.delete")}
              </button>
              <a
                class="btn btn-sm gap-1.5"
                href=${`${this.base}/monitors/${m.id}/edit`}
                >${icon("edit", "size-4")}${t("monitor.edit")}</a
              >`
            : ""}
          ${can(role, "monitor.test") && this.showCheckNow()
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
        </div>
      </div>
    `;
  }

  protected incidentsCard() {
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
