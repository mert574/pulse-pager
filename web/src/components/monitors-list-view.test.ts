import { expect, fixture, html, waitUntil } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./monitors-list-view.js";
import type { MonitorsListView } from "./monitors-list-view.js";
import { appContext, type AppContext } from "../state/context.js";
import { subscribeToasts } from "../toast.js";
import type {
  Entitlements,
  MonitorListItem,
  OrgMembership,
  Role,
} from "../api/types.js";

const ORG: OrgMembership = {
  org_id: "o1",
  name: "Org One",
  slug: "org-one",
  role: "owner",
  plan: "team",
};

const MONITORS: MonitorListItem[] = [
  {
    id: "mon_1",
    name: "Marketing site",
    url: "https://example.com",
    enabled: true,
    status: "up",
    last_check_at: "2026-06-21T10:00:00Z",
    next_check_at: null,
    interval_seconds: 60,
    last_latency_ms: 95,
    incident_open: false,
  },
  {
    id: "mon_2",
    name: "Prod API",
    url: "https://api.example.com",
    enabled: true,
    status: "down",
    last_check_at: "2026-06-21T10:01:00Z",
    next_check_at: null,
    interval_seconds: 60,
    last_latency_ms: 540,
    incident_open: true,
  },
];

function entitlements(used: number, cap: number): Entitlements {
  return {
    plan: "team",
    monitors_used: used,
    monitors_cap: cap,
    seats_used: 1,
    seats_cap: 10,
    status_pages_used: 0,
    status_pages_cap: 3,
    min_interval_seconds: 60,
    retention_days: 90,
    regions_allowed: ["eu-central"],
    regions_per_monitor_cap: 4,
    custom_domain_allowed: true,
    api_write_allowed: true,
    failure_snapshot: true,
  };
}

type Handler = (url: string) => Response;

function installFetch(handler: Handler): () => void {
  const original = globalThis.fetch;
  globalThis.fetch = ((input: RequestInfo | URL) =>
    Promise.resolve(handler(String(input)))) as typeof fetch;
  return () => (globalThis.fetch = original);
}

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

async function mount(opts: {
  role?: Role;
  ent?: Entitlements;
}): Promise<MonitorsListView> {
  const host = document.createElement("div");
  const ctx: AppContext = {
    me: { user_id: "u", email: "e", name: "n", avatar_url: null, orgs: [ORG] },
    activeOrg: { ...ORG, role: opts.role ?? "owner" },
    role: opts.role ?? "owner",
    entitlements: opts.ent ?? entitlements(2, 100),
    refreshMe: async () => {},
  };
  new ContextProvider(host, { context: appContext, initialValue: ctx });
  document.body.appendChild(host);
  host.innerHTML = "<monitors-list-view></monitors-list-view>";
  const el = host.querySelector<MonitorsListView>("monitors-list-view")!;
  await el.updateComplete;
  return el;
}

