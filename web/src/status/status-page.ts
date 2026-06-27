import { html, nothing, type TemplateResult } from "lit";
import { customElement, state } from "lit/decorators.js";
import { AppElement } from "../components/base.js";
import { t, tDynamic, type MessageKey } from "../i18n.js";
import {
  fetchPublicStatusPage,
  resolveSlug,
  PublicFetchError,
} from "./public-client.js";
import type {
  PublicDisplayedMonitor,
  PublicIncident,
  PublicStatusPage as PublicStatusPageData,
  PublicUptime,
  StatusPageTheme,
} from "../api/types.js";

import "../components/uptime-bar.js";
import type { UptimeBar } from "../components/uptime-bar.js";

type LoadState =
  | { kind: "loading" }
  | { kind: "ready"; data: PublicStatusPageData }
  | { kind: "notFound" }
  | { kind: "error" };

// The status page's chosen theme maps onto the two themes the build ships.
const THEME_NAME: Record<StatusPageTheme, string> = {
  light: "caramellatte",
  dark: "coffee",
};

// Public status page (PRD-004 3.6, RFC-013 section 8.2). Fetches the public-safe
// projection for the slug (from the subdomain or ?slug=) and renders branding, the
// overall banner, each displayed monitor's friendly name + reduced status + uptime
// summary + recent history strip, and past public incidents. It renders ONLY what
// the public endpoint returns, so no internal URLs, methods, or failure detail can
// ever appear here. A 404 (unknown or unpublished slug) shows a plain not-found.
// A public status page should keep itself current without a manual reload, so it
// refetches every 30s. The poll pauses while the tab is hidden and resumes (with an
// immediate refetch) when it comes back, so a backgrounded page does not keep hitting
// the endpoint.
const POLL_MS = 30_000;

@customElement("status-page")
export class StatusPage extends AppElement {
  @state() private status: LoadState = { kind: "loading" };

