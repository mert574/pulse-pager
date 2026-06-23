import { expect, waitUntil } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./members-view.js";
import "./toast-host.js";
import type { MembersView } from "./members-view.js";
import { appContext, type AppContext } from "../state/context.js";
import { session } from "../state/session.js";
import type { Member, Invitation, OrgMembership, Role } from "../api/types.js";

// members-view: renders members + role controls, hides mutations for a viewer,
// posts an invite and surfaces a seat-limit error, and fires the right endpoints
// for change-role / remove / transfer / revoke / resend. The api client is driven
// by a mocked global fetch, exactly like channels-list-view.test.ts.

const ORG: OrgMembership = {
  org_id: "o1",
  name: "Org One",
  slug: "org-one",
  role: "owner",
  plan: "tier3",
};

const ME = {
  user_id: "u_me",
  email: "me@example.com",
  name: "Me",
  avatar_url: null,
  locale: "en",
  timezone: "UTC",
  orgs: [ORG],
};

const MEMBERS: Member[] = [
  {
    user_id: "u_me",
    email: "me@example.com",
    name: "Me",
    avatar_url: null,
    role: "owner",
    joined_at: "2024-01-01T00:00:00Z",
  },
  {
    user_id: "u_2",
    email: "bob@example.com",
    name: "Bob",
    avatar_url: null,
    role: "member",
    joined_at: "2024-02-01T00:00:00Z",
  },
];

const INVITES: Invitation[] = [
  {
    id: "inv_1",
    email: "carol@example.com",
    role: "viewer",
    state: "pending",
    created_at: "2024-03-01T00:00:00Z",
    expires_at: "2024-03-08T00:00:00Z",
    invited_by: "u_me",
  },
];

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

// default handler: members + invitations list, 204 for any mutation
function defaultHandler(c: Call): Response {
  if (c.url.endsWith("/members")) return json(200, MEMBERS);
  if (c.url.endsWith("/invitations")) {
    if (c.method === "POST") {
      return json(201, {
        id: "inv_new",
        email: (c.body as { email: string }).email,
        role: (c.body as { role: Role }).role,
        state: "pending",
        created_at: "2024-04-01T00:00:00Z",
        expires_at: "2024-04-08T00:00:00Z",
        invited_by: "u_me",
      });
    }
    return json(200, INVITES);
  }
  if (c.url.endsWith("/resend")) return json(200, { ...INVITES[0] });
  if (c.method === "DELETE") return new Response(null, { status: 204 });
  if (c.method === "POST") return new Response(null, { status: 204 });
  if (c.method === "PATCH") {
    return json(200, { ...MEMBERS[1], role: (c.body as { role: Role }).role });
  }
  return json(200, MEMBERS);
}

async function mount(opts: {
  role?: Role;
  handler?: (c: Call) => Response;
}): Promise<{ el: MembersView; calls: Call[]; restore: () => void }> {
  session.setMe(ME);
  const { calls, restore } = installFetch(opts.handler ?? defaultHandler);
  const host = document.createElement("div");
  const ctx: AppContext = {
    me: ME,
    activeOrg: { ...ORG, role: opts.role ?? "owner" },
    role: opts.role ?? "owner",
    entitlements: null,
    refreshMe: async () => {},
  };
  new ContextProvider(host, { context: appContext, initialValue: ctx });
  document.body.appendChild(host);
  host.innerHTML = "<members-view></members-view>";
  const el = host.querySelector<MembersView>("members-view")!;
  await el.updateComplete;
  return {
    el,
    calls,
    restore: () => {
      restore();
      host.remove();
      session.clear();
    },
  };
}

