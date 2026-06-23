import { expect, waitUntil } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./billing-view.js";
import type { BillingView } from "./billing-view.js";
import { appContext, type AppContext } from "../state/context.js";
import type {
  Entitlements,
  OrgMembership,
  PlanCatalogEntry,
  Role,
} from "../api/types.js";

const ORG: OrgMembership = {
  org_id: "o1",
  name: "Org One",
  slug: "org-one",
  role: "owner",
  plan: "tier3",
};

// Team org: monitors near cap (9/10), seats well under, status pages under.
const ENT: Entitlements = {
  plan: "tier3",
  monitors_used: 9,
  monitors_cap: 10,
  seats_used: 2,
  seats_cap: 10,
  status_pages_used: 1,
  status_pages_cap: 5,
  min_interval_seconds: 60,
  retention_days: 90,
  regions_allowed: ["us-east", "us-west"],
  regions_per_monitor_cap: 2,
  custom_domain_allowed: true,
  api_access_allowed: true,
  api_write_allowed: true,
  failure_snapshot: true,
};

function plan(p: PlanCatalogEntry["plan"], over: Partial<PlanCatalogEntry>): PlanCatalogEntry {
  return {
    plan: p,
    monitors_cap: 10,
    min_interval_seconds: 60,
    seats_cap: 10,
    status_pages_cap: 5,
    retention_days: 90,
    regions_allowed: ["us-east"],
    regions_per_monitor_cap: 1,
    custom_domain_allowed: false,
    api_access_allowed: false,
    api_write_allowed: false,
    api_rate_per_min: 60,
    channel_types: ["email"],
    ...over,
  };
}

const PLANS: PlanCatalogEntry[] = [
  plan("tier1", { monitors_cap: 3, min_interval_seconds: 300, seats_cap: 1, status_pages_cap: 1, retention_days: 7, api_rate_per_min: 30, channel_types: ["email"] }),
  plan("tier2", { monitors_cap: 5, min_interval_seconds: 120, seats_cap: 3, status_pages_cap: 2, retention_days: 30, api_rate_per_min: 60, channel_types: ["email", "slack"] }),
  plan("tier3", { monitors_cap: 10, min_interval_seconds: 60, seats_cap: 10, status_pages_cap: 5, retention_days: 90, api_rate_per_min: 120, channel_types: ["email", "slack", "webhook"] }),
  plan("tierCustom", { monitors_cap: 50, min_interval_seconds: 30, seats_cap: 50, status_pages_cap: 20, retention_days: 365, api_rate_per_min: 600, channel_types: ["email", "slack", "webhook", "pagerduty"] }),
];

interface Call {
  url: string;
  method: string;
}

function installFetch(handler: (c: Call) => Response): {
  calls: Call[];
  restore: () => void;
} {
  const calls: Call[] = [];
  const original = globalThis.fetch;
  globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    const call: Call = {
      url: String(input).split("?")[0],
      method: init?.method ?? "GET",
    };
    calls.push(call);
    return Promise.resolve(handler(call));
  }) as typeof fetch;
  return { calls, restore: () => (globalThis.fetch = original) };
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

// default handler: entitlements + plans both 200
function defaultHandler(c: Call): Response {
  if (c.url.endsWith("/entitlements")) return json(200, ENT);
  if (c.url.endsWith("/plans")) return json(200, PLANS);
  return json(404, { error: { code: "not_found", message: "nope" } });
}

async function mount(opts: {
  role?: Role;
  handler?: (c: Call) => Response;
}): Promise<{ el: BillingView; calls: Call[]; restore: () => void }> {
  const { calls, restore } = installFetch(opts.handler ?? defaultHandler);
  const host = document.createElement("div");
  const ctx: AppContext = {
    me: { user_id: "u", email: "e", name: "n", avatar_url: null, locale: "en", timezone: "UTC", orgs: [ORG] },
    activeOrg: { ...ORG, role: opts.role ?? "owner" },
    role: opts.role ?? "owner",
    entitlements: null,
    refreshMe: async () => {},
  };
  new ContextProvider(host, { context: appContext, initialValue: ctx });
  document.body.appendChild(host);
  host.innerHTML = "<billing-view></billing-view>";
  const el = host.querySelector<BillingView>("billing-view")!;
  await el.updateComplete;
  return { el, calls, restore };
}

