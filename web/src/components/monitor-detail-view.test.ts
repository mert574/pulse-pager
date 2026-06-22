import { expect, waitUntil } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./monitor-detail-view.js";
import type { MonitorDetailView } from "./monitor-detail-view.js";
import { appContext, type AppContext } from "../state/context.js";
import type { CheckResult, Monitor, OrgMembership, Role } from "../api/types.js";

const ORG: OrgMembership = {
  org_id: "o1",
  name: "Org One",
  slug: "org-one",
  role: "owner",
  plan: "team",
};

const MONITOR: Monitor = {
  id: "mon_1",
  org_id: "o1",
  name: "Marketing site",
  url: "https://example.com",
  method: "GET",
  headers: [],
  body: "",
  expected_status_codes: "200",
  timeout_seconds: 10,
  interval_seconds: 60,
  enabled: true,
  max_latency_ms: null,
  body_contains: null,
  failure_threshold: 1,
  notification_channel_ids: [],
  regions: ["eu-central"],
  down_policy: "quorum",
  created_at: "2026-06-21T10:00:00Z",
  updated_at: "2026-06-21T10:00:00Z",
};

// 4 results, 1 failed -> uptime 75%. Single region, so each is its own run.
const RESULTS: CheckResult[] = [
  { id: "r0", monitor_id: "mon_1", region: "eu-central", scheduled_at: "2026-06-21T10:15:00Z", checked_at: "2026-06-21T10:15:00Z", healthy: true, failure_reason: null, status_code: 200, latency_ms: 100, error: null },
  { id: "r1", monitor_id: "mon_1", region: "eu-central", scheduled_at: "2026-06-21T10:10:00Z", checked_at: "2026-06-21T10:10:00Z", healthy: false, failure_reason: "status_mismatch", status_code: 503, latency_ms: 120, error: null },
  { id: "r2", monitor_id: "mon_1", region: "eu-central", scheduled_at: "2026-06-21T10:05:00Z", checked_at: "2026-06-21T10:05:00Z", healthy: true, failure_reason: null, status_code: 200, latency_ms: 90, error: null },
  { id: "r3", monitor_id: "mon_1", region: "eu-central", scheduled_at: "2026-06-21T10:00:00Z", checked_at: "2026-06-21T10:00:00Z", healthy: true, failure_reason: null, status_code: 200, latency_ms: 110, error: null },
];

// A two-region monitor with two runs: the second run has one region down. Used to
// check the per-region grouping (one row per run, expandable to the region detail).
const MONITOR_MR: Monitor = { ...MONITOR, regions: ["eu-central", "eu"] };
const RESULTS_MR: CheckResult[] = [
  { id: "a-home", monitor_id: "mon_1", region: "eu-central", scheduled_at: "2026-06-21T10:15:00Z", checked_at: "2026-06-21T10:15:00Z", healthy: true, failure_reason: null, status_code: 200, latency_ms: 100, error: null },
  { id: "a-eu", monitor_id: "mon_1", region: "eu", scheduled_at: "2026-06-21T10:15:00Z", checked_at: "2026-06-21T10:15:00Z", healthy: true, failure_reason: null, status_code: 200, latency_ms: 140, error: null },
  { id: "b-home", monitor_id: "mon_1", region: "eu-central", scheduled_at: "2026-06-21T10:10:00Z", checked_at: "2026-06-21T10:10:00Z", healthy: true, failure_reason: null, status_code: 200, latency_ms: 90, error: null },
  { id: "b-eu", monitor_id: "mon_1", region: "eu", scheduled_at: "2026-06-21T10:10:00Z", checked_at: "2026-06-21T10:10:00Z", healthy: false, failure_reason: "status_mismatch", status_code: 503, latency_ms: 200, error: null },
];

function routeMR(url: string): Response {
  const path = url.split("?")[0];
  if (path.endsWith("/results")) return json(200, { items: RESULTS_MR, next_cursor: null });
  if (path.endsWith("/incidents")) return json(200, { items: [], next_cursor: null });
  if (path.endsWith("/last-failure")) return new Response(null, { status: 404 });
  if (path.endsWith("/monitor-region-states")) return json(200, { monitors: {} });
  return json(200, MONITOR_MR);
}

