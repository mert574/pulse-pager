import { html } from "lit";
import { customElement, property, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { t } from "../i18n.js";
import type { Monitor } from "../api/types.js";

import "./http-monitor-detail.js";
import "./ssl-monitor-detail.js";

// Monitor detail (RFC-013 section 7.2). This is the route element: it loads the
// monitor once and dispatches to the type-specific view (http-monitor-detail or
// ssl-monitor-detail), handing the loaded monitor down so the child does not
// refetch it. The two views share their header / check-now / delete chrome through
// MonitorDetailBase but render entirely different middles, because an http check
// (uptime, latency, regions, recent checks) and an ssl cert check (the certificate,
// its expiry) have nothing in common to show.
@customElement("monitor-detail-view")
export class MonitorDetailView extends AppElement {
  // set from the route :id param
  @property({ type: String }) monitorId = "";

  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private monitor: Monitor | null = null;
  @state() private loading = false;
  @state() private error: string | null = null;

  private loadedKey: string | null = null;

  private get orgId(): string | null {
    return this.ctx?.activeOrg?.org_id ?? null;
  }

  override updated(): void {
    const orgId = this.orgId;
    const key = orgId && this.monitorId ? `${orgId}:${this.monitorId}` : null;
    if (key && key !== this.loadedKey) void this.load();
  }

  private async load(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId || !this.monitorId) return;
    this.loadedKey = `${orgId}:${this.monitorId}`;
    this.loading = true;
    this.error = null;
    try {
      this.monitor = await client.getMonitor(orgId, this.monitorId);
    } catch (err) {
      this.error = err instanceof ApiError ? err.message : t("state.error");
      this.monitor = null;
    } finally {
      this.loading = false;
    }
  }

  override render() {
    if (this.loading && !this.monitor) {
      return html`<div class="flex flex-col gap-6" aria-busy="true">
        <div class="skeleton h-9 w-64"></div>
        <div class="skeleton h-24 w-full"></div>
        <div class="skeleton h-56 w-full"></div>
        <div class="skeleton h-48 w-full"></div>
      </div>`;
    }
    if (this.error || !this.monitor) {
      return html`<div role="alert" class="alert alert-error">
        <span>${this.error ?? t("state.error")}</span>
        <button class="btn btn-sm" @click=${() => this.load()}>
          ${t("state.retry")}
        </button>
      </div>`;
    }
    return this.monitor.type === "ssl"
      ? html`<ssl-monitor-detail .monitor=${this.monitor}></ssl-monitor-detail>`
      : html`<http-monitor-detail .monitor=${this.monitor}></http-monitor-detail>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "monitor-detail-view": MonitorDetailView;
  }
}
