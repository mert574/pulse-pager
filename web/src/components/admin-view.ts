import { html, nothing } from "lit";
import { customElement, state } from "lit/decorators.js";
import { AppElement } from "./base.js";
import { client, ApiError } from "../api/client.js";
import { t, type MessageKey } from "../i18n.js";
import { icon } from "../icons.js";
import { formatDuration } from "../format.js";
import type { AdminMetrics, MonitorType, Plan } from "../api/types.js";

// Operator admin panel: platform-wide totals across every org (signed-up users,
// orgs, monitors, channels), orgs by plan, and a 30-day signup trend. It lives in
// its own app on a separate origin (admin.pulsepager.com), not the customer SPA,
// so a customer-app XSS can't reach an admin session. Authorization is fully
// server-side (the PULSE_PLATFORM_ADMINS allowlist, checked against the Cloudflare
// Access identity in prod), so this view holds no gate of its own: it just renders
// what GET /admin/metrics returns and shows a forbidden state on a 403.

const PLAN_LABEL: Record<Plan, MessageKey> = {
  tier1: "plan.tier1",
  tier2: "plan.tier2",
  tier3: "plan.tier3",
  tierCustom: "plan.tierCustom",
};

// Plan rows render in tier order regardless of the order the API groups them.
const PLAN_ORDER: Plan[] = ["tier1", "tier2", "tier3", "tierCustom"];

// Monitor-type rows render in a fixed order, with the SSL split reusing the form's
// type labels.
const TYPE_ORDER: MonitorType[] = ["http", "ssl"];
const TYPE_LABEL: Record<MonitorType, MessageKey> = {
  http: "monitorForm.typeHttp",
  ssl: "monitorForm.typeSsl",
};

@customElement("admin-view")
export class AdminView extends AppElement {
  @state() private metrics: AdminMetrics | null = null;
  @state() private error: string | null = null;
  @state() private forbidden = false;

  override firstUpdated(): void {
    void this.load();
  }

  private async load(): Promise<void> {
    this.error = null;
    this.forbidden = false;
    try {
      this.metrics = await client.getAdminMetrics();
    } catch (err) {
      this.metrics = null;
      if (err instanceof ApiError && err.status === 403) {
        this.forbidden = true;
      } else {
        this.error = err instanceof ApiError ? err.message : t("state.error");
      }
    }
  }

  private retry(): void {
    this.metrics = null;
    void this.load();
  }

  override render() {
    return html`
      <div class="flex flex-col gap-8 w-full">
        <div>
          <h1 class="text-2xl font-bold">${t("admin.title")}</h1>
          <p class="text-base-content/60 text-sm mt-1">${t("admin.subtitle")}</p>
        </div>
        ${this.body()}
      </div>
    `;
  }

  private body() {
    if (this.forbidden) {
      return html`<div role="status" class="alert alert-info">
        <span>${icon("lock", "size-5")}</span>
        <span>${t("admin.forbidden")}</span>
      </div>`;
    }
    if (this.error) {
      return html`<div role="alert" class="alert alert-error">
        <span>${this.error}</span>
        <button class="btn btn-sm" @click=${this.retry}>${t("state.retry")}</button>
      </div>`;
    }
    if (!this.metrics) {
      return html`<div class="flex flex-col gap-4" aria-busy="true">
        ${Array.from({ length: 3 }).map(
          () => html`<div class="skeleton h-24 w-full"></div>`,
        )}
      </div>`;
    }
    return html`
      ${this.totals(this.metrics)} ${this.activation(this.metrics)}
      ${this.byPlan(this.metrics)} ${this.byType(this.metrics)}
      ${this.signups(this.metrics)}
    `;
  }

  // --- activation (orgs actually using the product) ---

  private activation(m: AdminMetrics) {
    const rate = m.orgs > 0 ? Math.round((m.orgs_with_monitor / m.orgs) * 100) : 0;
    const ttfm =
      m.median_time_to_first_monitor_seconds == null
        ? "—"
        : formatDuration(m.median_time_to_first_monitor_seconds);
    return html`
      <section class="flex flex-col gap-3">
        <h2 class="text-lg font-semibold">${t("admin.activation")}</h2>
        <div class="grid grid-cols-1 sm:grid-cols-3 gap-4">
          ${this.card(
            t("admin.activationRate"),
            rate,
            `${m.orgs_with_monitor} / ${m.orgs} ${t("admin.orgsCount").toLowerCase()}`,
            "%",
          )}
          ${this.card(t("admin.activeOrgs7d"), m.active_orgs_7d)}
          ${this.textCard(t("admin.timeToFirstMonitor"), ttfm)}
        </div>
      </section>
    `;
  }

