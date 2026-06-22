import { expect } from "@open-wc/testing";
import { __test, ApiError } from "./client.js";
import { session } from "../state/session.js";

// Auth interceptor tests (RFC-013 section 11, decision D3): single-flight
// refresh-then-retry-once, CSRF echo, no recursion on /auth/refresh.
//
// We swap global fetch for a recorder and drive request() directly via the
// __test seam, so no real network is hit.

interface Call {
  url: string;
  method: string;
  headers: Record<string, string>;
}

type Handler = (call: Call, n: number) => Response;

function installFetch(handler: Handler): { calls: Call[]; restore: () => void } {
  const calls: Call[] = [];
  const original = globalThis.fetch;
  let n = 0;
  globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const headers = (init?.headers as Record<string, string>) ?? {};
    const call: Call = { url, method: init?.method ?? "GET", headers };
    calls.push(call);
    return Promise.resolve(handler(call, n++));
  }) as typeof fetch;
  return { calls, restore: () => (globalThis.fetch = original) };
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function ok(body: unknown): Response {
  return json(200, body);
}

function unauth(): Response {
  return json(401, { error: { code: "unauthenticated", message: "expired" } });
}

const refreshUrl = (c: Call) => c.url.startsWith("/auth/refresh");

describe("api client auth interceptor", () => {
  it("on 401 it refreshes once, then replays the original request and returns it", async () => {
    let refreshes = 0;
    const { calls, restore } = installFetch((c, n) => {
      if (refreshUrl(c)) {
        refreshes++;
        return ok(null);
      }
      // first hit 401, after refresh the replay succeeds
      return n === 0 ? unauth() : ok({ id: "mon_1" });
    });
    try {
      const res = await __test.request<{ id: string }>("/api/v1/orgs/o/monitors/mon_1");
      expect(res.id).to.equal("mon_1");
      expect(refreshes).to.equal(1);
      // original, refresh, replay
      expect(calls.length).to.equal(3);
      expect(calls[2].url).to.contain("/monitors/mon_1");
    } finally {
      restore();
    }
  });

  it("ten concurrent 401s share a single refresh", async () => {
    let refreshes = 0;
    const seen = new Set<string>();
    const { restore } = installFetch((c) => {
      if (refreshUrl(c)) {
        refreshes++;
        return ok(null);
      }
      // 401 the first time we see each distinct url, succeed on replay
      if (!seen.has(c.url)) {
        seen.add(c.url);
        return unauth();
      }
      return ok({ ok: true });
    });
    try {
      await Promise.all(
        Array.from({ length: 10 }, (_, i) =>
          __test.request(`/api/v1/orgs/o/monitors/m${i}`),
        ),
      );
      expect(refreshes).to.equal(1);
    } finally {
      restore();
    }
  });

  it("a 401 that survives refresh clears the session and throws 401", async () => {
    session.setMe({
      user_id: "u",
      email: "e",
      name: "n",
      avatar_url: null,
      orgs: [],
    });
    const { restore } = installFetch((c) => (refreshUrl(c) ? ok(null) : unauth()));
    try {
      let thrown: unknown;
      try {
        await __test.request("/api/v1/me");
      } catch (e) {
        thrown = e;
      }
      expect(thrown).to.be.instanceOf(ApiError);
      expect((thrown as ApiError).status).to.equal(401);
      expect(session.isLoggedIn).to.be.false;
    } finally {
      restore();
    }
  });

  it("does not recurse: a failing refresh goes straight to login, no extra calls", async () => {
    const { calls, restore } = installFetch((c) =>
      refreshUrl(c) ? json(401, { error: { code: "x", message: "x" } }) : unauth(),
    );
    try {
      try {
        await __test.request("/api/v1/me");
      } catch {
        // expected
      }
      // original + one refresh attempt, then give up. No retry, no refresh loop.
      expect(calls.length).to.equal(2);
      expect(calls.filter(refreshUrl).length).to.equal(1);
    } finally {
      restore();
    }
  });

  it("echoes the CSRF cookie on unsafe methods only", async () => {
    document.cookie = "pulse_csrf=tok123";
    const { calls, restore } = installFetch(() => ok({}));
    try {
      await __test.request("/api/v1/orgs/o/monitors", {
        method: "POST",
        body: {},
      });
      await __test.request("/api/v1/orgs/o/monitors");
      const post = calls.find((c) => c.method === "POST")!;
      const get = calls.find((c) => c.method === "GET")!;
      expect(post.headers["X-CSRF-Token"]).to.equal("tok123");
      expect(get.headers["X-CSRF-Token"]).to.be.undefined;
    } finally {
      restore();
      document.cookie = "pulse_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT";
    }
  });
});
