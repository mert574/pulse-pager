// App bootstrap. Defines the org-scoped route table, creates the router, mounts it
// on <app-root>, and starts it.
//
// Routing model (RFC-013 section 5.3): account/auth routes are global; product
// routes live under /orgs/:orgId so the active org is the URL path. Guards run
// before render: requireAuth sends a logged-out user to /login, orgGuard sends a
// non-member off an org they are not in.
//
// Feature views (RFC-013 section 7) are not built yet; their routes render
// <view-placeholder> for now. When a view lands, swap its target for a lazy import
// so it becomes its own chunk (the router's async render supports this):
//
//   render: () => import("./components/monitors-list-view.js")
//     .then(() => html`<monitors-list-view></monitors-list-view>`)

import {
  Router,
  registerRouter,
  type Route,
  type RouteContext,
} from "./router.js";
import type { AppRoot } from "./components/app-root.js";
import { session } from "./state/session.js";

import "@fontsource-variable/inter/index.css";
import "uplot/dist/uPlot.min.css";
import "./styles/app.css";

// shell + shared components (the app entry bundle, RFC-013 section 2.2)
import "./components/app-root.js";
import "./components/login-view.js";
import "./components/status-badge.js";
import "./components/confirm-dialog.js";
import "./components/form-field.js";
import "./components/upsell-banner.js";
import "./components/view-placeholder.js";

import { html } from "lit";
import { initLocale } from "./i18n.js";

// pick the initial locale (stored choice, browser language, or English) before
// anything renders. The theme is set even earlier by the inline script in index.html.
initLocale();

// --- guards ---

// Require a logged-in session. The shell already gates the outlet on login, so
// this mainly preserves return_to for deep links resolved very early.
function requireAuth(): string | null {
  if (session.isLoggedIn) return null;
  const returnTo = encodeURIComponent(window.location.pathname);
  return `/login?return_to=${returnTo}`;
}

// Require membership in the route's :orgId. A non-member is sent to a home org if
// they have one, else to account. The server is still authoritative; this is the
// usability mirror (RFC-013 section 4).
function orgGuard(ctx: RouteContext): string | null {
  const auth = requireAuth();
  if (auth) return auth;
  const me = session.me;
  if (!me) return "/login";
  const orgId = ctx.params.orgId;
  if (me.orgs.some((o) => o.org_id === orgId)) return null;
  return me.orgs.length ? `/orgs/${me.orgs[0].org_id}` : "/account";
}

// Placeholder render until the real feature view lands (see file header).
const placeholder = (name: string) => () =>
  html`<view-placeholder name=${name}></view-placeholder>`;

