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
    return html`<span
        class="pulse-tag"
        >TLS</span
      >
      <span class="truncate">${this.monitor.url}</span>`;
  }

  // The dominant figure band for an ssl monitor, full-bleed under the header: the
  // days-to-expiry as one very large Archivo countdown on the left (amber within two
  // weeks, red once expired, where it reads "Expired" instead of a number), with the
  // issuer and validity window as a subordinate spec block on the right. Only shown
  // once a cert was seen.
  protected override instrumentBand() {
    const cert = this.monitor.cert;
    if (cert == null) return "";
    const days = Math.floor(
      (new Date(cert.not_after).getTime() - Date.now()) / (24 * 60 * 60 * 1000),
    );
    const expired = days < 0;
    const tone = expired ? "text-down" : days <= 14 ? "text-deg" : "text-up";
    const numeral = expired
      ? html`<span class="text-down">${t("monitor.certExpired")}</span>`
      : html`<span class=${tone}>${days}</span
          ><span class="text-[0.26em] align-top text-ink3">
            ${t("monitor.certDaysUnit")}</span
          >`;

    const spec: { label: MessageKey; value: string }[] = [
      { label: "monitor.certIssuedBy", value: cert.issuer },
      { label: "monitor.certIssuedTo", value: cert.subject },
      {
        label: "monitor.certValidFrom",
        value: formatDateTime(cert.not_before) ?? "—",
      },
      { label: "monitor.certValidTo", value: formatDateTime(cert.not_after) ?? "—" },
    ];

    return html`
      <section class="border-b border-line">
        <div class="grid grid-cols-1 lg:grid-cols-[minmax(0,1fr)_1.2fr]">
          <div
            class="flex flex-col justify-center gap-3 px-6 lg:px-10 pt-8 pb-7 border-line lg:border-r"
          >
            <div class="pulse-label">${t("monitor.certExpiresIn")}</div>
            <div
              class="font-disp font-black leading-[0.82] tracking-[-0.05em] text-7xl lg:text-8xl"
            >
              ${numeral}
            </div>
            <div class="font-mono text-[12px] text-ink2">
              ${formatDateTime(cert.not_after) ?? ""}
            </div>
          </div>
          <div
            class="grid grid-cols-1 sm:grid-cols-2 gap-x-8 gap-y-5 content-center px-6 lg:px-10 py-7 bg-paper"
          >
            ${spec.map(
              (c) => html`<div class="min-w-0">
                <div class="pulse-label">${t(c.label)}</div>
                <div
                  class="font-disp font-bold text-[17px] tracking-[-0.02em] mt-1 break-all"
                >
                  ${c.value}
                </div>
              </div>`,
            )}
          </div>
        </div>
      </section>
    `;
  }

  protected override body() {
    return html`${this.certCard()} ${this.incidentsCard()}`;
  }

  // The certificate detail card. The expiry countdown and the issuer/validity live
  // in the instrument band above; this carries the serial and the alternative names,
  // and the empty state when no cert has been seen yet. cert is the latest leaf
  // detail from getMonitor, null until the first ssl check records one.
  private certCard() {
    const cert = this.monitor.cert;
    return html`
      <div class="pulse-panel p-5 flex flex-col gap-4">
        <div class="flex flex-wrap items-center justify-between gap-2">
          <h2 class="m-0 pulse-section-title">${t("monitor.certTitle")}</h2>
          <span class="font-mono text-[11px] text-ink3"
            >${t("monitor.certCheckCadence")}</span
          >
        </div>
        ${cert == null
          ? html`<p class="font-mono text-[12px] text-ink3">
              ${t("monitor.certNone")}
            </p>`
          : html`<dl class="grid grid-cols-1 sm:grid-cols-2 gap-x-6 gap-y-3 text-sm">
              ${this.certRow("monitor.certSerial", cert.serial)}
              ${cert.dns_names.length
                ? this.certRow("monitor.certSans", cert.dns_names.join(", "))
                : ""}
            </dl>`}
      </div>
    `;
  }

  private certRow(labelKey: MessageKey, value: string) {
    if (!value) return "";
    return html`<div class="flex flex-col gap-0.5">
      <dt class="font-mono text-[11px] uppercase tracking-[0.08em] text-ink3">
        ${t(labelKey)}
      </dt>
      <dd class="m-0 font-medium break-all">${value}</dd>
    </div>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "ssl-monitor-detail": SslMonitorDetail;
  }
}
