import { expect, waitUntil } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./incident-detail-view.js";
import type { IncidentDetailView } from "./incident-detail-view.js";
import { appContext, type AppContext } from "../state/context.js";
import { session } from "../state/session.js";
import { subscribeToasts, type ToastItem } from "../toast.js";
import type {
  IncidentDetail,
  Monitor,
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

const OPEN_DETAIL: IncidentDetail = {
  id: "inc_1",
  monitor_id: "mon_1",
  started_at: "2026-06-21T09:00:00Z",
  ended_at: null,
  cause_reason: "status_mismatch",
  close_reason: null,
  duration_seconds: null,
  annotations: [
    {
      id: "an_1",
      incident_id: "inc_1",
      author_user_id: "u",
      note: "Investigating the upstream",
      created_at: "2026-06-21T09:05:00Z",
    },
  ],
};

const CLOSED_DETAIL: IncidentDetail = {
  ...OPEN_DETAIL,
  ended_at: "2026-06-21T09:30:00Z",
  close_reason: "manual",
  duration_seconds: 1800,
};

function installFetch(handler: (url: string, init?: RequestInit) => Response): () => void {
  const original = globalThis.fetch;
  globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) =>
    Promise.resolve(handler(String(input), init))) as typeof fetch;
  return () => (globalThis.fetch = original);
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

// default route: GET incident -> open detail, GET monitor -> name.
function baseRoute(detail: IncidentDetail) {
  return (url: string): Response => {
    const path = url.split("?")[0];
    if (path.endsWith("/monitors/mon_1")) return json(200, MONITOR);
    if (path.endsWith("/incidents/inc_1")) return json(200, detail);
    return json(200, {});
  };
}

async function mount(role: Role = "owner"): Promise<IncidentDetailView> {
  // the "You" tag on a note compares against the session singleton, so seed it
  session.setMe({ user_id: "u", email: "e", name: "n", avatar_url: null, orgs: [ORG] });
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
  const el = document.createElement("incident-detail-view") as IncidentDetailView;
  el.incidentId = "inc_1";
  host.appendChild(el);
  await el.updateComplete;
  return el;
}

describe("incident-detail-view", () => {
  it("renders the header and the annotation timeline", async () => {
    const restore = installFetch(baseRoute(OPEN_DETAIL));
    try {
      const el = await mount();
      await waitUntil(
        () => el.textContent?.includes("Marketing site") ?? false,
        "header rendered",
      );
      // cause + open status in the header
      expect(el.textContent).to.contain("Status mismatch");
      expect(el.textContent).to.contain("Open");
      // the timeline note + its author (current user shows "You")
      expect(el.textContent).to.contain("Investigating the upstream");
      expect(el.textContent).to.contain("You");
    } finally {
      restore();
    }
  });

  it("shows the close reason for a closed incident", async () => {
    const restore = installFetch(baseRoute(CLOSED_DETAIL));
    try {
      const el = await mount();
      await waitUntil(
        () => el.textContent?.includes("Marketing site") ?? false,
      );
      expect(el.textContent).to.contain("Closed");
      expect(el.textContent).to.contain("Closed manually");
    } finally {
      restore();
    }
  });

  it("posts a note and appends it to the timeline (member+)", async () => {
    const restore = installFetch((url, init) => {
      const path = url.split("?")[0];
      if (path.endsWith("/annotations") && init?.method === "POST") {
        return json(201, {
          id: "an_2",
          incident_id: "inc_1",
          author_user_id: "u",
          note: "Root cause found",
          created_at: "2026-06-21T09:10:00Z",
        });
      }
      return baseRoute(OPEN_DETAIL)(url);
    });
    try {
      const el = await mount("member");
      await waitUntil(() => el.querySelector("textarea") !== null, "form shown");
      const textarea = el.querySelector("textarea") as HTMLTextAreaElement;
      textarea.value = "Root cause found";
      textarea.dispatchEvent(new Event("input"));
      await el.updateComplete;
      const form = el.querySelector("form") as HTMLFormElement;
      form.dispatchEvent(new Event("submit"));
      await waitUntil(
        () => el.textContent?.includes("Root cause found") ?? false,
        "note appended",
      );
    } finally {
      restore();
    }
  });

  it("shows the close action for owner/admin and closes the incident", async () => {
    let closed = false;
    const restore = installFetch((url, init) => {
      const path = url.split("?")[0];
      if (path.endsWith("/incidents/inc_1/close") && init?.method === "POST") {
        closed = true;
        return json(200, CLOSED_DETAIL);
      }
      return baseRoute(OPEN_DETAIL)(url);
    });
    try {
      const el = await mount("admin");
      await waitUntil(
        () => el.textContent?.includes("Close incident") ?? false,
        "close button shown",
      );
      const closeBtn = Array.from(el.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("Close incident"),
      ) as HTMLButtonElement;
      closeBtn.click();
      await el.updateComplete;
      // confirm in the dialog
      const confirmBtn = Array.from(
        el.querySelectorAll(".pulse-dialog button"),
      ).find((b) => b.textContent?.trim() === "Close incident") as HTMLButtonElement;
      confirmBtn.click();
      await waitUntil(() => closed, "close endpoint called");
      await waitUntil(
        () => el.textContent?.includes("Closed manually") ?? false,
        "header updated to closed",
      );
    } finally {
      restore();
    }
  });

  it("hides the close action for a member", async () => {
    const restore = installFetch(baseRoute(OPEN_DETAIL));
    try {
      const el = await mount("member");
      await waitUntil(
        () => el.textContent?.includes("Marketing site") ?? false,
      );
      const closeBtn = Array.from(el.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("Close incident"),
      );
      expect(closeBtn).to.be.undefined;
    } finally {
      restore();
    }
  });

  it("surfaces an already-closed 409 from the close endpoint", async () => {
    const restore = installFetch((url, init) => {
      const path = url.split("?")[0];
      if (path.endsWith("/incidents/inc_1/close") && init?.method === "POST") {
        return json(409, {
          error: { code: "conflict", message: "already closed" },
        });
      }
      return baseRoute(OPEN_DETAIL)(url);
    });
    let toasts: ToastItem[] = [];
    const off = subscribeToasts((items) => (toasts = items));
    try {
      const el = await mount("owner");
      await waitUntil(
        () => el.textContent?.includes("Close incident") ?? false,
      );
      const closeBtn = Array.from(el.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("Close incident"),
      ) as HTMLButtonElement;
      closeBtn.click();
      await el.updateComplete;
      const confirmBtn = Array.from(
        el.querySelectorAll(".pulse-dialog button"),
      ).find((b) => b.textContent?.trim() === "Close incident") as HTMLButtonElement;
      confirmBtn.click();
      // the close fails with 409 and the view toasts the localized already-closed
      // message rather than the raw server message
      await waitUntil(
        () => toasts.some((tt) => tt.message === "This incident is already closed."),
        "already-closed toast shown",
      );
    } finally {
      off();
      restore();
    }
  });
});