describe("billing-view", () => {
  it("renders the current plan and usage meters with a near-cap warning", async () => {
    const { el, restore } = await mount({});
    try {
      await waitUntil(
        () => el.querySelector("[data-meter]") !== null,
        "meters render",
      );
      // current plan badge (team plan renders as its display name "Professional")
      expect(el.textContent).to.contain("Professional");
      // three meters, one per metered dimension
      expect(el.querySelectorAll("[data-meter]").length).to.equal(3);
      // monitors bar reflects used/cap
      const monitorsMeter = el.querySelector<HTMLElement>(
        '[data-meter="billing.meterMonitors"]',
      )!;
      const bar = monitorsMeter.querySelector("progress")!;
      expect(bar.getAttribute("value")).to.equal("9");
      expect(bar.getAttribute("max")).to.equal("10");
      // 9/10 is over the near-cap threshold: warning color + a near-cap note
      expect(bar.className).to.contain("progress-warning");
      expect(monitorsMeter.textContent).to.contain("Near your plan limit");
      // a comfortably-under meter is not warning-colored
      const seatsMeter = el.querySelector<HTMLElement>(
        '[data-meter="billing.meterSeats"]',
      )!;
      expect(seatsMeter.querySelector("progress")!.className).to.contain(
        "progress-primary",
      );
    } finally {
      restore();
    }
  });

  it("renders the plan comparison with the current plan highlighted", async () => {
    const { el, restore } = await mount({});
    try {
      await waitUntil(() => el.querySelector("table") !== null, "table renders");
      // a row per tier from /plans
      expect(el.querySelectorAll("tbody tr").length).to.equal(4);
      // the current (team) row is marked current; lower/other rows are not
      const teamRow = el.querySelector<HTMLElement>('tr[data-plan="tier3"]')!;
      expect(teamRow.dataset.current).to.equal("true");
      expect(teamRow.textContent).to.contain("Current");
      const freeRow = el.querySelector<HTMLElement>('tr[data-plan="tier1"]')!;
      expect(freeRow.dataset.current).to.equal("false");
    } finally {
      restore();
    }
  });

  it("offers Upgrade only on higher tiers and opens a contact/coming-soon affordance, not a checkout", async () => {
    const { el, calls, restore } = await mount({});
    try {
      await waitUntil(() => el.querySelector("table") !== null);
      // upgrade only on the higher tier (business), not on free/starter/team
      expect(el.querySelector('[data-upgrade="tier1"]')).to.be.null;
      expect(el.querySelector('[data-upgrade="tier3"]')).to.be.null;
      const upgrade = el.querySelector<HTMLButtonElement>(
        '[data-upgrade="tierCustom"]',
      )!;
      expect(upgrade).to.not.be.null;
      upgrade.click();
      await waitUntil(
        () => el.querySelector("[data-upgrade-modal]") !== null,
        "upgrade modal opens",
      );
      const modal = el.querySelector<HTMLElement>("[data-upgrade-modal]")!;
      expect(modal.textContent).to.contain("coming soon");
      // the only link is a mailto contact, never a checkout URL
      const link = modal.querySelector<HTMLAnchorElement>("a[href]")!;
      expect(link.getAttribute("href")).to.match(/^mailto:/);
      // no checkout request was fired
      expect(calls.some((c) => c.url.includes("checkout"))).to.be.false;
    } finally {
      restore();
    }
  });

  it("shows the no-access message for a member and does not fetch", async () => {
    const { el, calls, restore } = await mount({ role: "member" });
    try {
      await el.updateComplete;
      expect(el.textContent).to.contain("managed by owners and admins");
      // a member never reaches the entitlements/plans calls
      expect(calls.length).to.equal(0);
      expect(el.querySelector("[data-meter]")).to.be.null;
      expect(el.querySelector("table")).to.be.null;
    } finally {
      restore();
    }
  });
});
