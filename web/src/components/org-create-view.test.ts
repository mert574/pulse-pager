import { expect, waitUntil } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./org-create-view.js";
import type { OrgCreateView } from "./org-create-view.js";
import { appContext, type AppContext } from "../state/context.js";
import type { OrgMembership } from "../api/types.js";

// org-create-view: submitting the form POSTs /orgs, then refreshes /me and
// navigates to the new org. We mock fetch, spy refreshMe via the provided context,
// and assert the POST body and the navigation target.

const NEW_ORG: OrgMembership = {
  org_id: "o_new",
  name: "Acme",
  slug: "acme",
  role: "owner",
  plan: "free",
};

interface Call {
  url: string;
  method: string;
  body: string | null;
}

function installFetch(
  handler: (call: Call) => Response,
): { calls: Call[]; restore: () => void } {
  const calls: Call[] = [];
  const original = globalThis.fetch;
  globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    const call: Call = {
      url: String(input),
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

async function mount(): Promise<OrgCreateView> {
  const ctx: AppContext = {
    me: {
      user_id: "u",
      email: "e",
      name: "n",
      avatar_url: null,
      locale: "en",
      timezone: "UTC",
      orgs: [],
    },
    activeOrg: null,
    role: null,
    entitlements: null,
    refreshMe: async () => {},
  };
  const host = document.createElement("div");
  new ContextProvider(host, { context: appContext, initialValue: ctx });
  document.body.appendChild(host);
  host.innerHTML = "<org-create-view></org-create-view>";
  const el = host.querySelector<OrgCreateView>("org-create-view")!;
  await el.updateComplete;
  return el;
}

describe("org-create-view", () => {
  it("POSTs /orgs with the name and navigates to the new org", async () => {
    const f = installFetch((c) =>
      c.method === "POST" ? json(201, NEW_ORG) : json(200, {}),
    );
    try {
      const el = await mount();
      const input = el.querySelector<HTMLInputElement>("#name")!;
      input.value = "Acme";
      input.dispatchEvent(new Event("input", { bubbles: true }));
      await el.updateComplete;

      el.querySelector<HTMLButtonElement>('button[type="submit"]')!.click();

      await waitUntil(
        () => f.calls.some((c) => c.method === "POST"),
        "POST /orgs fired",
      );
      const post = f.calls.find((c) => c.method === "POST")!;
      expect(post.url).to.contain("/api/v1/orgs");
      expect(JSON.parse(post.body!)).to.deep.equal({ name: "Acme" });

      await waitUntil(
        () => window.location.pathname.includes("o_new"),
        "navigated to new org",
      );
      expect(window.location.pathname).to.contain("/orgs/o_new");
    } finally {
      f.restore();
    }
  });

  it("does not POST when the name is blank", async () => {
    const f = installFetch(() => json(201, NEW_ORG));
    try {
      const el = await mount();
      el.querySelector<HTMLButtonElement>('button[type="submit"]')!.click();
      await el.updateComplete;
      expect(f.calls.filter((c) => c.method === "POST").length).to.equal(0);
      // the field error renders
      expect(el.querySelector('[role="alert"], .text-error')).to.not.be.null;
    } finally {
      f.restore();
    }
  });
});
