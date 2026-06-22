import { expect, waitUntil } from "@open-wc/testing";
import { html } from "lit";
import "./app-root.js";
import type { AppRoot } from "./app-root.js";
import { Router, registerRouter } from "../router.js";
import { session } from "../state/session.js";
import type { Me } from "../api/types.js";

// Session bootstrap (app-root): on connect it calls GET /api/v1/me. On 200 it
// populates the session and renders the authed shell; on 401 it routes to /login
// and renders the login view. We mock fetch and a minimal router outlet.

const ME: Me = {
  user_id: "u1",
  email: "ada@example.com",
  name: "Ada",
  avatar_url: null,
  locale: "en",
  timezone: "UTC",
  orgs: [
    { org_id: "o1", name: "Org One", slug: "org-one", role: "owner", plan: "team" },
  ],
};

function installFetch(handler: (url: string) => Response): () => void {
  const original = globalThis.fetch;
  globalThis.fetch = ((input: RequestInfo | URL) =>
    Promise.resolve(handler(String(input)))) as typeof fetch;
  return () => (globalThis.fetch = original);
}

function json(status: number, body: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function mountRoot(): AppRoot {
  const router = new Router(
    [{ pattern: "/orgs/:orgId", render: () => html`<div id="org-home">home</div>` }],
    { pattern: "*", render: () => html`<div>fallback</div>` },
  );
  registerRouter(router);
  const el = document.createElement("app-root") as AppRoot;
  el.router = router;
  document.body.appendChild(el);
  el.startRouter();
  return el;
}

describe("app-root session bootstrap", () => {
  afterEach(() => session.clear());

  it("renders the app shell when /me returns 200", async () => {
    window.history.replaceState({}, "", "/");
    const restore = installFetch((url) =>
      url.includes("/api/v1/me")
        ? json(200, ME)
        : url.includes("/entitlements")
          ? json(200, {})
          : json(200, {}),
    );
    const el = mountRoot();
    try {
      await waitUntil(() => session.isLoggedIn, "session populated");
      await el.updateComplete;
      await waitUntil(
        () => el.querySelector("app-nav") !== null,
        "authed shell rendered",
      );
      expect(el.querySelector("login-view")).to.be.null;
    } finally {
      restore();
      el.remove();
    }
  });

  it("routes to /login and shows login-view when /me returns 401", async () => {
    window.history.replaceState({}, "", "/orgs/o1");
    const restore = installFetch((url) =>
      url.includes("/auth/refresh")
        ? json(401, { error: { code: "x", message: "x" } })
        : json(401, { error: { code: "unauthenticated", message: "no" } }),
    );
    const el = mountRoot();
    try {
      await waitUntil(() => session.checked, "bootstrap settled");
      await waitUntil(
        () => window.location.pathname.endsWith("/login"),
        "navigated to /login",
      );
      await el.updateComplete;
      await waitUntil(
        () => el.querySelector("login-view") !== null,
        "login view rendered",
      );
      expect(session.isLoggedIn).to.be.false;
      expect(window.location.pathname).to.contain("/login");
    } finally {
      restore();
      el.remove();
    }
  });
});
