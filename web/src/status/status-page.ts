import { html, nothing, type TemplateResult } from "lit";
import { customElement, state } from "lit/decorators.js";
import { AppElement } from "../components/base.js";
import { t, type MessageKey } from "../i18n.js";
import {
  fetchPublicStatusPage,
  resolveSlug,
  PublicFetchError,
} from "./public-client.js";
import type {
  PublicBanner,
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

const BANNER_LABEL: Record<PublicBanner, MessageKey> = {
  operational: "publicStatus.bannerOperational",
  partial_outage: "publicStatus.bannerPartial",
  major_outage: "publicStatus.bannerMajor",
};

// daisyUI alert variant per banner. The banner always carries a text label too,
// so color is never the only signal (RFC-013 section 9.1).
const BANNER_CLASS: Record<PublicBanner, string> = {
  operational: "alert-success",
  partial_outage: "alert-warning",
  major_outage: "alert-error",
};

// The status page's chosen theme maps onto the two daisyUI themes the build ships.
const THEME_TO_DAISY: Record<StatusPageTheme, string> = {
  light: "caramellatte",
  dark: "coffee",
};

// Public status page (PRD-004 3.6, RFC-013 section 8.2). Fetches the public-safe
// projection for the slug (from the subdomain or ?slug=) and renders branding, the
// overall banner, each displayed monitor's friendly name + reduced status + uptime
// summary + recent history strip, and past public incidents. It renders ONLY what
// the public endpoint returns, so no internal URLs, methods, or failure detail can
// ever appear here. A 404 (unknown or unpublished slug) shows a plain not-found.
@customElement("status-page")
export class StatusPage extends AppElement {
  @state() private status: LoadState = { kind: "loading" };

  override connectedCallback(): void {
    super.connectedCallback();
    void this.load();
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

  // Apply the page's theme and accent at the document level so daisyUI tokens and
  // the brand accent reach the whole page, not just this element's subtree.
  private applyBranding(data: PublicStatusPageData): void {
    document.documentElement.dataset.theme = THEME_TO_DAISY[data.theme];
    document.documentElement.style.setProperty("--brand-accent", data.accent_color);
    if (data.name) document.title = data.name;
  }

  override render() {
    switch (this.status.kind) {
      case "loading":
        return this.centered(
          html`<span class="loading loading-spinner loading-lg"></span>`,
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
      class="card bg-base-100 border border-base-300 max-w-md w-full"
    >
      <div class="card-body items-center text-center gap-3">
        <p class="text-xl font-bold">${title}</p>
        <p class="text-base-content/60">${body}</p>
        ${retry
          ? html`<button class="btn btn-sm" @click=${() => this.load()}>
              ${t("publicStatus.retry")}
            </button>`
          : nothing}
      </div>
    </div>`);
  }

  private page(data: PublicStatusPageData) {
    return html`
      <div class="max-w-3xl mx-auto px-6 py-12 flex flex-col gap-8">
        ${this.header(data)} ${this.banner(data.banner)}
        ${this.monitors(data.monitors)} ${this.incidents(data.incidents)}
        <footer class="text-center text-sm text-base-content/60">
          ${t("publicStatus.poweredBy")}
        </footer>
      </div>
    `;
  }

  private header(data: PublicStatusPageData) {
    return html`<header class="flex flex-col items-center text-center gap-3">
      ${data.logo_url
        ? html`<img
            src=${data.logo_url}
            alt=""
            class="h-12 object-contain"
            referrerpolicy="no-referrer"
          />`
        : nothing}
      <h1 class="text-3xl font-bold" style="color: var(--brand-accent)">
        ${data.name}
      </h1>
    </header>`;
  }

  private banner(banner: PublicBanner) {
    return html`<div role="status" class="alert ${BANNER_CLASS[banner]} justify-center">
      <span class="text-lg font-semibold">${t(BANNER_LABEL[banner])}</span>
    </div>`;
  }

  private monitors(monitors: PublicDisplayedMonitor[]) {
    if (monitors.length === 0) return nothing;
    return html`<section class="flex flex-col gap-3">
      ${monitors.map((m) => this.monitorCard(m))}
    </section>`;
  }

  private monitorCard(m: PublicDisplayedMonitor) {
    const operational = m.status === "operational";
    const bars: UptimeBar[] = m.history.map((p) => ({ healthy: p.up }));
    return html`<div class="card bg-base-100 border border-base-300">
      <div class="card-body gap-3 p-5">
        <div class="flex items-center justify-between gap-3">
          <span class="font-medium">${m.display_name}</span>
          <span
            class="badge gap-1 ${operational
              ? "badge-success badge-soft"
              : "badge-error badge-soft"}"
          >
            <span class="inline-block size-2 rounded-full bg-current" aria-hidden="true"></span>
            ${t(operational ? "publicStatus.statusOperational" : "publicStatus.statusDown")}
          </span>
        </div>
        ${bars.length ? html`<uptime-bar .bars=${bars}></uptime-bar>` : nothing}
        ${this.uptimeSummary(m.uptime)}
      </div>
    </div>`;
  }

  private uptimeSummary(u: PublicUptime) {
    const cell = (labelKey: MessageKey, has: boolean, value: number) => html`<div
      class="flex flex-col"
    >
      <span class="text-xs text-base-content/50">${t(labelKey)}</span>
      <span class="font-medium"
        >${has ? `${value.toFixed(2)}%` : t("publicStatus.noData")}</span
      >
    </div>`;
    return html`<div class="grid grid-cols-3 gap-3">
      ${cell("publicStatus.uptime24h", u.has_24h, u.uptime_24h)}
      ${cell("publicStatus.uptime7d", u.has_7d, u.uptime_7d)}
      ${cell("publicStatus.uptime90d", u.has_90d, u.uptime_90d)}
    </div>`;
  }

  private incidents(incidents: PublicIncident[]) {
    return html`<section class="flex flex-col gap-3">
      <h2 class="font-semibold">${t("publicStatus.incidents")}</h2>
      ${incidents.length === 0
        ? html`<p class="text-base-content/60 text-sm">
            ${t("publicStatus.noIncidents")}
          </p>`
        : html`<ul class="flex flex-col gap-2">
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
      class="card bg-base-100 border border-base-300 px-4 py-3 flex-row items-center justify-between gap-3"
    >
      <div class="flex flex-col">
        <span class="font-medium">${inc.display_name}</span>
        <span class="text-xs text-base-content/50">${startLabel}</span>
      </div>
      <span
        class="badge badge-sm ${inc.resolved ? "badge-success badge-soft" : "badge-error badge-soft"}"
      >
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
