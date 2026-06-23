import { expect, waitUntil } from "@open-wc/testing";
import "./admin-view.js";
import type { AdminView } from "./admin-view.js";
import type { AdminMetrics } from "../api/types.js";

const METRICS: AdminMetrics = {
  users: 42,
  orgs: 17,
  monitors_total: 128,
  monitors_enabled: 113,
  monitors_disabled: 15,
  channels: 54,
  orgs_with_monitor: 12,
  median_time_to_first_monitor_seconds: 3600,
  active_orgs_7d: 9,
  orgs_by_plan: [
    { plan: "tier1", count: 9 },
    { plan: "tier2", count: 4 },
    { plan: "tier3", count: 3 },
    { plan: "tierCustom", count: 1 },
  ],
  monitors_by_type: [
    { type: "http", count: 104 },
    { type: "ssl", count: 24 },
  ],
  signups: [
    { date: "2026-05-25", users: 2, orgs: 1 },
    { date: "2026-06-23", users: 3, orgs: 0 },
  ],
};

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

async function mount(handler: (c: Call) => Response): Promise<{
  el: AdminView;
  calls: Call[];
  restore: () => void;
}> {
  const { calls, restore } = installFetch(handler);
  const host = document.createElement("div");
  document.body.appendChild(host);
  host.innerHTML = "<admin-view></admin-view>";
  const el = host.querySelector<AdminView>("admin-view")!;
  await el.updateComplete;
  return { el, calls, restore };
}

describe("admin-view", () => {
  it("loads and renders the platform totals", async () => {
    const { el, calls, restore } = await mount(() => json(200, METRICS));
    try {
      await waitUntil(
        () => el.textContent?.includes("42") ?? false,
        "totals render",
      );
      expect(calls.some((c) => c.url.endsWith("/admin/metrics"))).to.be.true;
      // core totals and the enabled/disabled sub-line
      expect(el.textContent).to.contain("128");
      expect(el.textContent).to.contain("113");
      expect(el.textContent).to.contain("15");
      // plan breakdown table renders a row per tier
      expect(el.textContent).to.contain("9");
      // monitors-by-type breakdown
      expect(el.textContent).to.contain("Monitors by type");
      expect(el.textContent).to.contain("104");
      expect(el.textContent).to.contain("24");
      // signup trend totals (2 + 3 new users)
      expect(el.textContent).to.contain("5");
    } finally {
      restore();
    }
  });

  it("shows a forbidden message on a 403", async () => {
    const { el, restore } = await mount(() =>
      json(403, { error: { code: "forbidden", message: "platform admin only" } }),
    );
    try {
      await waitUntil(
        () => /do not have access/i.test(el.textContent ?? ""),
        "forbidden renders",
      );
      expect(el.textContent ?? "").to.match(/do not have access/i);
    } finally {
      restore();
    }
  });
});
