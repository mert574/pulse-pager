import { expect, fixture, html, waitUntil } from "@open-wc/testing";
import "./account-view.js";
import type { AccountView } from "./account-view.js";
import { session } from "../state/session.js";
import type { Identity, Me } from "../api/types.js";

// account-view: profile fields seeded from /me, the linked-providers list, the
// unlink-last guard, and the logout action hitting the endpoint. We mock fetch and
// seed the session as the app-root bootstrap would.

const ME: Me = {
  user_id: "u1",
  email: "ada@example.com",
  name: "Ada Lovelace",
  avatar_url: null,
  locale: "en",
  timezone: "UTC",
  orgs: [],
};

function identity(provider: "google" | "github"): Identity {
  return {
    provider,
    provider_user_id: `${provider}-123`,
    created_at: "2026-01-01T00:00:00Z",
  };
}

interface Call {
  url: string;
  method: string;
}

function installFetch(
  handler: (call: Call) => Response,
): { calls: Call[]; restore: () => void } {
  const calls: Call[] = [];
  const original = globalThis.fetch;
  globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    const call: Call = { url: String(input), method: init?.method ?? "GET" };
    calls.push(call);
    return Promise.resolve(handler(call));
  }) as typeof fetch;
  return { calls, restore: () => (globalThis.fetch = original) };
}

function json(status: number, body: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("account-view", () => {
  beforeEach(() => session.setMe(ME));
  afterEach(() => session.clear());

  it("seeds the profile from /me and lists linked providers", async () => {
    const restore = installFetch((c) =>
      c.url.includes("/me/identities")
        ? json(200, [identity("google"), identity("github")])
        : json(200, ME),
    ).restore;
    try {
      const el = await fixture<AccountView>(
        html`<account-view></account-view>`,
      );
      await waitUntil(
        () => el.querySelector("ul") !== null,
        "identities loaded",
      );
      const name = el.querySelector<HTMLInputElement>("#name")!;
      expect(name.value).to.equal("Ada Lovelace");
      const email = el.querySelector<HTMLInputElement>("#email")!;
      expect(email.value).to.equal("ada@example.com");
      // both providers connected, so two Unlink buttons are enabled
      const unlinks = [...el.querySelectorAll("button")].filter((b) =>
        b.textContent?.includes("Unlink"),
      );
      expect(unlinks.length).to.equal(2);
      expect(unlinks.every((b) => !(b as HTMLButtonElement).disabled)).to.be
        .true;
    } finally {
      restore();
    }
  });

  it("guards unlinking the last identity (button disabled, no request)", async () => {
    const f = installFetch((c) =>
      c.url.includes("/me/identities")
        ? json(200, [identity("google")])
        : json(200, ME),
    );
    try {
      const el = await fixture<AccountView>(
        html`<account-view></account-view>`,
      );
      await waitUntil(() => el.querySelector("ul") !== null);
      const unlink = [...el.querySelectorAll("button")].find((b) =>
        b.textContent?.includes("Unlink"),
      ) as HTMLButtonElement;
      expect(unlink).to.not.be.undefined;
      expect(unlink.disabled).to.be.true;
      const before = f.calls.length;
      unlink.click();
      await el.updateComplete;
      // no DELETE fired
      expect(f.calls.filter((c) => c.method === "DELETE").length).to.equal(0);
      expect(f.calls.length).to.equal(before);
    } finally {
      f.restore();
    }
  });

  it("logout calls the logout endpoint and clears the session", async () => {
    const f = installFetch((c) =>
      c.url.includes("/me/identities")
        ? json(200, [identity("google"), identity("github")])
        : c.url.includes("/auth/logout")
          ? json(204, undefined)
          : json(200, ME),
    );
    try {
      const el = await fixture<AccountView>(
        html`<account-view></account-view>`,
      );
      await waitUntil(() => el.querySelector("ul") !== null);
      const logout = [...el.querySelectorAll("button")].find(
        (b) => b.textContent?.trim() === "Log out",
      ) as HTMLButtonElement;
      logout.click();
      await waitUntil(
        () => f.calls.some((c) => c.url.includes("/auth/logout")),
        "logout endpoint hit",
      );
      await waitUntil(() => !session.isLoggedIn, "session cleared");
      expect(session.isLoggedIn).to.be.false;
    } finally {
      f.restore();
    }
  });
});
