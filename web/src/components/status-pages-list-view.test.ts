import { expect, waitUntil } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./status-pages-list-view.js";
import type { StatusPagesListView } from "./status-pages-list-view.js";
import { appContext, type AppContext } from "../state/context.js";
import type { OrgMembership, Role, StatusPage } from "../api/types.js";

const ORG: OrgMembership = {
  org_id: "o1",
  name: "Org One",
  slug: "org-one",
  role: "owner",
  plan: "tier3",
};

function page(id: string, name: string, published: boolean): StatusPage {
  return {
    id,
    org_id: "o1",
    name,
    slug: name.toLowerCase().replace(/\s+/g, "-"),
    logo_url: "",
    accent_color: "#2563eb",
    theme: "light",
    state: published ? "published" : "draft",
    custom_domain: null,
    display_monitors: [],
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
}

const PAGES: StatusPage[] = [page("sp_1", "Public", true), page("sp_2", "Draft One", false)];

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

async function mount(opts: {
  role?: Role;
  handler?: (c: Call) => Response;
}): Promise<{ el: StatusPagesListView; calls: Call[]; restore: () => void }> {
  const { calls, restore } = installFetch(opts.handler ?? (() => json(200, PAGES)));
  const host = document.createElement("div");
  const ctx: AppContext = {
    me: { user_id: "u", email: "e", name: "n", avatar_url: null, orgs: [ORG] },
    activeOrg: { ...ORG, role: opts.role ?? "owner" },
    role: opts.role ?? "owner",
    entitlements: null,
    refreshMe: async () => {},
  };
  new ContextProvider(host, { context: appContext, initialValue: ctx });
  document.body.appendChild(host);
  host.innerHTML = "<status-pages-list-view></status-pages-list-view>";
  const el = host.querySelector<StatusPagesListView>("status-pages-list-view")!;
  await el.updateComplete;
  return { el, calls, restore };
}

describe("status-pages-list-view", () => {
  it("renders a row per status page with state and public link", async () => {
    const { el, restore } = await mount({});
    try {
      await waitUntil(
        () => el.querySelector("[data-status-page-card]") !== null,
        "ledger renders",
      );
      expect(el.querySelectorAll("[data-status-page-card]").length).to.equal(2);
      expect(el.textContent).to.contain("Public");
      expect(el.textContent).to.contain("Published");
      expect(el.textContent).to.contain("Draft");
      // published page links out, draft page does not
      expect(el.querySelector('a[target="_blank"]')).to.not.be.null;
    } finally {
      restore();
    }
  });

  it("shows the empty state when there are no pages", async () => {
    const { el, restore } = await mount({ handler: () => json(200, []) });
    try {
      await waitUntil(
        () => el.textContent?.includes("No status pages yet") ?? false,
        "empty state",
      );
      expect(el.querySelector("[data-status-page-card]")).to.be.null;
    } finally {
      restore();
    }
  });

  it("toggles publish via the API and updates the row", async () => {
    const { el, calls, restore } = await mount({
      role: "owner",
      handler: (c) => {
        if (c.url.endsWith("/publish")) return json(200, page("sp_2", "Draft One", true));
        return json(200, PAGES);
      },
    });
    try {
      await waitUntil(() => el.querySelector("[data-status-page-card]") !== null);
      // exact match picks the draft row's "Publish" (not the published row's "Unpublish")
      const publishBtn = Array.from(el.querySelectorAll("button")).find(
        (b) => b.textContent?.trim() === "Publish",
      )!;
      publishBtn.click();
      await waitUntil(
        () => calls.some((c) => c.method === "PUT" && c.url.endsWith("/publish")),
        "publish PUT fired",
      );
      const put = calls.find((c) => c.url.endsWith("/publish"))!;
      expect(put.url).to.contain("/orgs/o1/status-pages/sp_2/publish");
    } finally {
      restore();
    }
  });

  it("deletes a page after the confirm dialog and removes its row", async () => {
    const { el, calls, restore } = await mount({
      role: "owner",
      handler: (c) => {
        if (c.method === "DELETE") return new Response(null, { status: 204 });
        return json(200, PAGES);
      },
    });
    try {
      await waitUntil(() => el.querySelector("[data-status-page-card]") !== null);
      const deleteBtn = el.querySelector<HTMLButtonElement>(
        'button[aria-label="Delete"]',
      )!;
      deleteBtn.click();
      await waitUntil(
        () => el.querySelector(".pulse-dialog") !== null,
        "confirm dialog opens",
      );
      const confirm = el.querySelector<HTMLButtonElement>("[data-confirm]")!;
      confirm.click();
      await waitUntil(() => calls.some((c) => c.method === "DELETE"), "DELETE fired");
      const del = calls.find((c) => c.method === "DELETE")!;
      expect(del.url).to.contain("/orgs/o1/status-pages/sp_1");
      await waitUntil(
        () => el.querySelectorAll("[data-status-page-card]").length === 1,
        "row removed",
      );
    } finally {
      restore();
    }
  });

  it("hides mutating actions for a viewer (read-only)", async () => {
    const { el, restore } = await mount({ role: "viewer" });
    try {
      await waitUntil(() => el.querySelector("[data-status-page-card]") !== null);
      // both rows still render for a viewer, just with no action controls
      expect(el.querySelectorAll("[data-status-page-card]").length).to.equal(2);
      expect(el.querySelector(".pulse-btn")).to.be.null;
      expect(el.querySelector('button[aria-label="Delete"]')).to.be.null;
      expect(el.querySelector(".pulse-iconbtn")).to.be.null;
    } finally {
      restore();
    }
  });
});
