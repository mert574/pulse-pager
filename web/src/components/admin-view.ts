import { html, nothing } from "lit";
import { customElement, state } from "lit/decorators.js";
import { AppElement } from "./base.js";
import { client, ApiError } from "../api/client.js";
import { t, type MessageKey } from "../i18n.js";
import { icon } from "../icons.js";
import { formatDuration } from "../format.js";
import type {
  AdminBilling,
  AdminMetrics,
  AdminOrg,
  AdminOrgPlanUpdate,
  MonitorType,
  Plan,
} from "../api/types.js";

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

// An operator action that needs a confirm dialog before it hits the provider.
type AdminAction =
  | { kind: "cancel"; org: AdminOrg }
  | { kind: "refund"; org: AdminOrg }
  | { kind: "custom"; org: AdminOrg };

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
  @state() private orgs: AdminOrg[] | null = null;
  @state() private billing: AdminBilling | null = null;
  @state() private error: string | null = null;
  @state() private forbidden = false;
  // id of the org whose plan is being saved right now (disables its select), and
  // a separate error for a failed plan change so it doesn't blow away the panel.
  @state() private savingOrgId: string | null = null;
  @state() private planError: string | null = null;

  // Operator action dialog (cancel / refund / custom-price). One at a time.
  @state() private action: AdminAction | null = null;
  @state() private actionBusy = false;
  @state() private actionError: string | null = null;
  // Dialog form fields (reset each time a dialog opens).
  @state() private fCancelWhen: "immediate" | "period_end" = "period_end";
  @state() private fPaymentId = "";
  @state() private fRefundAmount = "";
  @state() private fCustomAmount = "";
  @state() private fCustomCurrency = "USD";
  @state() private fCycle: "monthly" | "annual" = "monthly";

  override firstUpdated(): void {
    void this.load();
  }

  private async load(): Promise<void> {
    this.error = null;
    this.forbidden = false;
    try {
      const [metrics, orgs, billing] = await Promise.all([
        client.getAdminMetrics(),
        client.listAdminOrgs(),
        client.getAdminBilling(),
      ]);
      this.metrics = metrics;
      this.orgs = orgs;
      this.billing = billing;
    } catch (err) {
      this.metrics = null;
      this.orgs = null;
      this.billing = null;
      if (err instanceof ApiError && err.status === 403) {
        this.forbidden = true;
      } else {
        this.error = err instanceof ApiError ? err.message : t("state.error");
      }
    }
  }

  private retry(): void {
    this.metrics = null;
    this.orgs = null;
    this.billing = null;
    void this.load();
  }

  // changePlan handles the plan dropdown. A move to Custom needs a negotiated price,
  // so it opens the custom dialog instead of changing in place; every other tier is a
  // direct override (no provider price). The select reverts on the next render if the
  // dialog is cancelled, since orgs still holds the old plan.
  private changePlan(o: AdminOrg, plan: Plan): void {
    if (plan === "tierCustom") {
      this.openAction({ kind: "custom", org: o });
      return;
    }
    void this.applyPlan(o.id, { plan });
  }

  private async applyPlan(
    orgId: string,
    body: AdminOrgPlanUpdate,
  ): Promise<void> {
    this.savingOrgId = orgId;
    this.planError = null;
    try {
      const updated = await client.setAdminOrgPlan(orgId, body);
      this.orgs = (this.orgs ?? []).map((o) => (o.id === orgId ? updated : o));
    } catch (err) {
      this.planError = err instanceof ApiError ? err.message : t("state.error");
    } finally {
      this.savingOrgId = null;
    }
  }

  // --- operator action dialog ---

  private openAction(a: AdminAction): void {
    this.action = a;
    this.actionError = null;
    this.actionBusy = false;
    this.fCancelWhen = "period_end";
    this.fPaymentId = "";
    this.fRefundAmount = "";
    this.fCustomAmount = "";
    this.fCustomCurrency = "USD";
    this.fCycle = "monthly";
  }

  private closeAction(): void {
    this.action = null;
    // The plan select may show tierCustom from a cancelled custom dialog; re-render
    // restores it from orgs (which still holds the real plan).
    this.requestUpdate();
  }

  private async submitAction(): Promise<void> {
    const a = this.action;
    if (!a) return;
    this.actionBusy = true;
    this.actionError = null;
    try {
      if (a.kind === "cancel") {
        await client.cancelAdminOrgSubscription(a.org.id, this.fCancelWhen);
      } else if (a.kind === "refund") {
        const amount = this.fRefundAmount.trim()
          ? Number(this.fRefundAmount)
          : undefined;
        await client.refundAdminOrgPayment(a.org.id, {
          payment_id: this.fPaymentId.trim(),
          amount,
        });
      } else {
        const amount = this.fCustomAmount.trim()
          ? Number(this.fCustomAmount)
          : undefined;
        const updated = await client.setAdminOrgPlan(a.org.id, {
          plan: "tierCustom",
          cycle: this.fCycle,
          custom_amount: amount,
          custom_currency: amount != null ? this.fCustomCurrency : undefined,
        });
        this.orgs = (this.orgs ?? []).map((o) =>
          o.id === a.org.id ? updated : o,
        );
      }
      this.action = null;
    } catch (err) {
      this.actionError =
        err instanceof ApiError ? err.message : t("state.error");
    } finally {
      this.actionBusy = false;
    }
  }

  override render() {
    return html`
      <div class="flex flex-col gap-8 w-full">
        <div>
          <h1 class="text-2xl font-bold">${t("admin.title")}</h1>
          <p class="text-base-content/60 text-sm mt-1">
            ${t("admin.subtitle")}
          </p>
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
        <button class="btn btn-sm" @click=${this.retry}>
          ${t("state.retry")}
        </button>
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
      ${this.byPlan(this.metrics)} ${this.billingSection()} ${this.orgsTable()}
      ${this.byType(this.metrics)} ${this.signups(this.metrics)}
    `;
  }

  // --- billing & paid users (RFC-018): paid orgs, subscription statuses, revenue ---

  private billingSection() {
    const b = this.billing;
    if (!b) return nothing;
    const hasActivity =
      b.subscriptions_by_status.length > 0 || b.revenue_by_currency.length > 0;
    return html`
      <section class="flex flex-col gap-3">
        <div>
          <h2 class="text-lg font-semibold">${t("admin.billing")}</h2>
          <p class="text-base-content/60 text-sm">${t("admin.billingHint")}</p>
        </div>
        <div class="grid grid-cols-2 sm:grid-cols-4 gap-4">
          ${this.card(t("admin.paidOrgs"), b.paid_orgs)}
          ${this.card(t("admin.activeSubs"), this.subCount(b, "active"))}
          ${this.card(t("admin.pastDueSubs"), this.subCount(b, "past_due"))}
          ${this.card(t("admin.canceledSubs"), this.subCount(b, "canceled"))}
        </div>
        ${b.revenue_by_currency.length > 0
          ? html`<table class="table border border-base-300 rounded-box">
              <thead>
                <tr>
                  <th>${t("admin.currency")}</th>
                  <th class="text-right">${t("admin.revenueGross")}</th>
                  <th class="text-right">${t("admin.revenueRefunded")}</th>
                  <th class="text-right">${t("admin.revenuePayments")}</th>
                </tr>
              </thead>
              <tbody>
                ${b.revenue_by_currency.map(
                  (r) => html`<tr>
                    <td>${r.currency}</td>
                    <td class="text-right tabular-nums">
                      ${this.money(r.gross, r.currency)}
                    </td>
                    <td class="text-right tabular-nums">
                      ${this.money(r.refunded, r.currency)}
                    </td>
                    <td class="text-right tabular-nums">${r.payments}</td>
                  </tr>`,
                )}
              </tbody>
            </table>`
          : nothing}
        ${!hasActivity
          ? html`<p class="text-base-content/50 text-sm">
              ${t("admin.billingEmpty")}
            </p>`
          : nothing}
      </section>
    `;
  }

  // subCount returns how many subscriptions are in a given status (0 if none).
  private subCount(b: AdminBilling, status: string): number {
    return b.subscriptions_by_status.find((s) => s.status === status)?.count ?? 0;
  }

  // money formats minor units (cents) + currency, plainly (no Intl currency code
  // validation, since the provider currency is free-form text in the mirror).
  private money(minor: number, currency: string): string {
    return `${(minor / 100).toFixed(2)} ${currency}`;
  }

  // --- organizations (see and change each org's plan) ---

  private orgsTable() {
    const orgs = this.orgs;
    if (!orgs) return nothing;
    return html`
      <section class="flex flex-col gap-3">
        <h2 class="text-lg font-semibold">${t("admin.organizations")}</h2>
        ${this.planError
          ? html`<div role="alert" class="alert alert-error">
              <span>${this.planError}</span>
            </div>`
          : nothing}
        <table class="table border border-base-300 rounded-box">
          <thead>
            <tr>
              <th>${t("admin.orgName")}</th>
              <th>${t("admin.orgSlug")}</th>
              <th>${t("admin.plan")}</th>
              <th>${t("admin.actions")}</th>
            </tr>
          </thead>
          <tbody>
            ${orgs.map(
              (o) =>
                html`<tr>
                  <td>${o.name}</td>
                  <td class="text-base-content/60">${o.slug}</td>
                  <td>
                    <select
                      class="select select-sm select-bordered w-44"
                      aria-label=${`${t("admin.plan")} — ${o.name}`}
                      .value=${o.plan}
                      ?disabled=${this.savingOrgId === o.id}
                      @change=${(e: Event) =>
                        this.changePlan(
                          o,
                          (e.target as HTMLSelectElement).value as Plan,
                        )}
                    >
                      ${PLAN_ORDER.map(
                        (p) =>
                          html`<option value=${p} ?selected=${p === o.plan}>
                            ${t(PLAN_LABEL[p])}
                          </option>`,
                      )}
                    </select>
                  </td>
                  <td>
                    <div class="flex gap-2">
                      <button
                        class="btn btn-sm btn-ghost"
                        @click=${() => this.openAction({ kind: "cancel", org: o })}
                      >
                        ${t("admin.cancelSub")}
                      </button>
                      <button
                        class="btn btn-sm btn-ghost text-error"
                        @click=${() => this.openAction({ kind: "refund", org: o })}
                      >
                        ${t("admin.refund")}
                      </button>
                    </div>
                  </td>
                </tr>`,
            )}
          </tbody>
        </table>
      </section>
      ${this.actionDialog()}
    `;
  }

  // --- operator action dialog (cancel / refund / custom price) ---

  private actionDialog() {
    const a = this.action;
    if (!a) return nothing;
    return html`
      <div class="modal modal-open" role="dialog" aria-modal="true">
        <div class="modal-box flex flex-col gap-4">
          <h3 class="text-lg font-semibold">
            ${this.dialogTitle(a)} — ${a.org.name}
          </h3>
          ${this.dialogBody(a)}
          ${this.actionError
            ? html`<div role="alert" class="alert alert-error">
                <span>${this.actionError}</span>
              </div>`
            : nothing}
          <div class="modal-action">
            <button
              class="btn btn-ghost"
              ?disabled=${this.actionBusy}
              @click=${this.closeAction}
            >
              ${t("admin.dialogCancel")}
            </button>
            <button
              class=${`btn ${a.kind === "refund" ? "btn-error" : "btn-primary"}`}
              ?disabled=${this.actionBusy || !this.canSubmit(a)}
              @click=${this.submitAction}
            >
              ${this.actionBusy
                ? html`<span class="loading loading-spinner loading-sm"></span>`
                : t("admin.confirm")}
            </button>
          </div>
        </div>
        <div class="modal-backdrop" @click=${this.closeAction}></div>
      </div>
    `;
  }

  private dialogTitle(a: AdminAction): string {
    if (a.kind === "cancel") return t("admin.cancelSub");
    if (a.kind === "refund") return t("admin.refund");
    return t("admin.customPrice");
  }

  // canSubmit blocks the confirm until the required field is filled.
  private canSubmit(a: AdminAction): boolean {
    if (a.kind === "refund") return this.fPaymentId.trim() !== "";
    return true;
  }

  private dialogBody(a: AdminAction) {
    if (a.kind === "cancel") {
      return html`
        <p class="text-sm text-base-content/70">${t("admin.cancelHelp")}</p>
        <select
          class="select select-bordered w-full"
          .value=${this.fCancelWhen}
          @change=${(e: Event) =>
            (this.fCancelWhen = (e.target as HTMLSelectElement).value as
              | "immediate"
              | "period_end")}
        >
          <option value="period_end">${t("admin.cancelPeriodEnd")}</option>
          <option value="immediate">${t("admin.cancelImmediate")}</option>
        </select>
      `;
    }
    if (a.kind === "refund") {
      return html`
        <p class="text-sm text-base-content/70">${t("admin.refundHelp")}</p>
        <input
          class="input input-bordered w-full"
          placeholder=${t("admin.refundPaymentId")}
          .value=${this.fPaymentId}
          @input=${(e: Event) =>
            (this.fPaymentId = (e.target as HTMLInputElement).value)}
        />
        <input
          class="input input-bordered w-full"
          type="number"
          min="0"
          placeholder=${t("admin.refundAmount")}
          .value=${this.fRefundAmount}
          @input=${(e: Event) =>
            (this.fRefundAmount = (e.target as HTMLInputElement).value)}
        />
      `;
    }
    return html`
      <p class="text-sm text-base-content/70">${t("admin.customHelp")}</p>
      <input
        class="input input-bordered w-full"
        type="number"
        min="0"
        placeholder=${t("admin.customAmount")}
        .value=${this.fCustomAmount}
        @input=${(e: Event) =>
          (this.fCustomAmount = (e.target as HTMLInputElement).value)}
      />
      <div class="flex gap-2">
        <input
          class="input input-bordered w-24"
          placeholder=${t("admin.customCurrency")}
          .value=${this.fCustomCurrency}
          @input=${(e: Event) =>
            (this.fCustomCurrency = (e.target as HTMLInputElement).value)}
        />
        <select
          class="select select-bordered flex-1"
          .value=${this.fCycle}
          @change=${(e: Event) =>
            (this.fCycle = (e.target as HTMLSelectElement).value as
              | "monthly"
              | "annual")}
        >
          <option value="monthly">${t("admin.cycleMonthly")}</option>
          <option value="annual">${t("admin.cycleAnnual")}</option>
        </select>
      </div>
    `;
  }

  // --- activation (orgs actually using the product) ---

  private activation(m: AdminMetrics) {
    const rate =
      m.orgs > 0 ? Math.round((m.orgs_with_monitor / m.orgs) * 100) : 0;
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
    return html`<div
      class="rounded-box border border-base-300 p-5 flex flex-col gap-1"
    >
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
    return html`<div
      class="rounded-box border border-base-300 p-5 flex flex-col gap-1"
    >
      <span class="text-base-content/60 text-sm">${label}</span>
      <span class="text-3xl font-bold tabular-nums"
        >${value}${suffix
          ? html`<span class="text-xl">${suffix}</span>`
          : nothing}</span
      >
      ${sub
        ? html`<span class="text-xs text-base-content/50">${sub}</span>`
        : nothing}
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
              (p) =>
                html`<tr>
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
              (ty) =>
                html`<tr>
                  <td>${t(TYPE_LABEL[ty])}</td>
                  <td class="text-right tabular-nums">
                    ${counts.get(ty) ?? 0}
                  </td>
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
            <span
              >${t("admin.newUsers")}:
              <b class="tabular-nums">${totalUsers}</b></span
            >
            <span
              >${t("admin.newOrgs")}:
              <b class="tabular-nums">${totalOrgs}</b></span
            >
          </div>
          <div
            class="flex items-end gap-0.5 h-24"
            role="img"
            aria-label=${t("admin.signups")}
          >
            ${m.signups.map(
              (s) =>
                html`<div
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
