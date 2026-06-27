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
import { pageShell, pageHeader, errorBox } from "./ui.js";
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
// Broadsheet pricing layout: a masthead, an inked current-plan band (the plan name
// set big with a current badge), a usage block of thin used/cap token bars (a near-
// cap bar turns warning-colored) plus read-only plan facts, a plan-comparison grid
// where each plan reads as a newspaper column with a huge display price and the
// current plan wears an inked top band, then the invoices ledger. Stripe checkout is
// phased: the per-tier Upgrade button opens a "coming soon / contact us" modal for
// Custom; tier2/tier3 start a real hosted checkout.

// The plan tiers in catalog order, so the comparison cards are stable regardless
// of the order the API returns them in.
const PLAN_ORDER: Plan[] = ["tier1", "tier2", "tier3", "tierCustom"];

const PLAN_LABEL: Record<Plan, MessageKey> = {
  tier1: "plan.tier1",
  tier2: "plan.tier2",
  tier3: "plan.tier3",
  tierCustom: "plan.tierCustom",
};

// One-line "who it's for" tagline per plan, same copy as the public pricing page.
const PLAN_TAGLINE: Record<Plan, MessageKey> = {
  tier1: "billing.tier1For",
  tier2: "billing.tier2For",
  tier3: "billing.tier3For",
  tierCustom: "billing.tierCustomFor",
};

type PriceInfo =
  | { kind: "free" }
  | { kind: "paid"; monthly: number; annual: number }
  | { kind: "custom"; from: number };

// Published cloud prices in whole dollars (docs-site/pricing.html and PRD.md).
// The catalog API returns caps, not price, and the real charge always comes from
// the provider price id at checkout, so these are display-only. Annual is ten
// months' price (two months free), so the saving is computed, not hard-coded.
const PLAN_PRICE: Record<Plan, PriceInfo> = {
  tier1: { kind: "free" },
  tier2: { kind: "paid", monthly: 7, annual: 70 },
  tier3: { kind: "paid", monthly: 19, annual: 190 },
  tierCustom: { kind: "custom", from: 129 },
};

