import { expect, waitUntil } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./monitor-form-view.js";
import type { MonitorFormView } from "./monitor-form-view.js";
import { appContext, type AppContext } from "../state/context.js";
import type { OrgMembership } from "../api/types.js";

const ORG: OrgMembership = {
  org_id: "o1",
  name: "Org One",
  slug: "org-one",
  role: "owner",
  plan: "team",
};

interface Call {
  url: string;
  method: string;
  body: string | null;
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
      body: (init?.body as string) ?? null,
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

function route(c: Call): Response {
  if (c.url.endsWith("/channels")) return json(200, []);
  if (c.url.endsWith("/monitors") && c.method === "POST")
    return json(201, { id: "mon_new", org_id: "o1" });
  return json(200, {});
}

async function mountForm(): Promise<MonitorFormView> {
  const host = document.createElement("div");
  const ctx: AppContext = {
    me: { user_id: "u", email: "e", name: "n", avatar_url: null, orgs: [ORG] },
    activeOrg: ORG,
    role: "owner",
    entitlements: null,
    refreshMe: async () => {},
  };
  new ContextProvider(host, { context: appContext, initialValue: ctx });
  document.body.appendChild(host);
  const el = document.createElement("monitor-form-view") as MonitorFormView;
  host.appendChild(el);
  await el.updateComplete;
  // wait for channels load to settle and the form to render
  await waitUntil(() => el.querySelector("#name") !== null, "form renders");
  return el;
}

function setInput(el: MonitorFormView, selector: string, value: string): void {
  const input = el.querySelector<HTMLInputElement>(selector)!;
  input.value = value;
  input.dispatchEvent(new Event("input", { bubbles: true }));
}

describe("monitor-form-view (create)", () => {
  it("renders the create heading and fields", async () => {
    const { restore } = installFetch(route);
    try {
      const el = await mountForm();
      expect(el.textContent).to.contain("New monitor");
      expect(el.querySelector("#url")).to.not.be.null;
      expect(el.querySelector("#method")).to.not.be.null;
    } finally {
      restore();
    }
  });

  it("blocks submit and shows field errors when name/url are empty", async () => {
    const { calls, restore } = installFetch(route);
    try {
      const el = await mountForm();
      el.querySelector("form")!.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      await el.updateComplete;
      expect(el.textContent).to.contain("Name is required");
      expect(el.textContent).to.contain("valid http(s) URL");
      expect(calls.some((c) => c.method === "POST")).to.be.false;
    } finally {
      restore();
    }
  });

  it("POSTs the monitor and navigates on a valid submit", async () => {
    const { calls, restore } = installFetch(route);
    try {
      const el = await mountForm();
      setInput(el, "#name", "Marketing site");
      setInput(el, "#url", "https://example.com");
      await el.updateComplete;
      el.querySelector("form")!.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      await waitUntil(
        () => calls.some((c) => c.method === "POST"),
        "POST fired",
      );
      const post = calls.find((c) => c.method === "POST")!;
      expect(post.url).to.contain("/orgs/o1/monitors");
      const sent = JSON.parse(post.body ?? "{}");
      expect(sent.name).to.equal("Marketing site");
      expect(sent.url).to.equal("https://example.com");
      // the v2 fields ride along. regions starts empty (no FE-guessed default);
      // the backend fills its default region when none is picked.
      expect(sent.regions).to.deep.equal([]);
      expect(sent.down_policy).to.equal("quorum");
      await waitUntil(
        () => location.pathname === "/orgs/o1/monitors/mon_new",
        "navigated to the new monitor",
      );
    } finally {
      restore();
    }
  });

  it("surfaces a 402 monitor-cap as an inline upsell", async () => {
    const { restore } = installFetch((c) => {
      if (c.url.endsWith("/channels")) return json(200, []);
      if (c.url.endsWith("/monitors") && c.method === "POST")
        return json(402, {
          error: {
            code: "monitor_limit_reached",
            message: "Monitor limit reached",
            params: { limit: 10 },
          },
        });
      return json(200, {});
    });
    try {
      const el = await mountForm();
      setInput(el, "#name", "Marketing site");
      setInput(el, "#url", "https://example.com");
      await el.updateComplete;
      el.querySelector("form")!.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      await waitUntil(
        () => el.querySelector("upsell-banner") !== null,
        "upsell shown",
      );
      const banner = el.querySelector("upsell-banner")!;
      // upgrade link points at billing, not a dead end
      expect(banner.querySelector("a")?.getAttribute("href")).to.contain(
        "/orgs/o1/billing",
      );
      // still on the form (no navigation away)
      expect(el.querySelector("form")).to.not.be.null;
    } finally {
      restore();
    }
  });

  it("shows a per-field error for a sub-floor interval", async () => {
    const { restore } = installFetch((c) => {
      if (c.url.endsWith("/channels")) return json(200, []);
      if (c.url.endsWith("/monitors") && c.method === "POST")
        return json(422, {
          error: {
            code: "validation_failed",
            message: "Validation failed",
            fields: { interval_seconds: "Interval is below your plan minimum" },
          },
        });
      return json(200, {});
    });
    try {
      const el = await mountForm();
      setInput(el, "#name", "Marketing site");
      setInput(el, "#url", "https://example.com");
      await el.updateComplete;
      el.querySelector("form")!.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      await waitUntil(
        () =>
          el.textContent?.includes("Interval is below your plan minimum") ??
          false,
        "interval error shown",
      );
      expect(el.querySelector("upsell-banner")).to.be.null;
    } finally {
      restore();
    }
  });
});