  // A card whose value is already-formatted text (not a raw number).
  private textCard(label: string, value: string) {
    return html`<div class="rounded-box border border-base-300 p-5 flex flex-col gap-1">
      <span class="text-base-content/60 text-sm">${label}</span>
      <span class="text-3xl font-bold tabular-nums">${value}</span>
    </div>`;
  }

  // --- core totals ---

  private totals(m: AdminMetrics) {
    return html`
      <section class="grid grid-cols-2 sm:grid-cols-4 gap-4">
        ${this.card(t("admin.users"), m.users)}
        ${this.card(t("admin.orgs"), m.orgs)}
        ${this.card(
          t("admin.monitors"),
          m.monitors_total,
          `${m.monitors_enabled} ${t("admin.monitorsEnabled")} · ${m.monitors_disabled} ${t("admin.monitorsDisabled")}`,
        )}
        ${this.card(t("admin.channels"), m.channels)}
      </section>
    `;
  }

  private card(label: string, value: number, sub?: string, suffix?: string) {
    return html`<div class="rounded-box border border-base-300 p-5 flex flex-col gap-1">
      <span class="text-base-content/60 text-sm">${label}</span>
      <span class="text-3xl font-bold tabular-nums"
        >${value}${suffix ? html`<span class="text-xl">${suffix}</span>` : nothing}</span
      >
      ${sub ? html`<span class="text-xs text-base-content/50">${sub}</span>` : nothing}
    </div>`;
  }

  // --- orgs by plan ---

  private byPlan(m: AdminMetrics) {
    const counts = new Map<Plan, number>();
    for (const p of m.orgs_by_plan) counts.set(p.plan, p.count);
    return html`
      <section class="flex flex-col gap-3">
        <h2 class="text-lg font-semibold">${t("admin.byPlan")}</h2>
        <table class="table border border-base-300 rounded-box">
          <thead>
            <tr>
              <th>${t("admin.plan")}</th>
              <th class="text-right">${t("admin.orgsCount")}</th>
            </tr>
          </thead>
          <tbody>
            ${PLAN_ORDER.map(
              (p) => html`<tr>
                <td>${t(PLAN_LABEL[p])}</td>
                <td class="text-right tabular-nums">${counts.get(p) ?? 0}</td>
              </tr>`,
            )}
          </tbody>
        </table>
      </section>
    `;
  }

  // --- monitors by check type ---

  private byType(m: AdminMetrics) {
    const counts = new Map<MonitorType, number>();
    for (const c of m.monitors_by_type) counts.set(c.type, c.count);
    return html`
      <section class="flex flex-col gap-3">
        <h2 class="text-lg font-semibold">${t("admin.byType")}</h2>
        <table class="table border border-base-300 rounded-box">
          <thead>
            <tr>
              <th>${t("admin.type")}</th>
              <th class="text-right">${t("admin.monitors")}</th>
            </tr>
          </thead>
          <tbody>
            ${TYPE_ORDER.map(
              (ty) => html`<tr>
                <td>${t(TYPE_LABEL[ty])}</td>
                <td class="text-right tabular-nums">${counts.get(ty) ?? 0}</td>
              </tr>`,
            )}
          </tbody>
        </table>
      </section>
    `;
  }

  // --- 30-day signup trend ---

  private signups(m: AdminMetrics) {
    const totalUsers = m.signups.reduce((a, s) => a + s.users, 0);
    const totalOrgs = m.signups.reduce((a, s) => a + s.orgs, 0);
    const max = Math.max(1, ...m.signups.map((s) => s.users));
    return html`
      <section class="flex flex-col gap-3">
        <h2 class="text-lg font-semibold">${t("admin.signups")}</h2>
        <div class="rounded-box border border-base-300 p-5 flex flex-col gap-4">
          <div class="flex gap-6 text-sm">
            <span>${t("admin.newUsers")}: <b class="tabular-nums">${totalUsers}</b></span>
            <span>${t("admin.newOrgs")}: <b class="tabular-nums">${totalOrgs}</b></span>
          </div>
          <div class="flex items-end gap-0.5 h-24" role="img" aria-label=${t("admin.signups")}>
            ${m.signups.map(
              (s) => html`<div
                class="flex-1 bg-primary/70 rounded-t min-h-px"
                style=${`height:${Math.round((s.users / max) * 100)}%`}
                title=${`${s.date}: ${s.users} ${t("admin.newUsers")}, ${s.orgs} ${t("admin.newOrgs")}`}
              ></div>`,
            )}
          </div>
          <div class="flex justify-between text-xs text-base-content/50">
            <span>${m.signups[0]?.date ?? ""}</span>
            <span>${m.signups[m.signups.length - 1]?.date ?? ""}</span>
          </div>
        </div>
      </section>
    `;
  }
}