describe("members-view", () => {
  it("renders a row per member with email and role", async () => {
    const { el, restore } = await mount({});
    try {
      await waitUntil(() => el.querySelector("table") !== null, "table renders");
      expect(el.textContent).to.contain("Bob");
      expect(el.textContent).to.contain("bob@example.com");
      // the owner row shows a read-only role badge; the member row a select
      expect(el.querySelector("select")).to.not.be.null;
    } finally {
      restore();
    }
  });

  it("hides role controls and actions for a viewer (read-only)", async () => {
    const { el, restore } = await mount({ role: "viewer" });
    try {
      await waitUntil(() => el.querySelector("table") !== null);
      // no role-change select, no remove buttons, no invite form
      expect(el.querySelector("select")).to.be.null;
      expect(el.querySelector('button[aria-label="Remove"]')).to.be.null;
      expect(el.textContent).to.not.contain("Pending invitations");
    } finally {
      restore();
    }
  });

  it("changes a member role via PATCH with the new role", async () => {
    const { el, calls, restore } = await mount({ role: "owner" });
    try {
      await waitUntil(() => el.querySelector("table") !== null);
      const select = el.querySelector<HTMLSelectElement>("select")!;
      select.value = "admin";
      select.dispatchEvent(new Event("change"));
      await waitUntil(
        () => calls.some((c) => c.method === "PATCH"),
        "PATCH fired",
      );
      const patch = calls.find((c) => c.method === "PATCH")!;
      expect(patch.url).to.contain("/orgs/o1/members/u_2");
      expect(patch.body).to.deep.equal({ role: "admin" });
    } finally {
      restore();
    }
  });

  it("removes a member after confirming and hits the member endpoint", async () => {
    const { el, calls, restore } = await mount({ role: "owner" });
    try {
      await waitUntil(() => el.querySelector("table") !== null);
      const removeBtn = el.querySelector<HTMLButtonElement>(
        'button[aria-label="Remove"]',
      )!;
      removeBtn.click();
      await waitUntil(
        () => el.querySelector(".modal-open") !== null,
        "confirm dialog opens",
      );
      el.querySelector<HTMLButtonElement>(".modal-box .btn-error")!.click();
      await waitUntil(
        () =>
          calls.some(
            (c) => c.method === "DELETE" && c.url.includes("/members/u_2"),
          ),
        "DELETE fired",
      );
    } finally {
      restore();
    }
  });

  it("transfers ownership after confirming via the transfer endpoint", async () => {
    const { el, calls, restore } = await mount({ role: "owner" });
    try {
      await waitUntil(() => el.querySelector("table") !== null);
      const transferBtn = Array.from(el.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("Make owner"),
      )!;
      transferBtn.click();
      await waitUntil(
        () => el.querySelector(".modal-open") !== null,
        "transfer dialog opens",
      );
      el.querySelector<HTMLButtonElement>(".modal-box .btn-primary")!.click();
      await waitUntil(
        () =>
          calls.some(
            (c) => c.method === "POST" && c.url.endsWith("/transfer-ownership"),
          ),
        "transfer POST fired",
      );
      const post = calls.find((c) => c.url.endsWith("/transfer-ownership"))!;
      expect(post.body).to.deep.equal({ user_id: "u_2", step_down: true });
    } finally {
      restore();
    }
  });

  it("posts an invitation with the chosen email and role", async () => {
    const { el, calls, restore } = await mount({ role: "owner" });
    try {
      await waitUntil(() => el.querySelector("#invite-email") !== null);
      const input = el.querySelector<HTMLInputElement>("#invite-email")!;
      input.value = "new@example.com";
      input.dispatchEvent(new Event("input"));
      await el.updateComplete;
      const form = el.querySelector("form")!;
      form.dispatchEvent(new Event("submit", { cancelable: true }));
      await waitUntil(
        () =>
          calls.some(
            (c) => c.method === "POST" && c.url.endsWith("/invitations"),
          ),
        "invite POST fired",
      );
      const post = calls.find(
        (c) => c.method === "POST" && c.url.endsWith("/invitations"),
      )!;
      expect(post.body).to.deep.equal({
        email: "new@example.com",
        role: "member",
      });
    } finally {
      restore();
    }
  });

  it("surfaces a seat-limit error from the invite endpoint", async () => {
    const handler = (c: Call): Response => {
      if (c.method === "POST" && c.url.endsWith("/invitations")) {
        return json(402, {
          error: {
            code: "seat_limit_reached",
            message: "No seats left on your plan",
          },
        });
      }
      return defaultHandler(c);
    };
    const toastHost = document.createElement("toast-host");
    document.body.appendChild(toastHost);
    const { el, restore } = await mount({ role: "owner", handler });
    try {
      await waitUntil(() => el.querySelector("#invite-email") !== null);
      const input = el.querySelector<HTMLInputElement>("#invite-email")!;
      input.value = "new@example.com";
      input.dispatchEvent(new Event("input"));
      await el.updateComplete;
      el.querySelector("form")!.dispatchEvent(
        new Event("submit", { cancelable: true }),
      );
      // the seat-limit message lands on the toast host (document-level)
      await waitUntil(
        () =>
          toastHost.textContent?.includes("No seats left on your plan") ?? false,
        "seat-limit toast shown",
      );
    } finally {
      restore();
      toastHost.remove();
    }
  });

  it("revokes and resends a pending invitation", async () => {
    const { el, calls, restore } = await mount({ role: "owner" });
    try {
      await waitUntil(
        () => el.textContent?.includes("carol@example.com") ?? false,
        "invitation row renders",
      );
      const resend = Array.from(el.querySelectorAll("button")).find((b) =>
        b.textContent?.trim().startsWith("Resend"),
      )!;
      resend.click();
      await waitUntil(
        () => calls.some((c) => c.url.endsWith("/invitations/inv_1/resend")),
        "resend POST fired",
      );
      // wait for the resend to settle so the revoke button re-enables
      const revokeBtn = () =>
        Array.from(el.querySelectorAll<HTMLButtonElement>("button")).find((b) =>
          b.textContent?.trim().startsWith("Revoke"),
        );
      await waitUntil(
        () => revokeBtn() !== undefined && !revokeBtn()!.disabled,
        "revoke button re-enabled",
      );
      revokeBtn()!.click();
      await waitUntil(
        () =>
          calls.some(
            (c) =>
              c.method === "DELETE" && c.url.endsWith("/invitations/inv_1"),
          ),
        "revoke DELETE fired",
      );
    } finally {
      restore();
    }
  });
});
