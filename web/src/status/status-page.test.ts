import { expect, waitUntil } from "@open-wc/testing";
import "./status-page.js";
import type { StatusPage } from "./status-page.js";
import type { PublicStatusPage } from "../api/types.js";

const FIXTURE: PublicStatusPage = {
  name: "Acme Status",
  slug: "acme",
  logo_url: "https://cdn.example.com/logo.png",
  accent_color: "#ff0066",
  theme: "light",
  banner: "partial_outage",
  monitors: [
    {
      display_name: "Public API",
      status: "operational",
      uptime: {
        uptime_24h: 100,
        uptime_7d: 99.5,
        uptime_90d: 98.2,
        has_24h: true,
        has_7d: true,
        has_90d: true,
      },
      history: [
        { at: "2026-06-20T00:00:00Z", up: true },
        { at: "2026-06-21T00:00:00Z", up: false },
      ],
    },
    {
      display_name: "Dashboard",
      status: "down",
      uptime: {
        uptime_24h: 0,
        uptime_7d: 0,
        uptime_90d: 0,
        has_24h: false,
        has_7d: false,
        has_90d: false,
      },
      history: [],
    },
  ],
  incidents: [
    {
      display_name: "Dashboard",
      started_at: "2026-06-21T10:00:00Z",
      ended_at: null,
      duration_seconds: null,
      resolved: false,
    },
  ],
  uptime_max_window: "90d",
};

interface FetchMock {
  urls: string[];
  restore: () => void;
}

function installFetch(handler: (url: string) => Response): FetchMock {
  const urls: string[] = [];
  const original = globalThis.fetch;
  globalThis.fetch = ((input: RequestInfo | URL) => {
    const url = String(input);
    urls.push(url);
    return Promise.resolve(handler(url));
  }) as typeof fetch;
  return { urls, restore: () => (globalThis.fetch = original) };
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

// Drive slug resolution through the path; resolveSlug falls back to the last path
// segment so it does not need a real subdomain in the test runner.
function withSlug(slug: string): () => void {
  const original = window.location.pathname + window.location.search;
  history.replaceState({}, "", `/status/${slug}`);
  return () => history.replaceState({}, "", original);
}

async function mount(): Promise<StatusPage> {
  const el = document.createElement("status-page") as StatusPage;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

describe("status-page (public)", () => {
  it("renders branding, banner, monitors and uptime from the fixture", async () => {
    const restoreSlug = withSlug("acme");
    const fetchMock = installFetch(() => json(200, FIXTURE));
    try {
      const el = await mount();
      await waitUntil(
        () => el.textContent?.includes("Acme Status") ?? false,
        "page renders",
      );
      // banner
      expect(el.textContent).to.contain("Partial outage");
      // both monitors by friendly name
      expect(el.textContent).to.contain("Public API");
      expect(el.textContent).to.contain("Dashboard");
      // uptime summary value and the no-data label for the second monitor
      expect(el.textContent).to.contain("100.00%");
      expect(el.textContent).to.contain("No data");
      // history strip reused from uptime-bar
      expect(el.querySelector("uptime-bar")).to.not.be.null;
      // incident
      expect(el.textContent).to.contain("Ongoing");
      // it asked the public endpoint, not an org-scoped one
      expect(fetchMock.urls.some((u) => u.includes("/api/v1/public/status-pages/acme")))
        .to.be.true;
    } finally {
      fetchMock.restore();
      restoreSlug();
    }
  });

  it("never leaks internal monitor fields", async () => {
    const restoreSlug = withSlug("acme");
    const fetchMock = installFetch(() => json(200, FIXTURE));
    try {
      const el = await mount();
      await waitUntil(() => el.textContent?.includes("Acme Status") ?? false);
      // the public projection carries no url/method/headers; assert none surface
      expect(el.textContent).to.not.contain("https://api.example.com");
      expect(el.textContent?.toLowerCase()).to.not.contain("method");
    } finally {
      fetchMock.restore();
      restoreSlug();
    }
  });

  it("shows a not-found state on a 404 (unknown or unpublished slug)", async () => {
    const restoreSlug = withSlug("missing");
    const fetchMock = installFetch(() =>
      json(404, { error: { code: "not_found", message: "no" } }),
    );
    try {
      const el = await mount();
      await waitUntil(
        () => el.textContent?.includes("Status page not found") ?? false,
        "not-found state",
      );
      expect(el.querySelector("uptime-bar")).to.be.null;
    } finally {
      fetchMock.restore();
      restoreSlug();
    }
  });
});