describe("monitors-list-view", () => {
  it("renders a row per monitor with status and incident marker", async () => {
    const restore = installFetch(() => jsonResponse(200, MONITORS));
    try {
      const el = await mount({});
      await waitUntil(() => el.querySelector("table") !== null, "table renders");
      expect(el.querySelectorAll("tbody tr").length).to.equal(2);
      expect(el.textContent).to.contain("Marketing site");
      expect(el.querySelectorAll("status-badge").length).to.equal(2);
      // the down monitor shows the incident marker
      expect(el.textContent).to.contain("Incident open");
    } finally {
      restore();
    }
  });

  it("shows the empty state when there are no monitors", async () => {
    const restore = installFetch(() => jsonResponse(200, []));
    try {
      const el = await mount({});
      await waitUntil(
        () => el.textContent?.includes("No monitors yet") ?? false,
        "empty state",
      );
      expect(el.querySelector("table")).to.be.null;
    } finally {
      restore();
    }
  });

  it("shows an error with retry when the request fails", async () => {
    const restore = installFetch(() =>
      jsonResponse(500, { error: { code: "internal_error", message: "boom" } }),
    );
    try {
      const el = await mount({});
      await waitUntil(
        () => el.querySelector('[role="alert"]') !== null,
        "error state",
      );
      expect(el.querySelector('[role="alert"]')?.textContent).to.contain("boom");
      expect(el.textContent).to.contain("Retry");
    } finally {
      restore();
    }
  });

  it("enables New monitor under the cap (member+)", async () => {
    const restore = installFetch(() => jsonResponse(200, MONITORS));
    try {
      const el = await mount({ ent: entitlements(2, 100) });
      await waitUntil(() => el.querySelector("table") !== null);
      const btn = el.querySelector("a.btn-primary");
      expect(btn).to.not.be.null;
      expect(el.querySelector("upsell-banner")).to.be.null;
    } finally {
      restore();
    }
  });

  it("disables New monitor and shows an upsell at the cap", async () => {
    const restore = installFetch(() => jsonResponse(200, MONITORS));
    try {
      const el = await mount({ ent: entitlements(100, 100) });
      await waitUntil(() => el.querySelector("table") !== null);
      const btn = el.querySelector("button.btn-primary");
      expect(btn).to.not.be.null;
      expect((btn as HTMLButtonElement).disabled).to.be.true;
      expect(el.querySelector("upsell-banner")).to.not.be.null;
    } finally {
      restore();
    }
  });

  it("hides New monitor for a viewer", async () => {
    const restore = installFetch(() => jsonResponse(200, MONITORS));
    try {
      const el = await mount({ role: "viewer" });
      await waitUntil(() => el.querySelector("table") !== null);
      expect(el.querySelector(".btn-primary")).to.be.null;
    } finally {
      restore();
    }
  });

  it("shows a check-now action and enable toggle per row for member+", async () => {
    const restore = installFetch(() => jsonResponse(200, MONITORS));
    try {
      const el = await mount({ role: "member" });
      await waitUntil(() => el.querySelector("table") !== null);
      expect(el.textContent).to.contain("Check now");
      expect(el.querySelector('input[type="checkbox"].toggle')).to.not.be.null;
    } finally {
      restore();
    }
  });

  it("hides row actions and the enable toggle for a viewer", async () => {
    const restore = installFetch(() => jsonResponse(200, MONITORS));
    try {
      const el = await mount({ role: "viewer" });
      await waitUntil(() => el.querySelector("table") !== null);
      expect(el.textContent).to.not.contain("Check now");
      expect(el.querySelector('input[type="checkbox"].toggle')).to.be.null;
    } finally {
      restore();
    }
  });

  it("queues a check on demand and shows scheduled region chips", async () => {
    const restore = installFetch((url) => {
      const path = url.split("?")[0];
      if (path.endsWith("/check"))
        return jsonResponse(202, {
          monitor_id: "mon_1",
          regions: [
            { region: "eu-central", state: "scheduled", updated_at: "2026-06-21T11:00:00Z" },
          ],
        });
      // the live-state poll: empty until the check is queued
      if (path.endsWith("/monitor-region-states"))
        return jsonResponse(200, { monitors: {} });
      return jsonResponse(200, MONITORS);
    });
    try {
      const el = await mount({ role: "member" });
      await waitUntil(() => el.querySelector("table") !== null);
      const checkBtn = Array.from(el.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("Check now"),
      )!;
      checkBtn.click();
      await waitUntil(
        () => el.querySelector("region-chips") !== null,
        "scheduled chips render after the check is queued",
      );
      expect(el.querySelector("region-chips")?.textContent).to.contain("eu-central");
      expect(el.querySelector("region-chips")?.textContent).to.contain("scheduled");
    } finally {
      restore();
    }
  });

  it("shows a 429 rate-limit toast with a countdown", async () => {
    const restore = installFetch((url) => {
      const path = url.split("?")[0];
      if (path.endsWith("/check"))
        return jsonResponse(429, {
          error: {
            code: "check_now_rate_limited",
            message: "rate limited",
            fields: { retry_after: "30", limit: "burst", upgrade: "team" },
          },
        });
      if (path.endsWith("/monitor-region-states"))
        return jsonResponse(200, { monitors: {} });
      return jsonResponse(200, MONITORS);
    });
    const seen: string[] = [];
    const unsub = subscribeToasts((toasts) =>
      seen.push(...toasts.map((t) => t.message)),
    );
    try {
      const el = await mount({ role: "member" });
      await waitUntil(() => el.querySelector("table") !== null);
      const checkBtn = Array.from(el.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("Check now"),
      )!;
      checkBtn.click();
      await waitUntil(
        () => seen.some((m) => m.includes("30s")),
        "rate-limit toast shows the retry countdown",
      );
      expect(seen.some((m) => m.includes("upgrade"))).to.be.true;
    } finally {
      unsub();
      restore();
    }
  });

  it("renders live region chips from the poll", async () => {
    const restore = installFetch((url) => {
      const path = url.split("?")[0];
      if (path.endsWith("/monitor-region-states"))
        return jsonResponse(200, {
          monitors: {
            mon_1: [
              {
                region: "eu-central",
                state: "done",
                healthy: true,
                latency_ms: 88,
                status_code: 200,
                updated_at: "2026-06-21T11:00:00Z",
              },
            ],
          },
        });
      return jsonResponse(200, MONITORS);
    });
    try {
      const el = await mount({ role: "member" });
      await waitUntil(
        () => el.querySelector("region-chips") !== null,
        "chips render from the poll",
      );
      expect(el.querySelector("region-chips")?.textContent).to.contain("eu-central");
      expect(el.querySelector("region-chips")?.textContent).to.contain("ok");
    } finally {
      restore();
    }
  });
});
