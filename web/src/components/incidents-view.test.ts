import { expect, waitUntil } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./incidents-view.js";
import type { IncidentsView } from "./incidents-view.js";
import { appContext, type AppContext } from "../state/context.js";
import type {
  Incident,
  MonitorListItem,
  OrgMembership,
  Role,
} from "../api/types.js";

const ORG: OrgMembership = {
  org_id: "o1",
  name: "Org One",
  slug: "org-one",
  role: "owner",
  plan: "tier3",
};

const MONITORS: MonitorListItem[] = [
  {
    id: "mon_1",
    name: "Marketing site",
    url: "https://example.com",
    enabled: true,
    status: "down",
    last_check_at: "2026-06-21T10:00:00Z",
    last_latency_ms: 95,
    incident_open: true,
  },
  {
    id: "mon_2",
    name: "Prod API",
    url: "https://api.example.com",
    enabled: true,
    status: "up",
    last_check_at: "2026-06-21T10:01:00Z",
    last_latency_ms: 120,
    incident_open: false,
  },
];

const OPEN_INCIDENT: Incident = {
  id: "inc_1",
  monitor_id: "mon_1",
  started_at: "2026-06-21T09:00:00Z",
  ended_at: null,
  cause_reason: "status_mismatch",
  close_reason: null,
  duration_seconds: null,
};

const CLOSED_INCIDENT: Incident = {
  id: "inc_2",
  monitor_id: "mon_2",
  started_at: "2026-06-20T08:00:00Z",
  ended_at: "2026-06-20T08:30:00Z",
  cause_reason: "timeout",
  close_reason: "recovered",
  duration_seconds: 1800,
};

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

// the incidents query string the view sent, captured so a test can assert how the
// list is queried.
let lastIncidentsUrl = "";

function route(items: Incident[]): (url: string) => Response {
  return (url: string) => {
    const path = url.split("?")[0];
    if (path.endsWith("/monitors")) return json(200, MONITORS);
    if (path.endsWith("/incidents")) {
      lastIncidentsUrl = url;
      return json(200, { items, next_cursor: null });
    }
    return json(200, {});
  };
}

async function mount(role: Role = "owner"): Promise<IncidentsView> {
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
  const el = document.createElement("incidents-view") as IncidentsView;
  host.appendChild(el);
  await el.updateComplete;
  return el;
}

describe("incidents-view", () => {
  it("renders a row per incident with monitor name, cause and status", async () => {
    const restore = installFetch(route([OPEN_INCIDENT, CLOSED_INCIDENT]));
    try {
      const el = await mount();
      await waitUntil(() => el.querySelector("[data-incident-row]") !== null, "table renders");
      expect(el.querySelectorAll("[data-incident-row]").length).to.equal(2);
      // monitor names are resolved from the monitor list
      expect(el.textContent).to.contain("Marketing site");
      expect(el.textContent).to.contain("Prod API");
      // localized cause
      expect(el.textContent).to.contain("Status mismatch");
      // open vs closed status + ongoing duration
      expect(el.textContent).to.contain("Open");
      expect(el.textContent).to.contain("Closed");
      expect(el.textContent).to.contain("Ongoing");
    } finally {
      restore();
    }
  });

  it("loads all incidents with no status filter", async () => {
    const restore = installFetch(route([OPEN_INCIDENT, CLOSED_INCIDENT]));
    try {
      const el = await mount();
      await waitUntil(() => el.querySelector("[data-incident-row]") !== null);
      expect(lastIncidentsUrl).to.not.contain("status=");
    } finally {
      restore();
    }
  });

  it("links a row to the incident detail", async () => {
    const restore = installFetch(route([OPEN_INCIDENT]));
    try {
      const el = await mount();
      await waitUntil(() => el.querySelector("[data-incident-row]") !== null);
      const link = el.querySelector(
        'a[href="/orgs/o1/incidents/inc_1"]',
      ) as HTMLAnchorElement;
      expect(link).to.not.be.null;
      expect(link.textContent).to.contain("Marketing site");
    } finally {
      restore();
    }
  });

  it("shows the empty state when there are no incidents", async () => {
    const restore = installFetch(route([]));
    try {
      const el = await mount();
      await waitUntil(
        () => el.textContent?.includes("No incidents") ?? false,
        "empty state",
      );
      expect(el.querySelector("[data-incident-row]")).to.be.null;
      // with nothing open the slim status strip reads calm, not a hazard band
      expect(el.textContent).to.contain("All systems operational");
    } finally {
      restore();
    }
  });

  it("shows an error with retry when the request fails", async () => {
    const restore = installFetch((url) => {
      if (url.includes("/incidents"))
        return json(500, { error: { code: "internal_error", message: "boom" } });
      return json(200, MONITORS);
    });
    try {
      const el = await mount();
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
});