const routes: Route[] = [
  // global / account
  { pattern: "/login", render: () => html`<login-view></login-view>` },
  {
    // reachable pre-login and without an active org: the accept view loads the
    // token preview itself and routes through login when the user is not signed in
    pattern: "/invitations/:token",
    render: (ctx) =>
      import("./components/invite-accept-view.js").then(
        () =>
          html`<invite-accept-view
            .token=${ctx.params.token}
          ></invite-accept-view>`,
      ),
  },
  {
    pattern: "/account",
    guard: requireAuth,
    render: () =>
      import("./components/account-view.js").then(
        () => html`<account-view></account-view>`,
      ),
  },
  {
    pattern: "/orgs/new",
    guard: requireAuth,
    render: () =>
      import("./components/org-create-view.js").then(
        () => html`<org-create-view></org-create-view>`,
      ),
  },

  // org-scoped product routes
  {
    pattern: "/orgs/:orgId",
    guard: orgGuard,
    // lazy chunk (RFC-013 section 2.2): the view loads on first navigation here
    render: () =>
      import("./components/monitors-list-view.js").then(
        () => html`<monitors-list-view></monitors-list-view>`,
      ),
  },
  {
    pattern: "/orgs/:orgId/monitors/new",
    guard: orgGuard,
    render: () =>
      import("./components/monitor-form-view.js").then(
        () => html`<monitor-form-view></monitor-form-view>`,
      ),
  },
  {
    pattern: "/orgs/:orgId/monitors/:id/edit",
    guard: orgGuard,
    render: (ctx) =>
      import("./components/monitor-form-view.js").then(
        () =>
          html`<monitor-form-view
            .monitorId=${ctx.params.id}
          ></monitor-form-view>`,
      ),
  },
  {
    pattern: "/orgs/:orgId/monitors/:id",
    guard: orgGuard,
    render: (ctx) =>
      import("./components/monitor-detail-view.js").then(
        () =>
          html`<monitor-detail-view
            .monitorId=${ctx.params.id}
          ></monitor-detail-view>`,
      ),
  },
  {
    pattern: "/orgs/:orgId/channels",
    guard: orgGuard,
    render: () =>
      import("./components/channels-list-view.js").then(
        () => html`<channels-list-view></channels-list-view>`,
      ),
  },
  {
    pattern: "/orgs/:orgId/channels/new",
    guard: orgGuard,
    render: () =>
      import("./components/channel-form-view.js").then(
        () => html`<channel-form-view></channel-form-view>`,
      ),
  },
  {
    pattern: "/orgs/:orgId/channels/:id/edit",
    guard: orgGuard,
    render: (ctx) =>
      import("./components/channel-form-view.js").then(
        () =>
          html`<channel-form-view
            .channelId=${ctx.params.id}
          ></channel-form-view>`,
      ),
  },
  {
    pattern: "/orgs/:orgId/incidents",
    guard: orgGuard,
    render: () =>
      import("./components/incidents-view.js").then(
        () => html`<incidents-view></incidents-view>`,
      ),
  },
  {
    pattern: "/orgs/:orgId/incidents/:id",
    guard: orgGuard,
    render: (ctx) =>
      import("./components/incident-detail-view.js").then(
        () =>
          html`<incident-detail-view
            .incidentId=${ctx.params.id}
          ></incident-detail-view>`,
      ),
  },
  {
    pattern: "/orgs/:orgId/status-pages",
    guard: orgGuard,
    render: () =>
      import("./components/status-pages-list-view.js").then(
        () => html`<status-pages-list-view></status-pages-list-view>`,
      ),
  },
  {
    pattern: "/orgs/:orgId/status-pages/new",
    guard: orgGuard,
    render: () =>
      import("./components/status-page-form-view.js").then(
        () => html`<status-page-form-view></status-page-form-view>`,
      ),
  },
  {
    pattern: "/orgs/:orgId/status-pages/:id/edit",
    guard: orgGuard,
    render: (ctx) =>
      import("./components/status-page-form-view.js").then(
        () =>
          html`<status-page-form-view
            .statusPageId=${ctx.params.id}
          ></status-page-form-view>`,
      ),
  },
  {
    pattern: "/orgs/:orgId/members",
    guard: orgGuard,
    render: () =>
      import("./components/members-view.js").then(
        () => html`<members-view></members-view>`,
      ),
  },
  {
    pattern: "/orgs/:orgId/api-keys",
    guard: orgGuard,
    render: () =>
      import("./components/api-keys-view.js").then(
        () => html`<api-keys-view></api-keys-view>`,
      ),
  },
  {
    pattern: "/orgs/:orgId/billing",
    guard: orgGuard,
    render: () =>
      import("./components/billing-view.js").then(
        () => html`<billing-view></billing-view>`,
      ),
  },
  {
    pattern: "/orgs/:orgId/settings",
    guard: orgGuard,
    render: placeholder("settings"),
  },
];

const fallback: Route = {
  pattern: "*",
  render: placeholder("not found"),
};

const router = new Router(routes, fallback);
registerRouter(router);

const root = document.querySelector<AppRoot>("app-root");
if (!root) {
  throw new Error("app-root element not found in index.html");
}
root.router = router;
root.startRouter();
