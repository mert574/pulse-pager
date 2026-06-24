import { html, nothing } from "lit";
import { customElement, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { can } from "../state/can.js";
import { t, tDynamic, type MessageKey } from "../i18n.js";
import { formatDuration } from "../format.js";
import { icon } from "../icons.js";
import type {
  Entitlements,
  Payment,
  Plan,
  PlanCatalogEntry,
} from "../api/types.js";

// Billing & usage (PRD-006 9, RFC-013). Owner/admin only: a member or viewer who
// reaches this route sees a "managed by owners/admins" message instead of an
// error, and the nav entry is already hidden for them (can("billing.view")). The
// server re-checks and 403s the entitlements call for non-owner/admin anyway.
//
// Three sections: the current plan, usage meters with used/cap bars (a near-cap
// bar turns warning-colored) plus read-only plan facts, and a plan comparison
// from GET /plans with the current tier highlighted. Stripe checkout is phased
// (not in scope): the per-tier Upgrade button opens a "coming soon / contact us"
// modal, it never starts a real checkout.

// The plan tiers in catalog order, so the comparison table rows are stable
// regardless of the order the API returns them in.
const PLAN_ORDER: Plan[] = ["tier1", "tier2", "tier3", "tierCustom"];

const PLAN_LABEL: Record<Plan, MessageKey> = {
  tier1: "plan.tier1",
  tier2: "plan.tier2",
  tier3: "plan.tier3",
  tierCustom: "plan.tierCustom",
};

// A bar is "near cap" once it crosses this fraction of the limit, which flips it
// to a warning color so an org sees a limit coming before it blocks a write.
const NEAR_CAP = 0.8;

interface Meter {
  labelKey: MessageKey;
  used: number;
  cap: number;
}

@customElement("billing-view")
export class BillingView extends AppElement {
  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private ent: Entitlements | null = null;
  @state() private plans: PlanCatalogEntry[] | null = null;
  @state() private payments: Payment[] | null = null;
  @state() private error: string | null = null;
  // the tier whose upgrade modal is open, or null when the modal is closed. Used only
  // for tierCustom now (Custom is contact-us, not self-serve, RFC-018 7).
  @state() private upgradeTo: Plan | null = null;
  // monthly/annual toggle for self-serve checkout (RFC-018 6).
  @state() private cycle: "monthly" | "annual" = "monthly";
  // a checkout/portal call is in flight (disables the buttons), and its error.
  @state() private billingBusy = false;
  @state() private billingError: string | null = null;

  private loadedOrgId: string | null = null;

  private get orgId(): string | null {
    return this.ctx?.activeOrg?.org_id ?? null;
  }

  // redirectTo sends the browser to the provider URL. A method (not an inline
  // window.location assignment) so a test can stub it instead of navigating.
  protected redirectTo(url: string): void {
    window.location.href = url;
  }

  // startCheckout buys a paid plan: it asks the server for a hosted-checkout URL and
  // redirects there. Custom is not self-serve, so only tier2/tier3 reach here.
  private async startCheckout(plan: Plan): Promise<void> {
    const orgId = this.orgId;
    if (!orgId) return;
    this.billingBusy = true;
    this.billingError = null;
    try {
      const { url } = await client.createBillingCheckout(orgId, {
        plan,
        cycle: this.cycle,
      });
      // stash where to come back to: /checkout (Paddle's default payment link) reads
      // this after the overlay closes, since the _ptxn redirect carries no org id.
      try {
        window.sessionStorage.setItem(
          "pulse.checkout.return",
          window.location.pathname,
        );
      } catch {
        // sessionStorage may be blocked; checkout falls back to /account.
      }
      this.redirectTo(url);
    } catch (err) {
      this.billingError =
        err instanceof ApiError ? err.message : t("state.error");
      this.billingBusy = false;
    }
  }

  // openPortal sends the customer to the provider portal to manage card / invoices /
  // self-cancel.
  private async openPortal(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId) return;
    this.billingBusy = true;
    this.billingError = null;
    try {
      const { url } = await client.createBillingPortal(orgId);
      this.redirectTo(url);
    } catch (err) {
      this.billingError =
        err instanceof ApiError ? err.message : t("state.error");
      this.billingBusy = false;
    }
  }

  override updated(): void {
    if (!this.hasAccess) return;
    const orgId = this.ctx?.activeOrg?.org_id ?? null;
    if (orgId && orgId !== this.loadedOrgId) void this.load(orgId);
  }

  private get hasAccess(): boolean {
    return can(this.ctx?.role ?? null, "billing.view");
  }

  private async load(orgId: string): Promise<void> {
    this.loadedOrgId = orgId;
    this.error = null;
    try {
      const [ent, plans, payments] = await Promise.all([
        client.entitlements(orgId),
        client.listPlans(),
        client.listBillingPayments(orgId),
      ]);
      this.ent = ent;
      this.plans = plans;
      this.payments = payments;
    } catch (err) {
      this.error = err instanceof ApiError ? err.message : t("state.error");
      this.ent = null;
      this.plans = null;
      this.payments = null;
    }
  }

  private retry(): void {
    const orgId = this.ctx?.activeOrg?.org_id;
    if (orgId) {
      this.loadedOrgId = null;
      void this.load(orgId);
    }
  }

  private planLabel(plan: Plan): string {
    return t(PLAN_LABEL[plan]);
  }

  override render() {
    return html`
      <div class="flex flex-col gap-8 max-w-4xl">
        <h1 class="text-2xl font-bold">${t("billing.heading")}</h1>
        ${this.body()}
      </div>
    `;
  }

  private body() {
    if (!this.hasAccess) {
      return html`<div role="status" class="alert alert-info">
        <span>${icon("lock", "size-5")}</span>
        <span>${t("billing.noAccess")}</span>
      </div>`;
    }

    if (this.error) {
      return html`<div role="alert" class="alert alert-error">
        <span>${this.error}</span>
        <button class="btn btn-sm" @click=${this.retry}>${t("state.retry")}</button>
      </div>`;
    }

    if (!this.ent || !this.plans) {
      return html`<div class="flex flex-col gap-4" aria-busy="true">
        ${Array.from({ length: 3 }).map(
          () => html`<div class="skeleton h-24 w-full"></div>`,
        )}
      </div>`;
    }

    return html`
      ${this.currentPlanSection(this.ent)} ${this.usageSection(this.ent)}
      ${this.invoicesSection()} ${this.compareSection(this.plans, this.ent.plan)}
      ${this.upgradeModal()}
    `;
  }

  // --- invoices / payments mirror (RFC-018 4) ---

  private invoicesSection() {
    const payments = this.payments;
    if (!payments || payments.length === 0) return nothing;
    return html`
      <section class="flex flex-col gap-3">
        <h2 class="text-lg font-semibold">${t("billing.invoices")}</h2>
        <div class="overflow-x-auto rounded-box border border-base-300">
          <table class="table">
            <thead>
              <tr>
                <th>${t("billing.invoiceDate")}</th>
                <th>${t("billing.invoiceAmount")}</th>
                <th>${t("billing.invoiceStatus")}</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              ${payments.map((p) => this.invoiceRow(p))}
            </tbody>
          </table>
        </div>
      </section>
    `;
  }

  private invoiceRow(p: Payment) {
    const refunded = p.refunded_amount > 0;
    return html`<tr data-payment=${p.id}>
      <td>${new Date(p.created_at).toLocaleDateString()}</td>
      <td class="tabular-nums">
        ${this.money(p.amount, p.currency)}
        ${refunded
          ? html`<span class="text-base-content/60 text-xs ml-1"
              >(${t("billing.refunded")}
              ${this.money(p.refunded_amount, p.currency)})</span
            >`
          : nothing}
      </td>
      <td><span class="badge badge-ghost">${p.status}</span></td>
      <td class="text-right">
        ${p.hosted_invoice_url
          ? html`<a
              class="link link-primary text-sm"
              href=${p.hosted_invoice_url}
              target="_blank"
              rel="noopener"
              >${t("billing.invoiceView")}</a
            >`
          : nothing}
      </td>
    </tr>`;
  }

  // money formats minor units (cents) as a plain amount + currency. Kept simple and
  // locale-safe (no Intl currency code validation) since the provider currency is
  // free-form text in the mirror.
  private money(minor: number, currency: string): string {
    return `${(minor / 100).toFixed(2)} ${currency}`;
  }

  // --- current plan ---

  private currentPlanSection(ent: Entitlements) {
    // The portal manages an existing paid subscription, so it only shows on a paid
    // plan (a free org has nothing to manage there yet).
    const paid = ent.plan !== "tier1";
    return html`
      <section class="flex flex-col gap-3">
        <h2 class="text-lg font-semibold">${t("billing.currentPlan")}</h2>
        ${this.billingError
          ? html`<div role="alert" class="alert alert-error">
              <span>${this.billingError}</span>
            </div>`
          : nothing}
        <div
          class="rounded-box border border-base-300 p-5 flex items-center gap-3"
        >
          <span class="badge badge-primary badge-lg">${this.planLabel(ent.plan)}</span>
          <span class="text-base-content/60 text-sm">${t("billing.currentPlanBadge")}</span>
          ${paid
            ? html`<button
                class="btn btn-sm btn-outline ml-auto"
                data-manage-billing
                ?disabled=${this.billingBusy}
                @click=${this.openPortal}
              >
                ${t("billing.manage")}
              </button>`
            : nothing}
        </div>
      </section>
    `;
  }

  // --- usage meters + facts ---

  private usageSection(ent: Entitlements) {
    const meters: Meter[] = [
      { labelKey: "billing.meterMonitors", used: ent.monitors_used, cap: ent.monitors_cap },
      { labelKey: "billing.meterSeats", used: ent.seats_used, cap: ent.seats_cap },
      {
        labelKey: "billing.meterStatusPages",
        used: ent.status_pages_used,
        cap: ent.status_pages_cap,
      },
    ];
    return html`
      <section class="flex flex-col gap-3">
        <div>
          <h2 class="text-lg font-semibold">${t("billing.usage")}</h2>
          <p class="text-base-content/60 text-sm">${t("billing.usageHint")}</p>
        </div>
        <div class="rounded-box border border-base-300 p-5 flex flex-col gap-5">
          ${meters.map((m) => this.meter(m))}
        </div>
        ${this.factsBlock(ent)}
      </section>
    `;
  }

  private meter(m: Meter) {
    // a zero cap means nothing is allowed; treat it as full so the bar reads as
    // at-cap rather than dividing by zero.
    const fraction = m.cap > 0 ? Math.min(m.used / m.cap, 1) : 1;
    const atCap = m.used >= m.cap;
    const near = !atCap && fraction >= NEAR_CAP;
    const color = atCap || near ? "progress-warning" : "progress-primary";
    const note = atCap
      ? t("billing.meterAtCap")
      : near
        ? t("billing.meterNearCap")
        : null;
    return html`<div class="flex flex-col gap-1" data-meter=${m.labelKey}>
      <div class="flex items-baseline justify-between gap-2">
        <span class="font-medium">${t(m.labelKey)}</span>
        <span class="text-sm text-base-content/70"
          >${tDynamic("billing.meterUsed", "", { used: m.used, cap: m.cap })}</span
        >
      </div>
      <progress
        class="progress ${color} w-full"
        value=${m.used}
        max=${m.cap > 0 ? m.cap : 1}
        aria-label=${t(m.labelKey)}
      ></progress>
      ${note
        ? html`<span class="text-warning text-xs font-medium" role="status"
            >${note}</span
          >`
        : nothing}
    </div>`;
  }

  private factsBlock(ent: Entitlements) {
    const facts: { labelKey: MessageKey; value: string | ReturnType<typeof html> }[] = [
      {
        labelKey: "billing.factMinInterval",
        value: formatDuration(ent.min_interval_seconds),
      },
      {
        labelKey: "billing.factRetention",
        value: tDynamic("billing.factRetentionValue", "", { days: ent.retention_days }),
      },
      {
        labelKey: "billing.factRegions",
        value: ent.regions_allowed.join(", "),
      },
      {
        labelKey: "billing.factCustomDomain",
        value: this.boolFact(ent.custom_domain_allowed),
      },
      {
        labelKey: "billing.factApiAccess",
        value: this.boolFact(ent.api_write_allowed),
      },
    ];
    return html`<dl
      class="rounded-box border border-base-300 p-5 grid grid-cols-1 sm:grid-cols-2 gap-x-8 gap-y-3"
    >
      ${facts.map(
        (f) => html`<div class="flex items-center justify-between gap-3">
          <dt class="text-base-content/70">${t(f.labelKey)}</dt>
          <dd class="font-medium text-right">${f.value}</dd>
        </div>`,
      )}
    </dl>`;
  }

  private boolFact(value: boolean) {
    return value
      ? html`<span class="text-success inline-flex items-center gap-1"
          >${icon("check", "size-4")}${t("billing.included")}</span
        >`
      : html`<span class="text-base-content/50">${t("billing.notIncluded")}</span>`;
  }

  // --- plan comparison ---

  private compareSection(plans: PlanCatalogEntry[], current: Plan) {
    const byPlan = new Map(plans.map((p) => [p.plan, p]));
    const ordered = PLAN_ORDER.map((p) => byPlan.get(p)).filter(
      (p): p is PlanCatalogEntry => p !== undefined,
    );
    const currentRank = PLAN_ORDER.indexOf(current);
    return html`
      <section class="flex flex-col gap-3">
        <div class="flex items-end justify-between gap-3 flex-wrap">
          <div>
            <h2 class="text-lg font-semibold">${t("billing.compare")}</h2>
            <p class="text-base-content/60 text-sm">${t("billing.compareHint")}</p>
          </div>
          <div class="join" role="group" aria-label=${t("billing.cycle")}>
            <button
              class=${`btn btn-sm join-item ${this.cycle === "monthly" ? "btn-active" : ""}`}
              @click=${() => (this.cycle = "monthly")}
            >
              ${t("billing.monthly")}
            </button>
            <button
              class=${`btn btn-sm join-item ${this.cycle === "annual" ? "btn-active" : ""}`}
              @click=${() => (this.cycle = "annual")}
            >
              ${t("billing.annual")}
            </button>
          </div>
        </div>
        <div class="overflow-x-auto rounded-box border border-base-300">
          <table class="table">
            <thead>
              <tr>
                <th>${t("billing.colPlan")}</th>
                <th>${t("billing.colMonitors")}</th>
                <th>${t("billing.colInterval")}</th>
                <th>${t("billing.colSeats")}</th>
                <th>${t("billing.colStatusPages")}</th>
                <th>${t("billing.colRetention")}</th>
                <th>${t("billing.colApiRate")}</th>
                <th>${t("billing.colChannels")}</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              ${ordered.map((p) => this.planRow(p, current, currentRank))}
            </tbody>
          </table>
        </div>
      </section>
    `;
  }

  private planRow(p: PlanCatalogEntry, current: Plan, currentRank: number) {
    const isCurrent = p.plan === current;
    const isHigher = PLAN_ORDER.indexOf(p.plan) > currentRank;
    return html`<tr
      class=${isCurrent ? "bg-primary/5" : ""}
      data-plan=${p.plan}
      data-current=${isCurrent ? "true" : "false"}
    >
      <td class="font-medium">
        <div class="flex items-center gap-2">
          ${this.planLabel(p.plan)}
          ${isCurrent
            ? html`<span class="badge badge-primary badge-sm"
                >${t("billing.currentTier")}</span
              >`
            : nothing}
        </div>
      </td>
      <td>${p.monitors_cap}</td>
      <td>${formatDuration(p.min_interval_seconds)}</td>
      <td>${p.seats_cap}</td>
      <td>${p.status_pages_cap}</td>
      <td>${tDynamic("billing.factRetentionValue", "", { days: p.retention_days })}</td>
      <td>${tDynamic("billing.colApiRateValue", "", { count: p.api_rate_per_min })}</td>
      <td>${p.channel_types.length}</td>
      <td class="text-right">
        ${isHigher ? this.upgradeButton(p.plan) : nothing}
      </td>
    </tr>`;
  }

  // The upgrade affordance differs by tier: tier2/tier3 are self-serve, so the button
  // starts a real hosted checkout; tierCustom is contract-negotiated (RFC-018 7), so it
  // opens the contact modal instead of a checkout.
  private upgradeButton(plan: Plan) {
    if (plan === "tierCustom") {
      return html`<button
        class="btn btn-sm btn-outline"
        data-upgrade=${plan}
        @click=${() => (this.upgradeTo = plan)}
      >
        ${t("billing.contactUs")}
      </button>`;
    }
    return html`<button
      class="btn btn-sm btn-primary"
      data-upgrade=${plan}
      data-checkout=${plan}
      ?disabled=${this.billingBusy}
      @click=${() => this.startCheckout(plan)}
    >
      ${t("billing.upgrade")}
    </button>`;
  }

  // --- contact CTA for Custom (contract-negotiated, never self-serve) ---

  private upgradeModal() {
    const plan = this.upgradeTo;
    if (!plan) return nothing;
    // Stripe checkout is roadmap-phased, so this is the whole "upgrade" affordance
    // for now: a clear coming-soon note plus a mailto contact, never a fake
    // checkout flow.
    const subject = encodeURIComponent(`Upgrade to ${this.planLabel(plan)}`);
    const mailto = `mailto:sales@pulse.example?subject=${subject}`;
    return html`<div
      class="modal modal-open"
      role="dialog"
      aria-modal="true"
      aria-labelledby="upgrade-heading"
      data-upgrade-modal
    >
      <div class="modal-box">
        <h3 id="upgrade-heading" class="text-lg font-bold">
          ${t("billing.upgradeHeading")}
        </h3>
        <p class="py-4">
          ${tDynamic("billing.upgradeBody", "", { plan: this.planLabel(plan) })}
        </p>
        <div class="modal-action">
          <button class="btn" @click=${() => (this.upgradeTo = null)}>
            ${t("billing.upgradeClose")}
          </button>
          <a class="btn btn-primary" href=${mailto}>
            ${t("billing.upgradeContact")}
          </a>
        </div>
      </div>
      <div
        class="modal-backdrop"
        @click=${() => (this.upgradeTo = null)}
      ></div>
    </div>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "billing-view": BillingView;
  }
}