// Free-trial length by billing cycle, display copy matching each provider
// price's trial_days: a shorter trial on monthly, a longer one on annual. Only
// the self-serve paid plans carry a trial (Free needs none, Custom is contract).
const CYCLE_TRIAL_DAYS: Record<"monthly" | "annual", number> = {
  monthly: 3,
  annual: 7,
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

  // The no-access / error / loading states keep the simple padded shell. The full
  // broadsheet (masthead + hero + sections) only renders once entitlements and the
  // plan catalog are in hand.
  override render() {
    if (!this.hasAccess) {
      return pageShell(
        t("billing.heading"),
        nothing,
        html`<div
          role="status"
          class="flex items-center gap-3 border border-hair bg-paper px-4 py-3 text-ink2"
        >
          <span>${icon("lock", "size-5")}</span>
          <span>${t("billing.noAccess")}</span>
        </div>`,
      );
    }
    if (this.error) {
      return pageShell(
        t("billing.heading"),
        nothing,
        errorBox(this.error, () => this.retry(), t("state.retry")),
      );
    }
    if (!this.ent || !this.plans) {
      return pageShell(
        t("billing.heading"),
        nothing,
        html`<div class="flex flex-col gap-4" aria-busy="true">
          ${Array.from({ length: 3 }).map(
            () => html`<div class="h-24 w-full bg-paper animate-pulse"></div>`,
          )}
        </div>`,
      );
    }

    const ent = this.ent;
    const plans = this.plans;
    return html`
      <div class="-mx-6 lg:-mx-10 -my-7">
        ${pageHeader(t("billing.heading"), this.manageAction(ent))}
        ${this.currentPlanBand(ent)}
        <div class="px-6 lg:px-10 py-7 flex flex-col gap-12">
          ${this.billingError
            ? html`<div role="alert" class="border border-down px-4 py-3 text-down">
                ${this.billingError}
              </div>`
            : nothing}
          ${this.usageSection(ent)}
          ${this.compareSection(plans, ent.plan)}
          ${this.invoicesSection()}
        </div>
        ${this.upgradeModal()}
      </div>
    `;
  }

  // The masthead action: a paid org gets the "Manage billing" portal button (a free
  // org has nothing to manage there yet).
  private manageAction(ent: Entitlements) {
    if (ent.plan === "tier1") return nothing;
    return html`<button
      class="pulse-btn pulse-btn-ghost pulse-btn-sm"
      data-manage-billing
      ?disabled=${this.billingBusy}
      @click=${this.openPortal}
    >
      ${t("billing.manage")}
    </button>`;
  }

  // --- current plan band (the bold inked masthead band) ---

  // A full-bleed inverse band (ink field, cream type, same pairing as the cream-on-
  // brand chips so it flips cleanly in dark mode): the current plan name set big with
  // a live current badge and the plan's tagline. Replaces the old hero helper.
  private currentPlanBand(ent: Entitlements) {
    return html`<div
      class="bg-ink text-cream px-6 lg:px-10 py-7 lg:py-8 border-b border-line flex flex-wrap items-end justify-between gap-x-8 gap-y-4"
    >
      <div>
        <div class="font-mono text-[10.5px] tracking-[0.16em] uppercase opacity-70">
          ${tDynamic("billing.heroPlanLabel", "Current plan", {})}
        </div>
        <div
          class="font-disp font-black leading-[0.84] tracking-[-0.05em] text-5xl sm:text-6xl lg:text-7xl mt-2.5"
        >
          ${this.planLabel(ent.plan)}
        </div>
      </div>
      <div class="font-mono text-[11px] flex flex-col items-start sm:items-end gap-1.5">
        <span class="pulse-state text-up"
          ><span class="pulse-state-sq bg-up"></span
          >${t("billing.currentPlanBadge")}</span
        >
        <span class="opacity-80">${t(PLAN_TAGLINE[ent.plan])}</span>
      </div>
    </div>`;
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
      <section class="flex flex-col gap-4">
        <div class="flex items-baseline justify-between border-b border-line pb-[11px]">
          <h2 class="pulse-section-title m-0">${t("billing.usage")}</h2>
          <span class="font-mono text-[11px] text-ink3">${t("billing.usageHint")}</span>
        </div>
        <div class="pulse-panel p-5 flex flex-col gap-5">
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
    const fill = atCap || near ? "bg-deg" : "bg-brand";
    const note = atCap
      ? t("billing.meterAtCap")
      : near
        ? t("billing.meterNearCap")
        : null;
    return html`<div class="flex flex-col gap-1.5" data-meter=${m.labelKey}>
      <div class="flex items-baseline justify-between gap-2">
        <span class="pulse-label">${t(m.labelKey)}</span>
        <span class="font-mono text-[13px] text-ink2 tabular-nums"
          >${tDynamic("billing.meterUsed", "", { used: m.used, cap: m.cap })}</span
        >
      </div>
      <div
        class="h-2 w-full bg-hair"
        role="progressbar"
        aria-label=${t(m.labelKey)}
        aria-valuenow=${m.used}
        aria-valuemax=${m.cap > 0 ? m.cap : 1}
      >
        <div
          data-meter-fill
          class="h-full ${fill}"
          style="width:${Math.round(fraction * 100)}%"
        ></div>
      </div>
      ${note
        ? html`<span class="text-deg text-xs font-medium" role="status"
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
      class="pulse-panel p-5 grid grid-cols-1 sm:grid-cols-2 gap-x-8 gap-y-3.5"
    >
      ${facts.map(
        (f) => html`<div class="flex items-center justify-between gap-3">
          <dt class="pulse-label">${t(f.labelKey)}</dt>
          <dd class="font-medium text-right text-sm">${f.value}</dd>
        </div>`,
      )}
    </dl>`;
  }

  private boolFact(value: boolean) {
    return value
      ? html`<span class="text-up inline-flex items-center gap-1"
          >${icon("check", "size-4")}${t("billing.included")}</span
        >`
      : html`<span class="text-ink3">${t("billing.notIncluded")}</span>`;
  }

  // --- plan comparison (broadsheet pricing grid) ---

  private compareSection(plans: PlanCatalogEntry[], current: Plan) {
    const byPlan = new Map(plans.map((p) => [p.plan, p]));
    const ordered = PLAN_ORDER.map((p) => byPlan.get(p)).filter(
      (p): p is PlanCatalogEntry => p !== undefined,
    );
    const currentRank = PLAN_ORDER.indexOf(current);
    return html`
      <section class="flex flex-col gap-4">
        <div
          class="flex items-end justify-between gap-3 flex-wrap border-b border-line pb-[11px]"
        >
          <div>
            <h2 class="pulse-section-title m-0">${t("billing.compare")}</h2>
            <p class="font-mono text-[11px] text-ink3 mt-1">${t("billing.compareHint")}</p>
          </div>
          <div class="flex flex-col items-end gap-1">
            <div class="flex" role="group" aria-label=${t("billing.cycle")}>
              <button
                class=${`pulse-btn pulse-btn-sm ${this.cycle === "monthly" ? "" : "pulse-btn-ghost"}`}
                @click=${() => (this.cycle = "monthly")}
              >
                ${t("billing.monthly")}
              </button>
              <button
                class=${`pulse-btn pulse-btn-sm -ml-px ${this.cycle === "annual" ? "" : "pulse-btn-ghost"}`}
                @click=${() => (this.cycle = "annual")}
              >
                ${t("billing.annual")}
              </button>
            </div>
            <span class="text-up text-xs font-medium">${t("billing.annualBadge")}</span>
          </div>
        </div>
        <div class="grid items-stretch gap-4 grid-cols-1 sm:grid-cols-2 xl:grid-cols-4">
          ${ordered.map((p) => this.planCard(p, current, currentRank))}
        </div>
      </section>
    `;
  }

  private planCard(p: PlanCatalogEntry, current: Plan, currentRank: number) {
    const isCurrent = p.plan === current;
    const isHigher = PLAN_ORDER.indexOf(p.plan) > currentRank;
    // tier3 (Professional) is the highlighted plan on the public pricing page too.
    const featured = p.plan === "tier3";
    const price = this.planPrice(p.plan);
    // Only show a trial when this person still qualifies for one. Someone who recently
    // had a subscription is trial-ineligible (RFC-018), so they get no badge here and the
    // trialless price at checkout.
    const trialDays =
      PLAN_PRICE[p.plan].kind === "paid" && this.ent?.trial_eligible
        ? CYCLE_TRIAL_DAYS[this.cycle]
        : undefined;
    // Each plan reads as a newspaper column. The current plan wears a bold inked top
    // band (ink/cream, so it flips in dark mode); the featured plan wears a brand band
    // and keeps the brand ring; the rest are quiet hairline columns.
    const frame = featured
      ? "border-brand ring-1 ring-brand"
      : isCurrent
        ? "border-line"
        : "border-hair";
    const topBand = isCurrent
      ? html`<div
          class="bg-ink text-cream font-mono text-[10px] uppercase tracking-[0.16em] px-4 py-2 flex items-center gap-2"
        >
          <span class="pulse-state-sq bg-up"></span>${t("billing.currentTier")}
        </div>`
      : featured
        ? html`<div
            class="bg-brand text-cream font-mono text-[10px] uppercase tracking-[0.16em] px-4 py-2"
          >
            ${t("billing.recommended")}
          </div>`
        : html`<div class="h-[34px] border-b border-hair"></div>`;
    return html`<div
      class=${`flex flex-col border bg-bg ${frame}`}
      data-plan=${p.plan}
      data-current=${isCurrent ? "true" : "false"}
    >
      ${topBand}
      <div class="p-5 lg:p-6 flex flex-1 flex-col gap-5">
        <span class="pulse-label">${this.planLabel(p.plan)}</span>
        <div>
          <div
            class="font-disp font-black tracking-[-0.05em] leading-[0.82] text-5xl xl:text-6xl tabular-nums"
          >
            ${price.amount}
          </div>
          <div class="font-mono text-[11px] uppercase tracking-[0.1em] text-ink3 mt-2.5">
            ${price.sub}
          </div>
          ${price.save
            ? html`<div class="flex items-center gap-2 text-xs mt-1.5 font-mono">
                ${price.struck
                  ? html`<span class="text-ink3 line-through tabular-nums"
                      >${price.struck}</span
                    >`
                  : nothing}
                <span class="text-up font-medium">${price.save}</span>
              </div>`
            : nothing}
          ${trialDays
            ? html`<span class="mt-3 inline-block w-fit border border-hair px-2 py-1 pulse-tag"
                >${tDynamic("billing.trialDays", "", { days: trialDays })}</span
              >`
            : nothing}
        </div>
        <p class="text-ink3 text-sm">${t(PLAN_TAGLINE[p.plan])}</p>
        <ul class="flex flex-col gap-2 text-sm border-t border-hair pt-4">
          ${this.planFeatures(p).map(
            (f) => html`<li class="flex items-start gap-2">
              <span class="text-up mt-0.5">${icon("check", "size-4")}</span>
              <span>${f}</span>
            </li>`,
          )}
        </ul>
        <div class="mt-auto pt-2">
          ${isCurrent
            ? html`<button class="pulse-btn pulse-btn-sm w-full" disabled>
                ${t("billing.currentTier")}
              </button>`
            : isHigher
              ? this.upgradeButton(p.plan)
              : nothing}
        </div>
      </div>
    </div>`;
  }

  // The published display price per plan (see PLAN_PRICE), with the monthly/annual
  // amount picked by the current toggle. On annual it also returns the struck
  // full-year price and the saving so the discount is visible on the card.
  private planPrice(plan: Plan): {
    amount: string;
    sub: string;
    struck?: string;
    save?: string;
  } {
    const info = PLAN_PRICE[plan];
    if (info.kind === "free") {
      return { amount: "$0", sub: t("billing.priceFree") };
    }
    if (info.kind === "custom") {
      return {
        amount: tDynamic("billing.fromPrice", "", { price: `$${info.from}` }),
        sub: t("billing.perMonthAnnual"),
      };
    }
    if (this.cycle === "annual") {
      const fullYear = info.monthly * 12;
      const saved = fullYear - info.annual;
      return {
        amount: `$${info.annual}`,
        sub: t("billing.perYear"),
        struck: `$${fullYear}`,
        save: tDynamic("billing.saveAnnual", "", { amount: `$${saved}` }),
      };
    }
    return { amount: `$${info.monthly}`, sub: t("billing.perMonth") };
  }

  // Six feature bullets per card, read straight off the live plan catalog so they
  // stay true to the enforced caps rather than drifting from marketing copy.
  private planFeatures(p: PlanCatalogEntry): string[] {
    return [
      tDynamic("billing.featChecks", "", {
        interval: formatDuration(p.min_interval_seconds),
      }),
      tDynamic("billing.featMonitors", "", { count: p.monitors_cap }),
      p.regions_per_monitor_cap > 1
        ? tDynamic("billing.featRegionsMulti", "", {
            count: p.regions_per_monitor_cap,
          })
        : t("billing.featRegionsSingle"),
      tDynamic("billing.featRetention", "", { days: p.retention_days }),
      tDynamic("billing.featStatusPages", "", { count: p.status_pages_cap }),
      !p.api_access_allowed
        ? t("billing.featApiNone")
        : p.api_write_allowed
          ? t("billing.featApiFull")
          : t("billing.featApiRead"),
    ];
  }

  // The upgrade affordance differs by tier: tier2/tier3 are self-serve, so the button
  // starts a real hosted checkout; tierCustom is contract-negotiated (RFC-018 7), so it
  // opens the contact modal instead of a checkout.
  private upgradeButton(plan: Plan) {
    if (plan === "tierCustom") {
      return html`<button
        class="pulse-btn pulse-btn-ghost pulse-btn-sm w-full"
        data-upgrade=${plan}
        @click=${() => (this.upgradeTo = plan)}
      >
        ${t("billing.contactUs")}
      </button>`;
    }
    return html`<button
      class="pulse-btn pulse-btn-sm w-full"
      data-upgrade=${plan}
      data-checkout=${plan}
      ?disabled=${this.billingBusy}
      @click=${() => this.startCheckout(plan)}
    >
      ${t("billing.upgrade")}
    </button>`;
  }

  // --- invoices / payments mirror (RFC-018 4) ---

  private invoicesSection() {
    const payments = this.payments;
    if (!payments || payments.length === 0) return nothing;
    return html`
      <section class="flex flex-col gap-4">
        <h2 class="pulse-section-title m-0 border-b border-line pb-[11px]">
          ${t("billing.invoices")}
        </h2>
        <div class="overflow-x-auto pulse-panel">
          <table class="w-full text-left text-sm">
            <thead>
              <tr class="border-b border-hair">
                <th class="px-4 py-2.5"><span class="pulse-label">${t("billing.invoiceDate")}</span></th>
                <th class="px-4 py-2.5"><span class="pulse-label">${t("billing.invoiceAmount")}</span></th>
                <th class="px-4 py-2.5"><span class="pulse-label">${t("billing.invoiceStatus")}</span></th>
                <th class="px-4 py-2.5"></th>
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
    return html`<tr data-payment=${p.id} class="border-b border-hair last:border-0">
      <td class="px-4 py-2.5 font-mono text-ink2">
        ${new Date(p.created_at).toLocaleDateString()}
      </td>
      <td class="px-4 py-2.5 font-mono tabular-nums">
        ${this.money(p.amount, p.currency)}
        ${refunded
          ? html`<span class="text-ink3 text-xs ml-1"
              >(${t("billing.refunded")}
              ${this.money(p.refunded_amount, p.currency)})</span
            >`
          : nothing}
      </td>
      <td class="px-4 py-2.5">
        <span class="pulse-tag">${p.status}</span>
      </td>
      <td class="px-4 py-2.5 text-right">
        ${p.hosted_invoice_url
          ? html`<a
              class="text-sm text-brand hover:no-underline"
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
      class="fixed inset-0 z-50 flex items-center justify-center p-4"
      role="dialog"
      aria-modal="true"
      aria-labelledby="upgrade-heading"
      data-upgrade-modal
    >
      <div
        class="absolute inset-0 bg-black/40"
        @click=${() => (this.upgradeTo = null)}
      ></div>
      <div
        class="pulse-dialog relative w-full max-w-md border border-line bg-bg p-6 flex flex-col gap-4"
      >
        <h3
          id="upgrade-heading"
          class="font-disp font-extrabold text-lg uppercase tracking-[-0.01em]"
        >
          ${t("billing.upgradeHeading")}
        </h3>
        <p class="text-ink2">
          ${tDynamic("billing.upgradeBody", "", { plan: this.planLabel(plan) })}
        </p>
        <div class="flex justify-end gap-2">
          <button
            class="pulse-btn pulse-btn-ghost"
            @click=${() => (this.upgradeTo = null)}
          >
            ${t("billing.upgradeClose")}
          </button>
          <a class="pulse-btn" href=${mailto}>
            ${t("billing.upgradeContact")}
          </a>
        </div>
      </div>
    </div>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "billing-view": BillingView;
  }
}
