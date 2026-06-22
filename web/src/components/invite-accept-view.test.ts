import { expect, waitUntil } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./invite-accept-view.js";
import type { InviteAcceptView } from "./invite-accept-view.js";
import { appContext, type AppContext } from "../state/context.js";
import { session } from "../state/session.js";
import type { InvitationPreview, OrgMembership } from "../api/types.js";

// invite-accept-view: renders the token preview ("Join {org} as {role}?"), shows a
// sign-in prompt when logged out, calls the accept endpoint when logged in, and
// renders a localized error for a mismatch (403) or an expired token (404/409).

const PREVIEW: InvitationPreview = {
  org_name: "Acme Inc",
  role: "admin",
  state: "pending",
  email: "invitee@example.com",
  inviter_name: "Alice",
};

const ORG: OrgMembership = {
  org_id: "o9",
  name: "Acme Inc",
  slug: "acme",
  role: "admin",
  plan: "team",
};

const ME = {
  user_id: "u_me",
  email: "invitee@example.com",
  name: "Invitee",
  avatar_url: null,
  locale: "en",
  timezone: "UTC",
  orgs: [] as OrgMembership[],
};

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
  loggedIn?: boolean;
  handler?: (c: Call) => Response;
}): Promise<{ el: InviteAcceptView; calls: Call[]; restore: () => void }> {
  if (opts.loggedIn) session.setMe(ME);
  else session.clear();
  const { calls, restore } = installFetch(
    opts.handler ?? (() => json(200, PREVIEW)),
  );
  const host = document.createElement("div");
  const ctx: AppContext = {
    me: opts.loggedIn ? ME : null,
    activeOrg: null,
    role: null,
    entitlements: null,
    refreshMe: async () => {},
  };
  new ContextProvider(host, { context: appContext, initialValue: ctx });
  document.body.appendChild(host);
  host.innerHTML =
    '<invite-accept-view token="tok_123"></invite-accept-view>';
  const el = host.querySelector<InviteAcceptView>("invite-accept-view")!;
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

describe("invite-accept-view", () => {
  it("renders the preview prompt with org and role", async () => {
    const { el, restore } = await mount({ loggedIn: true });
    try {
      await waitUntil(
        () => el.textContent?.includes("Acme Inc") ?? false,
        "preview renders",
      );
      expect(el.textContent).to.contain("Acme Inc");
      expect(el.textContent).to.contain("Admin");
      expect(el.textContent).to.contain("Alice");
    } finally {
      restore();
    }
  });

  it("shows a sign-in prompt when logged out", async () => {
    const { el, restore } = await mount({ loggedIn: false });
    try {
      await waitUntil(
        () => el.textContent?.includes("Acme Inc") ?? false,
        "preview renders",
      );
      const signIn = Array.from(el.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("Sign in"),
      );
      expect(signIn).to.not.be.undefined;
      // no accept button while logged out
      const accept = Array.from(el.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("Accept invitation"),
      );
      expect(accept).to.be.undefined;
    } finally {
      restore();
    }
  });

  it("accepts the invitation via the accept endpoint when logged in", async () => {
    const handler = (c: Call): Response => {
      if (c.url.endsWith("/accept")) return json(200, ORG);
      return json(200, PREVIEW);
    };
    const { el, calls, restore } = await mount({ loggedIn: true, handler });
    try {
      await waitUntil(
        () => el.textContent?.includes("Accept invitation") ?? false,
        "accept button renders",
      );
      const accept = Array.from(el.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("Accept invitation"),
      )!;
      accept.click();
      await waitUntil(
        () =>
          calls.some(
            (c) => c.method === "POST" && c.url.endsWith("/accept"),
          ),
        "accept POST fired",
      );
      const post = calls.find((c) => c.url.endsWith("/accept"))!;
      expect(post.url).to.contain("/invitations/tok_123/accept");
    } finally {
      restore();
    }
  });

  it("renders a mismatch error when accept returns 403", async () => {
    const handler = (c: Call): Response => {
      if (c.url.endsWith("/accept")) {
        return json(403, {
          error: { code: "email_mismatch", message: "wrong email" },
        });
      }
      return json(200, PREVIEW);
    };
    const { el, restore } = await mount({ loggedIn: true, handler });
    try {
      await waitUntil(
        () => el.textContent?.includes("Accept invitation") ?? false,
        "accept button renders",
      );
      Array.from(el.querySelectorAll("button"))
        .find((b) => b.textContent?.includes("Accept invitation"))!
        .click();
      await waitUntil(
        () => el.querySelector('[role="alert"]') !== null,
        "error renders",
      );
      expect(el.querySelector('[role="alert"]')?.textContent).to.contain(
        "different email",
      );
    } finally {
      restore();
    }
  });

  it("renders an expired error when the preview is 404/409", async () => {
    const { el, restore } = await mount({
      loggedIn: true,
      handler: () =>
        json(404, { error: { code: "not_found", message: "gone" } }),
    });
    try {
      await waitUntil(
        () => el.querySelector('[role="alert"]') !== null,
        "error renders",
      );
      expect(el.querySelector('[role="alert"]')?.textContent).to.contain(
        "expired",
      );
    } finally {
      restore();
    }
  });
});