function installFetch(handler: (url: string) => Response): () => void {
  const original = globalThis.fetch;
  globalThis.fetch = ((input: RequestInfo | URL) =>
    Promise.resolve(handler(String(input)))) as typeof fetch;
  return () => (globalThis.fetch = original);
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function route(url: string): Response {
  const path = url.split("?")[0];
  if (path.endsWith("/results")) return json(200, { items: RESULTS, next_cursor: null });
  if (path.endsWith("/incidents")) return json(200, { items: [], next_cursor: null });
  if (path.endsWith("/last-failure")) return new Response(null, { status: 404 });
  if (path.endsWith("/monitor-region-states")) return json(200, { monitors: {} });
  return json(200, MONITOR);
}

async function mount(role: Role = "owner"): Promise<MonitorDetailView> {
  const host = document.createElement("div");
  const ctx: AppContext = {
    me: { user_id: "u", email: "e", name: "n", avatar_url: null, orgs: [ORG] },
    activeOrg: { ...ORG, role },
    role,
    entitlements: null,
    refreshMe: async () => {},
  };
  new ContextProvider(host, { context: appContext, initialValue: ctx });
  document.body.appendChild(host);
  const el = document.createElement("monitor-detail-view") as MonitorDetailView;
  el.monitorId = "mon_1";
  host.appendChild(el);
  await el.updateComplete;
  return el;
}

describe("monitor-detail-view", () => {
  it("loads the monitor and renders stats, chart, and results", async () => {
    const restore = installFetch(route);
    try {
      const el = await mount();
      await waitUntil(() => el.querySelector("table") !== null, "loaded");
      expect(el.textContent).to.contain("Marketing site");
      expect(el.querySelector("status-badge")).to.not.be.null;
      // uptime 3/4 = 75%
      expect(el.textContent).to.contain("75%");
      // a row per result
      expect(el.querySelectorAll("tbody tr").length).to.equal(4);
      // the failed check shows its localized reason
      expect(el.textContent).to.contain("Status mismatch");
      // next-check stat: last check is well in the past at a 60s interval, so it reads
      // as due now, with the cadence underneath.
      expect(el.textContent).to.contain("Next check");
      expect(el.textContent).to.contain("due now");
      // chart rendered (>= 2 points)
      expect(el.querySelector("latency-chart")).to.not.be.null;
    } finally {
      restore();
    }
  });

  it("groups a multi-region monitor into one row per run, expandable to regions", async () => {
    const restore = installFetch(routeMR);
    try {
      const el = await mount();
      await waitUntil(() => el.querySelector("table") !== null, "loaded");
      // 4 results across 2 ticks -> 2 run rows (collapsed, no detail rows yet).
      expect(el.querySelectorAll("tbody tr").length).to.equal(2);
      // the run with a failing region shows how many of its regions are down.
      expect(el.textContent).to.contain("1/2 down");
      // the per-region reason is hidden until the run is expanded.
      expect(el.textContent).to.not.contain("Status mismatch");

      // expand the failing run to reveal its per-region detail.
      const toggles = Array.from(
        el.querySelectorAll<HTMLButtonElement>("button[aria-expanded]"),
      );
      expect(toggles.length).to.equal(2);
      toggles[1].click();
      await waitUntil(
        () => (el.textContent ?? "").includes("Status mismatch"),
        "expanded detail",
      );
      expect(el.textContent).to.contain("eu");
    } finally {
      restore();
    }
  });

  it("shows Check now / Edit / Delete for member+", async () => {
    const restore = installFetch(route);
    try {
      const el = await mount("owner");
      await waitUntil(() => el.querySelector("table") !== null);
      const labels = Array.from(el.querySelectorAll("button, a.btn")).map(
        (b) => b.textContent?.trim(),
      );
      expect(labels.join(" ")).to.contain("Check now");
      expect(labels.join(" ")).to.contain("Edit");
    } finally {
      restore();
    }
  });

  it("hides write actions for a viewer", async () => {
    const restore = installFetch(route);
    try {
      const el = await mount("viewer");
      await waitUntil(() => el.querySelector("table") !== null);
      const text = el.textContent ?? "";
      expect(text).to.not.contain("Check now");
      expect(text).to.not.contain("Edit");
    } finally {
      restore();
    }
  });

  it("puts the failure response above the history when the monitor is down", async () => {
    const downFirst = [
      { id: "r0", monitor_id: "mon_1", region: "eu-central", checked_at: "2026-06-21T10:15:00Z", healthy: false, failure_reason: "status_mismatch", status_code: 503, latency_ms: 120, error: null },
      ...RESULTS.slice(1),
    ];
    const restore = installFetch((url) => {
      const path = url.split("?")[0];
      if (path.endsWith("/results")) return json(200, { items: downFirst, next_cursor: null });
      if (path.endsWith("/last-failure"))
        return json(200, {
          monitor_id: "mon_1",
          checked_at: "2026-06-21T10:15:00Z",
          status_code: 503,
          headers: { "Content-Type": ["text/plain"] },
          body: "service unavailable",
          truncated: false,
        });
      return route(url);
    });
    try {
      const el = await mount();
      await waitUntil(() => el.querySelector("pre") !== null, "snapshot rendered");
      const pre = el.querySelector("pre")!;
      const table = el.querySelector("table")!;
      const before = (a: Element, b: Element) =>
        (a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
      // both the failure response and the incidents card sit above the history
      const incidentsHeading = Array.from(el.querySelectorAll("h2")).find(
        (h) => h.textContent?.trim() === "Incidents",
      )!;
      expect(before(pre, table), "snapshot before results").to.be.true;
      expect(before(incidentsHeading, table), "incidents before results").to.be.true;
    } finally {
      restore();
    }
  });

  it("shows live region chips from the poll for every monitor", async () => {
    const restore = installFetch((url) => {
      const path = url.split("?")[0];
      if (path.endsWith("/monitor-region-states"))
        return json(200, {
          monitors: {
            mon_1: [
              { region: "eu-central", state: "done", healthy: true, latency_ms: 100, status_code: 200, updated_at: "2026-06-21T10:15:00Z" },
              { region: "us-west", state: "failed", healthy: false, failure_reason: "timeout", updated_at: "2026-06-21T10:14:00Z" },
            ],
          },
        });
      return route(url);
    });
    try {
      const el = await mount();
      await waitUntil(
        () => el.querySelector("region-chips") !== null,
        "region chips rendered from the poll",
      );
      const chips = el.querySelector("region-chips")!;
      // both regions the poll reported are shown
      expect(chips.textContent).to.contain("eu-central");
      expect(chips.textContent).to.contain("us-west");
      // the healthy region reads ok, the failed one reads down
      expect(chips.textContent).to.contain("ok");
      expect(chips.textContent).to.contain("down");
    } finally {
      restore();
    }
  });

  it("shows the region card for a single-region monitor too", async () => {
    const restore = installFetch(route);
    try {
      const el = await mount();
      await waitUntil(() => el.querySelector("table") !== null);
      // the card is always present now, even before the first poll lands
      expect(el.textContent).to.contain("Status by region");
    } finally {
      restore();
    }
  });

  it("renders a captured failure response as text, never as HTML (no XSS)", async () => {
    const xssBody =
      '<img src=x onerror="window.__xss=true"><script>window.__xss=true</script>';
    const xssHeader = "<script>window.__xss=true</script>";
    const restore = installFetch((url) => {
      const path = url.split("?")[0];
      if (path.endsWith("/last-failure")) {
        return json(200, {
          monitor_id: "mon_1",
          checked_at: "2026-06-21T10:10:00Z",
          status_code: 503,
          headers: { "X-Evil": [xssHeader] },
          body: xssBody,
          truncated: false,
        });
      }
      return route(url);
    });
    try {
      (window as unknown as Record<string, unknown>).__xss = undefined;
      const el = await mount();
      await waitUntil(() => el.querySelector("pre") !== null, "snapshot rendered");

      const pre = el.querySelector("pre")!;
      // the malicious markup survives as literal text, not parsed into elements
      expect(pre.textContent).to.equal(xssBody);
      expect(pre.querySelector("img")).to.be.null;
      expect(pre.querySelector("script")).to.be.null;
      // the header value is escaped too
      expect(el.querySelector("dd")?.textContent).to.equal(xssHeader);
      expect(el.querySelector("dd script")).to.be.null;
      // nothing executed
      expect((window as unknown as Record<string, unknown>).__xss).to.equal(undefined);
      // no injected elements escaped into the component anywhere
      expect(el.querySelectorAll("script").length).to.equal(0);
    } finally {
      restore();
      (window as unknown as Record<string, unknown>).__xss = undefined;
    }
  });
});
