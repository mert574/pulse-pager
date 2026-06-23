import { expect, waitUntil } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./api-keys-view.js";
import type { ApiKeysView } from "./api-keys-view.js";
import { appContext, type AppContext } from "../state/context.js";
import type { ApiKey, OrgMembership, Role } from "../api/types.js";

const ORG: OrgMembership = {
  org_id: "o1",
  name: "Org One",
  slug: "org-one",
  role: "owner",
  plan: "tier3",
};

const KEYS: ApiKey[] = [
  {
    id: "ak_1",
    name: "CI pipeline",
    prefix: "pulse_sk_abc123",
    role: "member",
    created_by: "u1",
    created_at: "2026-01-01T10:00:00Z",
    last_used_at: "2026-02-01T12:00:00Z",
  },
  {
    id: "ak_2",
    name: "Backup script",
    prefix: "pulse_sk_def456",
    role: "admin",
    created_by: "u1",
    created_at: "2026-01-05T10:00:00Z",
    last_used_at: null,
  },
];

const FULL_SECRET = "pulse_sk_abc123_THE_ONE_TIME_FULL_SECRET_VALUE";

interface Call {
  url: string;
  method: string;
  body: unknown;
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
      body: init?.body ? JSON.parse(init.body as string) : undefined,
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
}): Promise<{ el: ApiKeysView; calls: Call[]; restore: () => void }> {
  const { calls, restore } = installFetch(
    opts.handler ?? (() => json(200, KEYS)),
  );
  const host = document.createElement("div");
  const ctx: AppContext = {
    me: { user_id: "u", email: "e", name: "n", avatar_url: null, orgs: [ORG] },
    activeOrg: { ...ORG, role: opts.role ?? "owner" },
    role: opts.role ?? "owner",
    // a paid plan with full API access so the management UI renders; the gating
    // (free = upgrade, hobby = read-only) is covered by the entitlement getters.
    entitlements: {
      plan: "tier3",
      monitors_used: 0,
      monitors_cap: 100,
      seats_used: 1,
      seats_cap: 10,
      status_pages_used: 0,
      status_pages_cap: 3,
      min_interval_seconds: 60,
      retention_days: 90,
      regions_allowed: ["eu-central"],
      regions_per_monitor_cap: 4,
      custom_domain_allowed: true,
      api_access_allowed: true,
      api_write_allowed: true,
      failure_snapshot: true,
    },
    refreshMe: async () => {},
  };
  new ContextProvider(host, { context: appContext, initialValue: ctx });
  document.body.appendChild(host);
  host.innerHTML = "<api-keys-view></api-keys-view>";
  const el = host.querySelector<ApiKeysView>("api-keys-view")!;
  await el.updateComplete;
  return { el, calls, restore };
}

