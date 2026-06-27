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
import { spinner } from "./ui.js";
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
  // the enable toggle in the header is in flight, so it shows as busy and cannot
  // be double-fired.
  @state() protected toggling = false;
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
    return html`<span
        class="pulse-tag"
        >${m.method}</span
      >
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

  // Flip the monitor's enabled flag from the header switch. The API has no partial
  // update, so we PUT the whole monitor (which we already hold) back with enabled
  // toggled and keep the returned copy.
  protected async onToggleEnabled(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId || this.toggling) return;
    this.toggling = true;
    const m = this.monitor;
    const next = !m.enabled;
    try {
      this.monitor = await client.updateMonitor(orgId, m.id, {
        type: m.type,
        name: m.name,
        url: m.url,
        method: m.method,
        headers: m.headers,
        body: m.body,
        expected_status_codes: m.expected_status_codes,
        timeout_seconds: m.timeout_seconds,
        interval_seconds: m.interval_seconds,
        enabled: next,
        max_latency_ms: m.max_latency_ms,
        body_contains: m.body_contains,
        failure_threshold: m.failure_threshold,
        notification_channel_ids: m.notification_channel_ids,
        regions: m.regions,
        down_policy: m.down_policy,
      });
      toast(t(next ? "monitors.enabled" : "monitors.disabled"), "success");
    } catch (err) {
      toastError(err, t("state.error"));
    } finally {
      this.toggling = false;
    }
  }

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
      <div class="-mx-6 lg:-mx-10 -my-7">
        ${this.header()} ${this.instrumentBand()}
        <div class="px-6 lg:px-10 py-7 flex flex-col gap-6">${this.body()}</div>
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

  // The full-bleed instrument band sits right under the header (the stat
  // centerpiece): http shows uptime + latency, ssl shows the cert countdown.
  // Default none, so a subclass that has nothing to show just renders the header.
  protected instrumentBand(): TemplateResult | string {
    return "";
  }

  // Editorial header band, full-bleed: the monitor name set huge in Archivo, the
  // method + url in mono underneath with a status marker, and the actions (enable
  // toggle, check-now, edit, delete) on the right.
  protected header() {
    const m = this.monitor;
    const role = this.ctx?.role ?? null;
    const member = can(role, "monitor.write");
    return html`
      <div
        class="flex flex-wrap items-end justify-between gap-5 px-6 lg:px-10 pt-8 lg:pt-[34px] pb-6 border-b border-line"
      >
        <div class="min-w-0 flex flex-col gap-3">
          <div class="flex flex-wrap items-center gap-3.5">
            <h1
              class="m-0 font-disp font-black uppercase tracking-[-0.045em] leading-[0.82] text-[34px] lg:text-[52px] truncate"
            >
              ${m.name}
            </h1>
            <status-badge .status=${this.currentStatus()}></status-badge>
          </div>
          <div
            class="font-mono text-[12px] text-ink2 flex items-center gap-2.5 min-w-0"
          >
            ${this.headerSubtitle()}
          </div>
        </div>
        <div class="flex items-center gap-2">
          ${member
            ? html`<label
                class="flex items-center gap-2 px-1 select-none cursor-pointer"
                title=${t("monitors.toggleEnabled")}
              >
                <span class="pulse-label">${t("monitor.active")}</span>
                <input
                  type="checkbox"
                  class="size-4 cursor-pointer align-middle accent-brand disabled:opacity-40"
                  aria-label=${t("monitors.toggleEnabled")}
                  .checked=${m.enabled}
                  ?disabled=${this.toggling}
                  @change=${() => this.onToggleEnabled()}
                />
              </label>`
            : ""}
          ${can(role, "monitor.test") && this.showCheckNow()
            ? html`<button
                class="pulse-btn pulse-btn-sm"
                ?disabled=${this.checking}
                @click=${this.onCheckNow}
              >
                ${this.checking
                  ? html`${spinner()} ${t("monitor.checking")}`
                  : html`${icon("refresh", "size-4")}${t("monitor.checkNow")}`}
              </button>`
            : ""}
          ${member
            ? html`<a
                  class="pulse-btn pulse-btn-ghost pulse-btn-sm"
                  href=${`${this.base}/monitors/${m.id}/edit`}
                  >${icon("edit", "size-4")}${t("monitor.edit")}</a
                ><button
                  class="pulse-btn pulse-btn-ghost pulse-btn-sm border-down text-down"
                  @click=${() => this.deleteDialog.open()}
                >
                  ${icon("trash", "size-4")}${t("monitor.delete")}
                </button>`
            : ""}
        </div>
      </div>
    `;
  }

  protected incidentsCard() {
    return html`
      <div class="border border-hair">
        <div class="p-5 flex flex-col gap-4">
          <h2
            class="m-0 pulse-section-title flex items-center gap-1"
          >
            ${t("monitor.incidentsTitle")}${fieldHelp(t("monitor.helpIncidents"))}
          </h2>
          ${this.incidents.length === 0
            ? html`<p class="text-ink3">${t("monitor.noIncidents")}</p>`
            : html`<ul class="flex flex-col gap-2 m-0 p-0 list-none">
                ${this.incidents.map((i) => this.incidentRow(i))}
              </ul>`}
        </div>
      </div>
    `;
  }

  private incidentRow(i: Incident) {
    const ongoing = i.ended_at === null;
    return html`
      <li class="flex flex-wrap items-center gap-3 border-l-2 border-down pl-3 py-1">
        ${ongoing
          ? html`<span class="pulse-state text-down"
              ><span class="pulse-state-sq bg-down"></span
              >${t("monitor.incidentOngoing")}</span
            >`
          : html`<span
              class="pulse-tag"
              >${formatDuration(i.duration_seconds)}</span
            >`}
        <span class="text-sm">
          ${t("monitor.incidentStarted")}:
          <relative-time .datetime=${i.started_at}></relative-time>
        </span>
        <span class="pulse-tag">
          ${t(FAILURE_LABEL[i.cause_reason])}
        </span>
      </li>
    `;
  }
}