  private pollTimer: number | null = null;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.load();
    document.addEventListener("visibilitychange", this.onVisibility);
    this.startPoll();
  }

  override disconnectedCallback(): void {
    document.removeEventListener("visibilitychange", this.onVisibility);
    this.stopPoll();
    super.disconnectedCallback();
  }

  private onVisibility = (): void => {
    if (document.visibilityState === "visible") {
      void this.refresh();
      this.startPoll();
    } else {
      this.stopPoll();
    }
  };

  private startPoll(): void {
    if (this.pollTimer !== null || document.visibilityState !== "visible") return;
    this.pollTimer = window.setInterval(() => void this.refresh(), POLL_MS);
  }

  private stopPoll(): void {
    if (this.pollTimer !== null) {
      window.clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
  }

  private async load(): Promise<void> {
    this.status = { kind: "loading" };
    const slug = resolveSlug();
    if (!slug) {
      this.status = { kind: "notFound" };
      return;
    }
    try {
      const data = await fetchPublicStatusPage(slug);
      this.applyBranding(data);
      this.status = { kind: "ready", data };
    } catch (err) {
      if (err instanceof PublicFetchError && err.status === 404) {
        this.status = { kind: "notFound" };
      } else {
        this.status = { kind: "error" };
      }
    }
  }

  // Background refetch on the poll: it does NOT reset to the loading spinner, and a
  // failed tick keeps the page showing its last good data rather than blanking it
  // (a status page must degrade gracefully). A 404 still flips to not-found.
  private async refresh(): Promise<void> {
    const slug = resolveSlug();
    if (!slug) return;
    try {
      const data = await fetchPublicStatusPage(slug);
      this.applyBranding(data);
      this.status = { kind: "ready", data };
    } catch (err) {
      if (err instanceof PublicFetchError && err.status === 404) {
        this.status = { kind: "notFound" };
      }
      // other errors: keep the current view, try again next tick.
    }
  }

  // Apply the page's theme and accent at the document level so the color tokens and
  // the brand accent reach the whole page, not just this element's subtree.
  private applyBranding(data: PublicStatusPageData): void {
    document.documentElement.dataset.theme = THEME_NAME[data.theme];
    document.documentElement.style.setProperty("--brand-accent", data.accent_color);
    if (data.name) document.title = data.name;
  }

  override render() {
    switch (this.status.kind) {
      case "loading":
        return this.centered(
          html`<span
            class="inline-block size-8 animate-spin rounded-full border-2 border-current border-t-transparent text-ink3"
          ></span>`,
        );
      case "notFound":
        return this.message(
          t("publicStatus.notFoundTitle"),
          t("publicStatus.notFoundBody"),
        );
      case "error":
        return this.message(
          t("publicStatus.errorTitle"),
          t("publicStatus.errorBody"),
          true,
        );
      case "ready":
        return this.page(this.status.data);
    }
  }

  private centered(body: TemplateResult) {
    return html`<div class="min-h-screen flex items-center justify-center p-6">
      ${body}
    </div>`;
  }

  private message(title: string, body: string, retry = false) {
    return this.centered(html`<div
      class="border border-hair bg-bg max-w-md w-full p-8 flex flex-col items-center text-center gap-3"
    >
      <p
        class="font-disp font-black uppercase tracking-[-0.03em] leading-[0.9] text-[26px]"
      >
        ${title}
      </p>
      <p class="text-ink3">${body}</p>
      ${retry
        ? html`<button
            class="pulse-btn pulse-btn-sm mt-1"
            @click=${() => this.load()}
          >
            ${t("publicStatus.retry")}
          </button>`
        : nothing}
    </div>`);
  }

  private page(data: PublicStatusPageData) {
    return html`
      <div class="min-h-screen bg-bg">
        <div class="max-w-3xl mx-auto px-6 py-12 flex flex-col gap-10">
          ${this.header(data)} ${this.statusHeadline(data)}
          ${this.monitors(data.monitors)} ${this.incidents(data.incidents)}
          <footer
            class="border-t border-line pt-5 font-mono text-[10.5px] uppercase tracking-[0.16em] text-ink3"
          >
            ${t("publicStatus.poweredBy")}
          </footer>
        </div>
      </div>
    `;
  }

  // Branded masthead: the logo and the published name in the page's accent color.
  private header(data: PublicStatusPageData) {
    return html`<header class="flex items-center gap-3 border-b border-line pb-6">
      ${data.logo_url
        ? html`<img
            src=${data.logo_url}
            alt=""
            class="h-10 object-contain"
            referrerpolicy="no-referrer"
          />`
        : nothing}
      <span
        class="font-disp font-black uppercase tracking-[-0.04em] leading-[0.85] text-[26px] lg:text-[30px]"
        style="color: var(--brand-accent)"
        >${data.name}</span
      >
    </header>`;
  }

  // The big operational headline (RFC-013 section 9.1): when every displayed monitor
  // is up it reads "All systems operational" in the up tone; the moment one is down it
  // flips to "N monitor(s) down" in the down tone with the hazard stripe, so the worst
  // state is unmistakable beyond color alone. The mono foot keeps the exact count.
  private statusHeadline(data: PublicStatusPageData) {
    const total = data.monitors.length;
    const down = data.monitors.filter((m) => m.status === "down").length;
    const ops = total - down;
    const allOk = down === 0;
    const headline = allOk
      ? t("publicStatus.bannerOperational")
      : tDynamic(
          down === 1
            ? "publicStatus.headlineDownOne"
            : "publicStatus.headlineDownMany",
          down === 1 ? "{n} monitor down" : "{n} monitors down",
          { n: down },
        );
    const tone = allOk ? "text-up" : "text-down";
    const sq = allOk ? "bg-up" : "bg-down";
    return html`<section role="status" class=${allOk ? "" : "pulse-hazard"}>
      <div class="pulse-label">
        ${tDynamic("publicStatus.kicker", "Current status")}
      </div>
      <h1
        class="m-0 font-disp font-black uppercase tracking-[-0.045em] leading-[0.82] text-[44px] lg:text-[64px] mt-2 ${tone}"
      >
        ${headline}
      </h1>
      ${total
        ? html`<div
            class="font-mono text-[12px] text-ink2 mt-3.5 flex items-center gap-2.5"
          >
            <span class="pulse-state-sq ${sq}" aria-hidden="true"></span>
            ${tDynamic("publicStatus.operationalCount", "{up} of {total} operational", {
              up: ops,
              total,
            })}
          </div>`
        : nothing}
    </section>`;
  }

  // Monitor uptime ledger: one row per displayed monitor, headlined by a big uptime
  // numeral (the widest window that has data), a status marker, and the history strip.
  private monitors(monitors: PublicDisplayedMonitor[]) {
    if (monitors.length === 0) return nothing;
    return html`<section class="flex flex-col">
      <div class="flex items-baseline justify-between border-b border-line pb-3">
        <h2 class="pulse-section-title">
          ${tDynamic("publicStatus.monitorsHeading", "Monitors")}
        </h2>
        <span class="font-mono text-[11px] text-ink3"
          >${String(monitors.length).padStart(2, "0")}</span
        >
      </div>
      ${monitors.map((m) => this.monitorRow(m))}
    </section>`;
  }

  private monitorRow(m: PublicDisplayedMonitor) {
    const operational = m.status === "operational";
    const bars: UptimeBar[] = m.history.map((p) => ({ healthy: p.up }));
    const headline = this.headlineUptime(m.uptime);
    return html`<div
      class="grid grid-cols-[1fr_auto] items-start gap-5 py-5 border-b border-hair"
    >
      <div class="min-w-0 flex flex-col gap-3">
        <div class="flex items-center gap-2.5 flex-wrap">
          <span
            class="font-disp font-extrabold text-[18px] tracking-[-0.025em] truncate"
            >${m.display_name}</span
          >
          <span class="pulse-state ${operational ? "text-up" : "text-down"}">
            <span
              class="pulse-state-sq ${operational ? "bg-up" : "bg-down"}"
              aria-hidden="true"
            ></span>
            ${t(
              operational
                ? "publicStatus.statusOperational"
                : "publicStatus.statusDown",
            )}
          </span>
        </div>
        ${bars.length ? html`<uptime-bar .bars=${bars}></uptime-bar>` : nothing}
        ${this.uptimeSummary(m.uptime)}
      </div>
      <div class="text-right">
        <div
          class="font-disp font-black text-[32px] leading-none tracking-[-0.03em] ${operational
            ? "text-up"
            : "text-down"}"
        >
          ${headline}
        </div>
        <div class="pulse-label mt-2">
          ${tDynamic("publicStatus.uptimeLabel", "uptime")}
        </div>
      </div>
    </div>`;
  }

  // The widest uptime window that actually has data, as the row's big numeral. Falls
  // back to the no-value placeholder when the monitor is too new for any window.
  private headlineUptime(u: PublicUptime): string {
    if (u.has_90d) return `${u.uptime_90d.toFixed(2)}%`;
    if (u.has_7d) return `${u.uptime_7d.toFixed(2)}%`;
    if (u.has_24h) return `${u.uptime_24h.toFixed(2)}%`;
    return "—";
  }

  private uptimeSummary(u: PublicUptime) {
    const cell = (labelKey: MessageKey, has: boolean, value: number) => html`<div
      class="flex flex-col gap-0.5"
    >
      <span class="pulse-label">${t(labelKey)}</span>
      <span class="font-mono text-[13px] ${has ? "text-ink" : "text-ink3"}"
        >${has ? `${value.toFixed(2)}%` : t("publicStatus.noData")}</span
      >
    </div>`;
    return html`<div class="grid grid-cols-3 gap-4 max-w-xs">
      ${cell("publicStatus.uptime24h", u.has_24h, u.uptime_24h)}
      ${cell("publicStatus.uptime7d", u.has_7d, u.uptime_7d)}
      ${cell("publicStatus.uptime90d", u.has_90d, u.uptime_90d)}
    </div>`;
  }

  private incidents(incidents: PublicIncident[]) {
    return html`<section class="flex flex-col">
      <div class="flex items-baseline justify-between border-b border-line pb-3">
        <h2 class="pulse-section-title">${t("publicStatus.incidents")}</h2>
        <span class="font-mono text-[11px] text-ink3"
          >${String(incidents.length).padStart(2, "0")}</span
        >
      </div>
      ${incidents.length === 0
        ? html`<p class="font-mono text-[12px] text-ink3 py-5">
            ${t("publicStatus.noIncidents")}
          </p>`
        : html`<ul class="flex flex-col">
            ${incidents.map((inc) => this.incidentRow(inc))}
          </ul>`}
    </section>`;
  }

  private incidentRow(inc: PublicIncident) {
    const started = new Date(inc.started_at);
    const startLabel = Number.isNaN(started.getTime())
      ? inc.started_at
      : started.toLocaleString();
    return html`<li
      class="grid grid-cols-[1fr_auto] items-center gap-4 py-4 border-b border-hair"
    >
      <div class="min-w-0">
        <div class="font-disp font-extrabold text-[16px] tracking-[-0.02em] truncate">
          ${inc.display_name}
        </div>
        <div class="font-mono text-[11.5px] text-ink3 mt-1">${startLabel}</div>
      </div>
      <span class="pulse-state ${inc.resolved ? "text-up" : "text-down"}">
        <span
          class="pulse-state-sq ${inc.resolved ? "bg-up" : "bg-down"}"
          aria-hidden="true"
        ></span>
        ${inc.resolved
          ? t("publicStatus.incidentResolved")
          : t("publicStatus.incidentOngoing")}
      </span>
    </li>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "status-page": StatusPage;
  }
}
