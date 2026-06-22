import { expect } from "@open-wc/testing";
import { html } from "lit";
import { Router, type RouteContext } from "./router.js";

// Router tests (RFC-013 section 11): param extraction, /orgs/:orgId scoping, and
// guard redirects.
describe("Router", () => {
  it("extracts params under /orgs/:orgId scoping", () => {
    let seen: RouteContext | null = null;
    const router = new Router([
      {
        pattern: "/orgs/:orgId/monitors/:id",
        render: (ctx) => {
          seen = ctx;
          return html``;
        },
      },
    ]);
    history.pushState({}, "", "/orgs/org_abc/monitors/mon_42?range=7d");
    router.outlet();
    expect(seen).to.not.be.null;
    expect(seen!.params.orgId).to.equal("org_abc");
    expect(seen!.params.id).to.equal("mon_42");
    expect(seen!.query.get("range")).to.equal("7d");
  });

  it("falls back when no route matches", () => {
    let hit = false;
    const router = new Router(
      [{ pattern: "/orgs/:orgId", render: () => html`x` }],
      {
        pattern: "*",
        render: () => {
          hit = true;
          return html``;
        },
      },
    );
    history.pushState({}, "", "/nope/deep/link");
    router.outlet();
    expect(hit).to.be.true;
  });

  it("a guard returning a path redirects, and the protected render does not run", async () => {
    let rendered = false;
    const router = new Router([
      {
        pattern: "/orgs/:orgId/settings",
        guard: () => "/login",
        render: () => {
          rendered = true;
          return html``;
        },
      },
    ]);
    history.pushState({}, "", "/orgs/org_x/settings");
    router.outlet();
    // the guard navigates on a microtask after the render settles
    await new Promise((r) => setTimeout(r, 0));
    expect(rendered).to.be.false;
    expect(location.pathname).to.equal("/login");
  });

  it("a guard returning null allows the route to render", async () => {
    let rendered = false;
    const router = new Router([
      {
        pattern: "/orgs/:orgId",
        guard: () => null,
        render: () => {
          rendered = true;
          return html``;
        },
      },
    ]);
    history.pushState({}, "", "/orgs/org_y");
    // the guarded render resolves through until(); drive the promise
    router.outlet();
    await new Promise((r) => setTimeout(r, 0));
    expect(rendered).to.be.true;
  });
});
