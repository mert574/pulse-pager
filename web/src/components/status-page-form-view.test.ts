import { expect, waitUntil } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./status-page-form-view.js";
import type { StatusPageFormView } from "./status-page-form-view.js";
import { appContext, type AppContext } from "../state/context.js";
import type { MonitorListItem, OrgMembership } from "../api/types.js";

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
    name: "API",
    url: "https://api.example.com",
    enabled: true,
    status: "up",
    last_check_at: null,
    last_latency_ms: null,
    incident_open: false,
  },
  {
    id: "mon_2",
    name: "Web",
    url: "https://example.com",
    enabled: true,
    status: "up",
    last_check_at: null,
    last_latency_ms: null,
    incident_open: false,
  },
];

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

function defaultRoute(c: Call): Response {
  if (c.url.endsWith("/monitors")) return json(200, MONITORS);
  if (c.url.endsWith("/status-pages") && c.method === "POST")
    return json(201, { id: "sp_new", org_id: "o1" });
  return json(200, {});
}

async function mount(
  handler: (c: Call) => Response = defaultRoute,
): Promise<{ el: StatusPageFormView; calls: Call[]; restore: () => void }> {
  const { calls, restore } = installFetch(handler);
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
  host.innerHTML = "<status-page-form-view></status-page-form-view>";
  const el = host.querySelector<StatusPageFormView>("status-page-form-view")!;
  await el.updateComplete;
  return { el, calls, restore };
}

describe("status-page-form-view", () => {
  it("renders the fields and a monitor picker from listMonitors", async () => {
    const { el, restore } = await mount();
    try {
      await waitUntil(
        () => el.textContent?.includes("API") ?? false,
        "monitors load into the picker",
      );
      expect(el.querySelector("#name")).to.not.be.null;
      expect(el.querySelector("#slug")).to.not.be.null;
      expect(el.querySelector("#logo_url")).to.not.be.null;
      expect(el.querySelector("#theme")).to.not.be.null;
      expect(el.querySelectorAll('input[type="checkbox"]').length).to.equal(2);
    } finally {
      restore();
    }
  });

  it("creates a page, posting the displayed monitors", async () => {
    const { el, calls, restore } = await mount();
    try {
      await waitUntil(() => el.textContent?.includes("API") ?? false);

      const name = el.querySelector<HTMLInputElement>("#name")!;
      name.value = "My Status";
      name.dispatchEvent(new Event("input"));
      const slug = el.querySelector<HTMLInputElement>("#slug")!;
      slug.value = "my-status";
      slug.dispatchEvent(new Event("input"));

      // select the first monitor
      const firstCheckbox = el.querySelector<HTMLInputElement>('input[type="checkbox"]')!;
      firstCheckbox.click();
      await el.updateComplete;

      el.querySelector("form")!.dispatchEvent(
        new Event("submit", { cancelable: true, bubbles: true }),
      );
      await waitUntil(
        () => calls.some((c) => c.method === "POST"),
        "create POST fired",
      );
      const post = calls.find((c) => c.method === "POST")!;
      expect(post.url).to.contain("/orgs/o1/status-pages");
      const sent = JSON.parse(post.body!);
      expect(sent.name).to.equal("My Status");
      expect(sent.slug).to.equal("my-status");
      expect(sent.display_monitors).to.have.length(1);
      expect(sent.display_monitors[0].monitor_id).to.equal("mon_1");
      expect(sent.display_monitors[0].order).to.equal(0);
    } finally {
      restore();
    }
  });

  it("surfaces a server per-field error under the field", async () => {
    const { el, restore } = await mount((c) => {
      if (c.url.endsWith("/monitors")) return json(200, MONITORS);
      if (c.method === "POST")
        return json(422, {
          error: {
            code: "validation_failed",
            message: "invalid",
            fields: { slug: "Slug already taken" },
          },
        });
      return json(200, {});
    });
    try {
      await waitUntil(() => el.textContent?.includes("API") ?? false);
      const name = el.querySelector<HTMLInputElement>("#name")!;
      name.value = "Taken";
      name.dispatchEvent(new Event("input"));
      const slug = el.querySelector<HTMLInputElement>("#slug")!;
      slug.value = "taken";
      slug.dispatchEvent(new Event("input"));

      el.querySelector("form")!.dispatchEvent(
        new Event("submit", { cancelable: true, bubbles: true }),
      );
      await waitUntil(
        () => el.textContent?.includes("Slug already taken") ?? false,
        "server field error renders",
      );
    } finally {
      restore();
    }
  });

  it("surfaces a 402 cap as an inline upsell", async () => {
    const { el, restore } = await mount((c) => {
      if (c.url.endsWith("/monitors")) return json(200, MONITORS);
      if (c.method === "POST")
        return json(402, {
          error: {
            code: "status_page_limit_reached",
            message: "You have reached your plan's status page limit.",
          },
        });
      return json(200, {});
    });
    try {
      await waitUntil(() => el.textContent?.includes("API") ?? false);
      const name = el.querySelector<HTMLInputElement>("#name")!;
      name.value = "Over";
      name.dispatchEvent(new Event("input"));
      const slug = el.querySelector<HTMLInputElement>("#slug")!;
      slug.value = "over";
      slug.dispatchEvent(new Event("input"));

      el.querySelector("form")!.dispatchEvent(
        new Event("submit", { cancelable: true, bubbles: true }),
      );
      await waitUntil(
        () => el.querySelector("upsell-banner") !== null,
        "upsell banner shows",
      );
      expect(el.querySelector("upsell-banner")?.textContent).to.contain("limit");
    } finally {
      restore();
    }
  });
});
