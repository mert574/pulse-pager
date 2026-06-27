import { html, nothing } from "lit";
import { customElement, state } from "lit/decorators.js";
import { AppElement } from "./base.js";
import { client, ApiError } from "../api/client.js";
import { t, tDynamic, type MessageKey } from "../i18n.js";
import { icon } from "../icons.js";
import { errorBox, spinner } from "./ui.js";
import { formatDuration } from "../format.js";
import type {
  AdminBilling,
  AdminMetrics,
  AdminOrg,
  AdminOrgPlanUpdate,
  MonitorType,
  Plan,
} from "../api/types.js";

import "./pulse-ledger.js";

// The org ledger wants a few per-org counts (monitors, members, open incidents) that
// GET /admin/orgs does not return yet. We read them off the row when present and show
// the no-value placeholder otherwise, so the surface is ready for the richer payload.
type AdminOrgRow = AdminOrg & {
  monitors?: number;
  members?: number;
  incidents_open?: number;
};

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

  // The small Archivo-caps title sitting above a section, with a hairline rule.
  private readonly sectionHeading = "pulse-section-title border-b border-line pb-3";

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
    return html`<div class="w-full">${this.body()}</div>`;
  }

  // The padded states (forbidden / error / loading) and, once metrics land, the
  // control-room layout: the dense instrument cluster up top, then the operator
  // sections. Everything sits in the px-6 lg:px-10 column so the org ledger can break
  // back out to the folio edge.
  private body() {
    if (this.forbidden) {
      return html`<div class="px-6 lg:px-10 py-8">
        <div
          role="status"
          class="flex items-center gap-3 border border-hair bg-paper px-4 py-3 text-ink2"
        >
          <span>${icon("lock", "size-5")}</span>
          <span>${t("admin.forbidden")}</span>
        </div>
      </div>`;
    }
    if (this.error) {
      return html`<div class="px-6 lg:px-10 py-8">
        ${errorBox(this.error, () => this.retry(), t("state.retry"))}
      </div>`;
    }
    if (!this.metrics) {
      return html`<div class="px-6 lg:px-10 py-8 flex flex-col gap-4" aria-busy="true">
        ${Array.from({ length: 3 }).map(
          () => html`<div class="h-24 w-full bg-paper animate-pulse"></div>`,
        )}
      </div>`;
    }
    return html`
      <div class="px-6 lg:px-10 py-8 flex flex-col gap-10">
        ${this.instrumentCluster(this.metrics)} ${this.billingSection()}
        ${this.orgsLedger()} ${this.breakdowns(this.metrics)}
        ${this.signups(this.metrics)}
      </div>
    `;
  }

  // The instrument cluster: a dense grid of small gauges (the control-room glance),
  // not one giant numeral. Each gauge is a mono micro-label over a compact Archivo
  // value; the gap-px hairline grid reads like a panel of dials. Open incidents are
  // summed across the org rows when that field is present (see totalOpenIncidents).
  private instrumentCluster(m: AdminMetrics) {
    const rate =
      m.orgs > 0 ? Math.round((m.orgs_with_monitor / m.orgs) * 100) : 0;
    const ttfm =
      m.median_time_to_first_monitor_seconds == null
        ? "—"
        : formatDuration(m.median_time_to_first_monitor_seconds);
    const incidents = this.totalOpenIncidents();
    return html`<section class="flex flex-col gap-3">
      <div class="flex items-baseline justify-between border-b border-line pb-3">
        <h2 class="pulse-section-title">${tDynamic("admin.cluster", "Platform")}</h2>
        <span class="font-mono text-[10.5px] uppercase tracking-[0.14em] text-ink3"
          >${tDynamic("admin.clusterReadout", "Live readout")}</span
        >
      </div>
      <div
        class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 gap-px bg-hair border border-hair"
      >
        ${this.gauge(t("admin.orgs"), m.orgs)}
        ${this.gauge(t("admin.users"), m.users)}
        ${this.gauge(
          t("admin.monitors"),
          m.monitors_total,
          `${m.monitors_enabled} ${t("admin.monitorsEnabled").toLowerCase()} · ${m.monitors_disabled} ${t("admin.monitorsDisabled").toLowerCase()}`,
        )}
        ${this.gauge(t("admin.channels"), m.channels)}
        ${this.gauge(
          t("admin.activationRate"),
          html`${rate}<span class="text-[15px] text-ink3">%</span>`,
          `${m.orgs_with_monitor} / ${m.orgs} ${t("admin.orgsCount").toLowerCase()}`,
        )}
        ${this.gauge(t("admin.activeOrgs7d"), m.active_orgs_7d)}
        ${this.gauge(
          tDynamic("admin.openIncidents", "Open incidents"),
          incidents.value,
          undefined,
          incidents.tone,
        )}
        ${this.gauge(t("admin.timeToFirstMonitor"), ttfm)}
      </div>
    </section>`;
  }

  // One gauge in the cluster: a mono micro-label, a compact Archivo value (tone can
  // color it), and an optional mono sub-line. The bg-bg cell over the gap-px bg-hair
  // grid draws the hairline dividers between dials.
  private gauge(label: string, value: unknown, sub?: unknown, tone = "") {
    return html`<div class="bg-bg px-4 py-4 flex flex-col">
      <div class="pulse-label">${label}</div>
      <div
        class="font-disp font-extrabold text-[26px] leading-[0.95] tracking-[-0.035em] mt-2 ${tone}"
      >
        ${value}
      </div>
      ${sub
        ? html`<div class="font-mono text-[10.5px] text-ink3 mt-1.5 leading-snug">
            ${sub}
          </div>`
        : nothing}
    </div>`;
  }

  // Open incidents across the platform, summed from the org rows that carry the
  // assumed incidents_open count. When no row has it we show the no-value placeholder
  // rather than a misleading zero (see AdminOrgRow).
  private totalOpenIncidents(): { value: string; tone: string } {
    const orgs = this.orgs as AdminOrgRow[] | null;
    if (!orgs) return { value: "—", tone: "" };
    const known = orgs.filter((o) => o.incidents_open != null);
    if (known.length === 0) return { value: "—", tone: "" };
    const total = known.reduce((sum, o) => sum + (o.incidents_open ?? 0), 0);
    return { value: String(total), tone: total > 0 ? "text-down" : "" };
  }

  // --- billing & paid users (RFC-018): paid orgs, subscription statuses, revenue ---

  private billingSection() {
    const b = this.billing;
    if (!b) return nothing;
    const hasActivity =
      b.subscriptions_by_status.length > 0 || b.revenue_by_currency.length > 0;
    return html`
      <section class="flex flex-col gap-4">
        <div class="flex flex-col gap-2 border-b border-line pb-3">
          <h2 class="pulse-section-title">${t("admin.billing")}</h2>
          <p class="font-mono text-[11.5px] text-ink2">${t("admin.billingHint")}</p>
        </div>
        <div
          class="grid grid-cols-2 lg:grid-cols-4 gap-px bg-hair border border-hair"
        >
          ${this.gauge(t("admin.paidOrgs"), b.paid_orgs)}
          ${this.gauge(t("admin.activeSubs"), this.subCount(b, "active"))}
          ${this.gauge(
            t("admin.pastDueSubs"),
            this.subCount(b, "past_due"),
            undefined,
            this.subCount(b, "past_due") ? "text-deg" : "",
          )}
          ${this.gauge(t("admin.canceledSubs"), this.subCount(b, "canceled"))}
        </div>
        ${b.revenue_by_currency.length > 0
          ? html`<div class="flex flex-col">
              ${b.revenue_by_currency.map(
                (r) => html`<div
                  class="flex flex-wrap items-baseline justify-between gap-x-6 gap-y-1.5 py-3 border-b border-hair"
                >
                  <span
                    class="font-disp font-extrabold text-[15px] uppercase tracking-[0.04em]"
                    >${r.currency}</span
                  >
                  <div
                    class="flex flex-wrap gap-x-6 gap-y-1 font-mono text-[12px] text-ink2"
                  >
                    <span
                      >${t("admin.revenueGross")}
                      <b class="text-ink">${this.money(r.gross, r.currency)}</b></span
                    >
                    <span
                      >${t("admin.revenueRefunded")}
                      <b class="text-ink"
                        >${this.money(r.refunded, r.currency)}</b
                      ></span
                    >
                    <span
                      >${t("admin.revenuePayments")}
                      <b class="text-ink">${r.payments}</b></span
                    >
                  </div>
                </div>`,
              )}
            </div>`
          : nothing}
        ${!hasActivity
          ? html`<p class="font-mono text-[12px] text-ink3">
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

  private orgsLedger() {
    const orgs = this.orgs;
    if (!orgs) return nothing;
    return html`
      <section class="flex flex-col gap-3">
        <div class="flex items-baseline justify-between border-b border-line pb-3">
          <h2 class="pulse-section-title">${t("admin.organizations")}</h2>
          <span class="font-mono text-[11px] text-ink3"
            >${String(orgs.length).padStart(2, "0")}</span
          >
        </div>
        ${this.planError
          ? html`<div role="alert" class="border border-down px-4 py-3 text-down">
              ${this.planError}
            </div>`
          : nothing}
        <pulse-ledger
          .items=${orgs}
          .renderRow=${(item: unknown, i: number) =>
            this.orgRow(item as AdminOrg, i)}
        ></pulse-ledger>
      </section>
      ${this.actionDialog()}
    `;
  }

  // One org per ledger row: an index, the org name (with its slug as a tag) over the
  // per-org counts, then the plan select and the operator actions. The select stays
  // one-per-row in org order so an operator can change any org's plan inline.
  private orgRow(o: AdminOrg, i: number) {
    const r = o as AdminOrgRow;
    const n = String(i + 1).padStart(2, "0");
    const num = (v?: number): string => (v == null ? "—" : String(v));
    return html`<div
      class="grid grid-cols-[36px_1fr] sm:grid-cols-[44px_1fr_auto] gap-x-4 gap-y-3 px-6 lg:px-10 py-5 border-b border-hair sm:items-center"
    >
      <span class="font-mono text-[12px] text-brand">${n}</span>
      <div class="min-w-0">
        <div class="flex items-center gap-2.5 flex-wrap">
          <span
            class="font-disp font-extrabold text-[17px] tracking-[-0.025em] truncate"
            >${o.name}</span
          >
          <span class="pulse-tag">${o.slug}</span>
        </div>
        <div
          class="font-mono text-[11.5px] text-ink3 mt-1 flex flex-wrap gap-x-4 gap-y-0.5"
        >
          <span>${num(r.monitors)} ${tDynamic("admin.colMonitors", "monitors")}</span>
          <span>${num(r.members)} ${tDynamic("admin.colMembers", "members")}</span>
          <span class=${r.incidents_open ? "text-down" : ""}
            >${num(r.incidents_open)} ${tDynamic("admin.colOpenIncidents", "open")}</span
          >
        </div>
      </div>
      <div
        class="col-start-2 sm:col-start-3 flex flex-wrap items-center gap-2 sm:justify-end"
      >
        <select
          class="pulse-input w-36"
          aria-label=${`${t("admin.plan")} — ${o.name}`}
          .value=${o.plan}
          ?disabled=${this.savingOrgId === o.id}
          @change=${(e: Event) =>
            this.changePlan(o, (e.target as HTMLSelectElement).value as Plan)}
        >
          ${PLAN_ORDER.map(
            (p) =>
              html`<option value=${p} ?selected=${p === o.plan}>
                ${t(PLAN_LABEL[p])}
              </option>`,
          )}
        </select>
        <button
          class="pulse-btn pulse-btn-ghost pulse-btn-sm"
          @click=${() => this.openAction({ kind: "cancel", org: o })}
        >
          ${t("admin.cancelSub")}
        </button>
        <button
          class="pulse-btn pulse-btn-ghost pulse-btn-sm border-down text-down"
          @click=${() => this.openAction({ kind: "refund", org: o })}
        >
          ${t("admin.refund")}
        </button>
      </div>
    </div>`;
  }

  // --- operator action dialog (cancel / refund / custom price) ---

  private actionDialog() {
    const a = this.action;
    if (!a) return nothing;
    return html`
      <div
        class="fixed inset-0 z-50 flex items-center justify-center p-4"
        role="dialog"
        aria-modal="true"
      >
        <div
          class="absolute inset-0 bg-black/50"
          @click=${this.closeAction}
        ></div>
        <div
          class="relative z-10 w-full max-w-md border border-hair bg-bg p-5 flex flex-col gap-4"
        >
          <h3 class="font-disp font-extrabold text-[15px] uppercase tracking-[0.03em]">
            ${this.dialogTitle(a)} — ${a.org.name}
          </h3>
          ${this.dialogBody(a)}
          ${this.actionError
            ? html`<div role="alert" class="border border-down px-4 py-3 text-down">
                ${this.actionError}
              </div>`
            : nothing}
          <div class="flex justify-end gap-2">
            <button
              class="pulse-btn pulse-btn-ghost"
              ?disabled=${this.actionBusy}
              @click=${this.closeAction}
            >
              ${t("admin.dialogCancel")}
            </button>
            <button
              class=${`pulse-btn ${a.kind === "refund" ? "pulse-btn-ghost border-down text-down" : ""}`}
              ?disabled=${this.actionBusy || !this.canSubmit(a)}
              @click=${this.submitAction}
            >
              ${this.actionBusy ? spinner() : t("admin.confirm")}
            </button>
          </div>
        </div>
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
        <p class="text-sm text-ink2">${t("admin.cancelHelp")}</p>
        <select
          class="pulse-input w-full"
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
        <p class="text-sm text-ink2">${t("admin.refundHelp")}</p>
        <input
          class="pulse-input w-full"
          placeholder=${t("admin.refundPaymentId")}
          .value=${this.fPaymentId}
          @input=${(e: Event) =>
            (this.fPaymentId = (e.target as HTMLInputElement).value)}
        />
        <input
          class="pulse-input w-full"
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
      <p class="text-sm text-ink2">${t("admin.customHelp")}</p>
      <input
        class="pulse-input w-full"
        type="number"
        min="0"
        placeholder=${t("admin.customAmount")}
        .value=${this.fCustomAmount}
        @input=${(e: Event) =>
          (this.fCustomAmount = (e.target as HTMLInputElement).value)}
      />
      <div class="flex gap-2">
        <input
          class="pulse-input w-24"
          placeholder=${t("admin.customCurrency")}
          .value=${this.fCustomCurrency}
          @input=${(e: Event) =>
            (this.fCustomCurrency = (e.target as HTMLInputElement).value)}
        />
        <select
          class="pulse-input flex-1"
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

  // --- orgs by plan + monitors by check type (editorial breakdowns) ---

  private breakdowns(m: AdminMetrics) {
    const planCounts = new Map<Plan, number>();
    for (const p of m.orgs_by_plan) planCounts.set(p.plan, p.count);
    const typeCounts = new Map<MonitorType, number>();
    for (const c of m.monitors_by_type) typeCounts.set(c.type, c.count);
    return html`<div class="grid grid-cols-1 md:grid-cols-2 gap-8">
      ${this.breakdown(
        t("admin.byPlan"),
        PLAN_ORDER.map((p) => ({
          label: t(PLAN_LABEL[p]),
          count: planCounts.get(p) ?? 0,
        })),
      )}
      ${this.breakdown(
        t("admin.byType"),
        TYPE_ORDER.map((ty) => ({
          label: t(TYPE_LABEL[ty]),
          count: typeCounts.get(ty) ?? 0,
        })),
      )}
    </div>`;
  }

  // A labelled breakdown: each row is a name, a proportional brand bar, and a mono
  // count. The bar is decorative; the count is always shown so it never relies on it.
  private breakdown(title: string, rows: { label: string; count: number }[]) {
    const max = Math.max(1, ...rows.map((r) => r.count));
    return html`<section class="flex flex-col gap-3">
      <h2 class=${this.sectionHeading}>${title}</h2>
      <div class="flex flex-col">
        ${rows.map(
          (r) => html`<div
            class="flex items-center gap-4 py-3 border-b border-hair"
          >
            <span
              class="flex-1 font-disp font-extrabold text-[14px] tracking-[-0.01em]"
              >${r.label}</span
            >
            <div class="w-24 sm:w-32 h-1.5 bg-paper" aria-hidden="true">
              <div
                class="h-full bg-brand"
                style=${`width:${Math.round((r.count / max) * 100)}%`}
              ></div>
            </div>
            <span class="font-mono text-[13px] w-8 text-right tabular-nums"
              >${r.count}</span
            >
          </div>`,
        )}
      </div>
    </section>`;
  }

  // --- 30-day signup trend ---

  private signups(m: AdminMetrics) {
    const totalUsers = m.signups.reduce((a, s) => a + s.users, 0);
    const totalOrgs = m.signups.reduce((a, s) => a + s.orgs, 0);
    const max = Math.max(1, ...m.signups.map((s) => s.users));
    return html`
      <section class="flex flex-col gap-3">
        <h2 class=${this.sectionHeading}>${t("admin.signups")}</h2>
        <div class="border border-hair p-5 flex flex-col gap-4">
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
                  class="flex-1 bg-brand min-h-px"
                  style=${`height:${Math.round((s.users / max) * 100)}%`}
                  title=${`${s.date}: ${s.users} ${t("admin.newUsers")}, ${s.orgs} ${t("admin.newOrgs")}`}
                ></div>`,
            )}
          </div>
          <div class="flex justify-between text-xs text-ink3">
            <span>${m.signups[0]?.date ?? ""}</span>
            <span>${m.signups[m.signups.length - 1]?.date ?? ""}</span>
          </div>
        </div>
      </section>
    `;
  }
}
