import { html } from "lit";
import { customElement } from "lit/decorators.js";
import { t, type MessageKey } from "../i18n.js";
import { formatDateTime } from "../format.js";
import type { CoverageStatus } from "../api/types.js";
import { MonitorDetailBase } from "./monitor-detail-base.js";

// The ssl monitor detail (BACKLOG: SSL-expiry). A TLS-cert check has none of the
// http view's uptime / latency / per-region / recent-checks shape, so it shows its
// own thing: the certificate card (issued to/by, validity, SANs, days to expiry)
// and the incident timeline. The shared header / check-now / delete live in
// MonitorDetailBase.
@customElement("ssl-monitor-detail")
export class SslMonitorDetail extends MonitorDetailBase {
  // ssl carries no extra per-check data: the latest cert rides on the monitor and
  // incidents are loaded by the base. So loadData is a no-op.

  protected override currentStatus(): CoverageStatus {
    if (!this.monitor.enabled) return "disabled";
    // An expiring/expired/invalid cert opens an incident; an open one means down.
    return this.incidents.some((i) => i.ended_at === null) ? "down" : "up";
  }

  // A daily cert check is not worth re-running on demand; only offer check-now to
  // confirm a fix and clear an open incident.
  protected override showCheckNow(): boolean {
    return this.incidents.some((i) => i.ended_at === null);
  }

  protected override headerSubtitle() {
    return html`<span class="badge badge-ghost badge-sm">TLS</span>
      <span class="truncate">${this.monitor.url}</span>`;
  }

  protected override body() {
    return html`${this.certCard()} ${this.incidentsCard()}`;
  }

  // The certificate card. cert is the latest leaf detail from getMonitor, null until
  // the first ssl check records one.
  private certCard() {
    const cert = this.monitor.cert;
    return html`
      <div class="card bg-base-100 border border-base-300 shadow-sm">
        <div class="card-body gap-4 p-5">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <h2 class="font-semibold">${t("monitor.certTitle")}</h2>
            <span class="text-xs text-base-content/50">${t("monitor.certCheckCadence")}</span>
          </div>
          ${cert == null
            ? html`<p class="text-base-content/60">${t("monitor.certNone")}</p>`
            : html`
                <div class="flex items-center gap-2">
                  ${this.certExpiryBadge(cert.not_after)}
                </div>
                <dl class="grid grid-cols-1 sm:grid-cols-2 gap-x-6 gap-y-2 text-sm">
                  ${this.certRow("monitor.certIssuedTo", cert.subject)}
                  ${this.certRow("monitor.certIssuedBy", cert.issuer)}
                  ${this.certRow("monitor.certValidFrom", formatDateTime(cert.not_before) ?? "")}
                  ${this.certRow("monitor.certValidTo", formatDateTime(cert.not_after) ?? "")}
                  ${this.certRow("monitor.certSerial", cert.serial)}
                  ${cert.dns_names.length
                    ? this.certRow("monitor.certSans", cert.dns_names.join(", "))
                    : ""}
                </dl>
              `}
        </div>
      </div>
    `;
  }

  private certRow(labelKey: MessageKey, value: string) {
    if (!value) return "";
    return html`<div class="flex flex-col">
      <dt class="text-base-content/60">${t(labelKey)}</dt>
      <dd class="font-medium break-all">${value}</dd>
    </div>`;
  }

  private certExpiryBadge(notAfter: string) {
    const ms = new Date(notAfter).getTime() - Date.now();
    const days = Math.floor(ms / (24 * 60 * 60 * 1000));
    if (days < 0) {
      return html`<span class="badge badge-error badge-soft">${t("monitor.certExpired")}</span>`;
    }
    const cls = days <= 7 ? "badge-warning" : "badge-success";
    return html`<span class="badge ${cls} badge-soft"
      >${days} ${t("monitor.certDaysLeft")}</span
    >`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "ssl-monitor-detail": SslMonitorDetail;
  }
}