describe("api-keys-view", () => {
  it("renders a row per key with name, prefix and role", async () => {
    const { el, restore } = await mount({});
    try {
      await waitUntil(() => el.querySelector("table") !== null, "table renders");
      expect(el.querySelectorAll("tbody tr").length).to.equal(2);
      expect(el.textContent).to.contain("CI pipeline");
      expect(el.textContent).to.contain("pulse_sk_abc123");
    } finally {
      restore();
    }
  });

  it("links to the API documentation in a new tab", async () => {
    const { el, restore } = await mount({});
    try {
      const link = el.querySelector<HTMLAnchorElement>('a[href="/api/docs"]')!;
      expect(link).to.not.be.null;
      expect(link.target).to.equal("_blank");
      expect(link.rel).to.contain("noopener");
    } finally {
      restore();
    }
  });

  it("offers member and admin in the create role select, never owner", async () => {
    const { el, restore } = await mount({ role: "owner" });
    try {
      const newBtn = Array.from(el.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("New API key"),
      )!;
      newBtn.click();
      await waitUntil(
        () => el.querySelector("#ak-role") !== null,
        "create dialog opens",
      );
      const select = el.querySelector<HTMLSelectElement>("#ak-role")!;
      const values = Array.from(select.options).map((o) => o.value);
      expect(values).to.deep.equal(["member", "admin"]);
      expect(values).to.not.contain("owner");
    } finally {
      restore();
    }
  });

  it("shows the one-time secret panel with a warning and copy control, and the secret is not in the list", async () => {
    const { el, calls, restore } = await mount({
      role: "owner",
      handler: (c) => {
        if (c.method === "POST") {
          return json(201, {
            key: {
              id: "ak_new",
              name: "New key",
              prefix: "pulse_sk_new789",
              role: "member",
              created_by: "u1",
              created_at: "2026-03-01T10:00:00Z",
              last_used_at: null,
            },
            secret: FULL_SECRET,
          });
        }
        return json(200, KEYS);
      },
    });
    try {
      await waitUntil(() => el.querySelector("table") !== null);
      const newBtn = Array.from(el.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("New API key"),
      )!;
      newBtn.click();
      await waitUntil(() => el.querySelector("#ak-name") !== null);
      const nameInput = el.querySelector<HTMLInputElement>("#ak-name")!;
      nameInput.value = "New key";
      nameInput.dispatchEvent(new Event("input"));
      const form = el.querySelector("form")!;
      form.dispatchEvent(new Event("submit"));

      // the one-time panel appears with the full secret + warning + copy
      await waitUntil(
        () => el.querySelector("[data-secret]") !== null,
        "secret panel shows",
      );
      const secretEl = el.querySelector("[data-secret]")!;
      expect(secretEl.textContent).to.contain(FULL_SECRET);
      expect(el.querySelector(".alert-warning")?.textContent).to.contain(
        "only time",
      );
      expect(el.querySelector('button[aria-label="Copy key"]')).to.not.be.null;

      // the POST fired with the right body
      const post = calls.find((c) => c.method === "POST")!;
      expect(post.url).to.contain("/orgs/o1/api-keys");
      expect(post.body).to.deep.equal({ name: "New key", role: "member" });

      // dismiss it: the secret is gone and never appears in the table
      const done = Array.from(el.querySelectorAll(".modal-box button")).find(
        (b) => b.textContent?.includes("Done"),
      ) as HTMLButtonElement;
      done.click();
      await waitUntil(
        () => el.querySelector("[data-secret]") === null,
        "secret dismissed",
      );
      expect(el.querySelector("table")?.textContent).to.not.contain(FULL_SECRET);
      // the new key still shows by its prefix, not its secret
      expect(el.textContent).to.contain("pulse_sk_new789");
    } finally {
      restore();
    }
  });

  it("revokes a key after the confirm dialog and removes its row", async () => {
    const { el, calls, restore } = await mount({
      role: "owner",
      handler: (c) => {
        if (c.method === "DELETE") return new Response(null, { status: 204 });
        return json(200, KEYS);
      },
    });
    try {
      await waitUntil(() => el.querySelector("table") !== null);
      const revokeBtn = el.querySelector<HTMLButtonElement>(
        'button[aria-label="Revoke"]',
      )!;
      revokeBtn.click();
      await waitUntil(
        () => el.querySelector(".modal-open") !== null,
        "confirm dialog opens",
      );
      const confirm = el.querySelector<HTMLButtonElement>(
        ".modal-box .btn-error",
      )!;
      confirm.click();
      await waitUntil(
        () => calls.some((c) => c.method === "DELETE"),
        "DELETE fired",
      );
      const del = calls.find((c) => c.method === "DELETE")!;
      expect(del.url).to.contain("/orgs/o1/api-keys/ak_1");
      await waitUntil(
        () => el.querySelectorAll("tbody tr").length === 1,
        "row removed",
      );
    } finally {
      restore();
    }
  });

  it("shows the no-access message for a member", async () => {
    const { el, calls, restore } = await mount({ role: "member" });
    try {
      await el.updateComplete;
      expect(el.textContent).to.contain("managed by owners and admins");
      expect(el.querySelector("table")).to.be.null;
      // no list call is made for a member
      expect(calls.length).to.equal(0);
    } finally {
      restore();
    }
  });
});
