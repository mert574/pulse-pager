import { expect, waitUntil } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./channels-list-view.js";
import type { ChannelsListView } from "./channels-list-view.js";
import { appContext, type AppContext } from "../state/context.js";
import type { Channel, OrgMembership, Role } from "../api/types.js";

const ORG: OrgMembership = {
  org_id: "o1",
  name: "Org One",
  slug: "org-one",
  role: "owner",
  plan: "tier3",
};

const CHANNELS: Channel[] = [
  {
    id: "ch_1",
    org_id: "o1",
    name: "Ops Slack",
    type: "slack",
    enabled: true,
    config: {},
  },
  {
    id: "ch_2",
    org_id: "o1",
    name: "On-call webhook",
    type: "webhook",
    enabled: false,
    config: {},
  },
];

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
}): Promise<{ el: ChannelsListView; calls: Call[]; restore: () => void }> {
  const { calls, restore } = installFetch(
    opts.handler ?? (() => json(200, CHANNELS)),
  );
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
  host.innerHTML = "<channels-list-view></channels-list-view>";
  const el = host.querySelector<ChannelsListView>("channels-list-view")!;
  await el.updateComplete;
  return { el, calls, restore };
}

describe("channels-list-view", () => {
  it("renders a row per channel with type and enabled flag", async () => {
    const { el, restore } = await mount({});
    try {
      await waitUntil(() => el.querySelector("table") !== null, "table renders");
      expect(el.querySelectorAll("tbody tr").length).to.equal(2);
      expect(el.textContent).to.contain("Ops Slack");
      expect(el.textContent).to.contain("slack");
    } finally {
      restore();
    }
  });

  it("shows the empty state when there are no channels", async () => {
    const { el, restore } = await mount({ handler: () => json(200, []) });
    try {
      await waitUntil(
        () => el.textContent?.includes("No channels yet") ?? false,
        "empty state",
      );
      expect(el.querySelector("table")).to.be.null;
    } finally {
      restore();
    }
  });

  it("shows an error with retry when the request fails", async () => {
    const { el, restore } = await mount({
      handler: () =>
        json(500, { error: { code: "internal_error", message: "boom" } }),
    });
    try {
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

  it("shows New channel and row actions for an owner", async () => {
    const { el, restore } = await mount({ role: "owner" });
    try {
      await waitUntil(() => el.querySelector("table") !== null);
      expect(el.querySelector("a.btn-primary")).to.not.be.null;
      // edit + delete + send test all present
      expect(el.textContent).to.contain("Send test");
    } finally {
      restore();
    }
  });

  it("hides mutating actions for a viewer (read-only)", async () => {
    const { el, restore } = await mount({ role: "viewer" });
    try {
      await waitUntil(() => el.querySelector("table") !== null);
      expect(el.querySelector(".btn-primary")).to.be.null;
      expect(el.textContent).to.not.contain("Send test");
      // the actions column is dropped entirely for a viewer
      expect(el.querySelectorAll("thead th").length).to.equal(3);
    } finally {
      restore();
    }
  });

  it("sends a test and shows a success toast", async () => {
    const { el, calls, restore } = await mount({
      role: "owner",
      handler: (c) => {
        if (c.url.endsWith("/test")) return new Response(null, { status: 204 });
        return json(200, CHANNELS);
      },
    });
    try {
      await waitUntil(() => el.querySelector("table") !== null);
      const testBtn = Array.from(el.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("Send test"),
      )!;
      testBtn.click();
      await waitUntil(
        () => calls.some((c) => c.method === "POST" && c.url.endsWith("/test")),
        "test POST fired",
      );
      const post = calls.find((c) => c.url.endsWith("/test"))!;
      expect(post.url).to.contain("/orgs/o1/channels/ch_1/test");
    } finally {
      restore();
    }
  });

  it("deletes a channel after the confirm dialog and removes its row", async () => {
    const { el, calls, restore } = await mount({
      role: "owner",
      handler: (c) => {
        if (c.method === "DELETE") return new Response(null, { status: 204 });
        return json(200, CHANNELS);
      },
    });
    try {
      await waitUntil(() => el.querySelector("table") !== null);
      const deleteBtn = el.querySelector<HTMLButtonElement>(
        'button[aria-label="Delete"]',
      )!;
      deleteBtn.click();
      await waitUntil(
        () => el.querySelector(".modal-open") !== null,
        "confirm dialog opens",
      );
      const confirm = el.querySelector<HTMLButtonElement>(".modal-box .btn-error")!;
      confirm.click();
      await waitUntil(
        () => calls.some((c) => c.method === "DELETE"),
        "DELETE fired",
      );
      const del = calls.find((c) => c.method === "DELETE")!;
      expect(del.url).to.contain("/orgs/o1/channels/ch_1");
      await waitUntil(
        () => el.querySelectorAll("tbody tr").length === 1,
        "row removed",
      );
    } finally {
      restore();
    }
  });
});
