# RFC-013 - Frontend

Status: DRAFT for review
Author: Principal Frontend Architecture
Audience: frontend engineers, api authors (RFC-003/RFC-012), deployment (RFC-011)
Parent: `docs/rfc/RFC-000-architecture-overview.md` (section 2.1 api, section 3 nginx serving, section 11.1 ingress, section 14 reuse)
Depends on: RFC-000, RFC-003 (auth: cookie JWT, refresh, org-context via `/orgs/{id}`), RFC-012 (API contract, id codec, error envelope, pagination). RFC-012 is not yet written; where it is needed this RFC leans on RFC-000 section 5/8 and the master PRD section 9 and flags the seam.
Product source of truth: master PRD section 10 (UX surfaces), section 14 (North Star), PRD-001 (orgs/members/account), PRD-002 (monitors/incidents), PRD-003 (channels), PRD-004 (status pages), PRD-005 (api keys/webhooks), PRD-006 (billing/entitlements), PRD-007 (multi-region UI).

House style: no em-dashes. Tables and diagrams over prose. Every load-bearing choice states the decision, the reasoning, and the rejected alternatives.

---

## 1. Overview, scope, and owned contracts

### 1.1 What this RFC is

The frontend is a single Lit 3 + TypeScript SPA, built with Vite, served as static assets by nginx, that talks to the api service over `/api`. It is the human face of every product surface in master PRD section 10. This RFC evolves the existing `web/` foundation (built in the v1 monolith era and carried forward per RFC-000 section 14); it does not start over. The proven shape (hand-rolled History router, typed fetch client, reactive session holder, app shell, status badge, confirm dialog) carries; what is new is everything the multi-tenant SaaS adds on top: social login, cookie-borne JWT with refresh-retry, the org switcher and org-scoped routing, role-aware and entitlement-aware UI, the many feature views, and a separately served public status page.

### 1.2 Scope

In scope: the SPA stack and build, the nginx serving model and how the SPA reaches api, cookie-based auth handling (login redirect, session bootstrap, the 401 -> refresh -> retry -> login interceptor, logout and log-out-all, CSRF echo), the multi-org active-org model and switcher, routing for every SaaS surface, dependency-light state with a small session/org/entitlements context, the feature view inventory, the public status page rendering and its serving model, accessibility/i18n-readiness/theming, error and form UX, and testing.

Out of scope (owned elsewhere): the auth protocol itself (RFC-003), the REST contract and OpenAPI spec and id codec (RFC-012), nginx/Helm/Terraform/TLS/cert runtime (RFC-011), the status-page read serving on the api side (RFC-000 section 2.1, RFC-012), and all server-side authz and entitlement enforcement (RFC-003, RFC-009). The UI mirrors authz and entitlements for usability; the server is always authoritative.

### 1.3 Contracts this RFC owns

| Contract | Decision |
|----------|----------|
| Serving | nginx serves the built SPA static assets and reverse-proxies `/api` and `/auth` to the api service, same-origin (section 2) |
| Token carriage | the SPA holds no token in JS; auth rides httpOnly cookies set by api; the SPA only knows "logged in" from `GET /api/v1/me` (section 3) |
| 401 handling | the api client does one refresh-then-retry on a 401, then redirects to login if refresh fails (section 3) |
| CSRF | the client echoes the `pulse_csrf` cookie in an `X-CSRF-Token` header on every unsafe request (section 3) |
| Active org | org id lives in the URL path under `/orgs/{orgId}/...`; the switcher changes the path, never reissues a token (section 4) |
| Router | the existing hand-rolled History router is kept and extended; not replaced by a library (section 5) |
| Public status page | served as a separate lightweight build (own entry, own bundle), unauthenticated, cache-first (section 8) |

### 1.4 What carries forward vs what is new

| Foundation piece (`web/src/...`) | Fate |
|----------------------------------|------|
| `router.ts` (History API, `:id` params, base-path, link interception, `navigate()`) | reused, extended with `/orgs/{orgId}` scoping and a few new patterns (section 5) |
| `api/client.ts` (typed fetch, `credentials: include`, `ApiError` carrying `code`/`message`/`fields`) | reused, the 401 branch evolves from "clear + go to login" to "refresh -> retry -> login"; CSRF header added; `/api/v1` + org-path prefixing added (section 3) |
| `api/types.ts` (wire types, snake_case, redaction `*_set`, `Page<T>` cursor) | reused, extended with org/region/down_policy/plan/status-page/member/api-key/billing shapes; `Me` shape changes (section 3.2) |
| `state/session.ts` (reactive holder, no store lib, `isLoggedIn` from `/me`) | reused, extended into a small session+org+entitlements context (section 6) |
| `components/app-root.ts` (shell, session bootstrap, outlet) | reused, extended: bootstrap calls `/me`, org switcher mounts in the nav, role/entitlement context provided here (section 6) |
| `components/app-nav.ts` | reused, extended with the org switcher and the full SaaS nav (section 4, 7) |
| `components/login-view.ts` | replaced: username/password form becomes "Sign in with Google / GitHub" redirect buttons (section 3.1) |
| `components/status-badge.ts`, `components/confirm-dialog.ts` | reused; status badge gains a `coverage-degraded` variant; both move to light DOM and restyle with daisyUI classes (section 2.5) |
| `components/view-placeholder.ts` | removed once feature views land |
| `styles/tokens.css`, `styles/global.css` | replaced by one Tailwind + daisyUI stylesheet; the palette and dark theme come from daisyUI themes, not hand-rolled tokens (section 2.5, 9.3) |
| login = username/password, `Me.username`, 401 -> login | replaced by RFC-003 social login + cookie JWT + refresh-retry |

Net: the plumbing (router, client, session, shell, two shared components, the token CSS) is proven and stays. The auth model, the org dimension, the feature views, and the public status page are the new work.

---

## 2. Stack, build, and serving

### 2.1 Stack

| Choice | Value | Note |
|--------|-------|------|
| Framework | Lit 3 (`lit ^3`) | already in `package.json`; web components, no virtual DOM, tiny runtime |
| Language | TypeScript (strict) | already configured; wire types mirror the API |
| Bundler | Vite 6 | already configured; ES modules, fast HMR, Rollup production build |
| Styling | Tailwind CSS v4 + daisyUI v5 | utility classes + a small component/theme layer, applied in light DOM (section 2.5) |
| Charts | uPlot | tiny time-series charts; colors read from the daisyUI theme vars so they track light/dark (decision D12) |
| Tables | daisyUI `.table` + TanStack Table (`@tanstack/table-core`) | daisyUI styles the markup; TanStack (headless, framework-agnostic) adds sort/filter/paginate (decision D13) |
| Typography | Inter (self-hosted, `@fontsource-variable/inter`) | a real product typeface, not the system stack; no CDN |
| Icons | lucide (inline, authored set in `src/icons.ts`) | consistent icon set; inlined so it works the same in Vite and the esbuild test runner |
| Status codes | `http-status-codes` | reason phrases for the results table (e.g. "503 Service Unavailable") |

UX patterns that come with this stack: a small toast service (`toast()` + `<toast-host>`) for action feedback instead of inline alerts, daisyUI `skeleton` loaders instead of spinners, the signature uptime bar strip (`<uptime-bar>`, section 7.1), and crafted empty states (icon + CTA).
| Rendering | Lit in light DOM (`createRenderRoot` returns `this`) | so the global Tailwind/daisyUI classes reach component markup (section 2.5) |
| Runtime deps | `lit` plus `@lit/context` (section 6); Tailwind + daisyUI are build-time CSS, not runtime JS | no Redux, no router lib, no JS component library; daisyUI ships CSS only, our components stay our own |

### 2.2 Build output and code-splitting

The app is now much larger than the v1 four-view SPA, so we move from one bundle to route-level code-splitting. Vite/Rollup splits on dynamic `import()`, and the router's `render` becomes async-capable so each feature view is a lazy chunk.

| Bundle | Loaded when | Contents |
|--------|-------------|----------|
| `app` entry (`index.html`) | first authed visit | shell, nav, org switcher, router, api client, session/context, login view, status-badge, confirm-dialog |
| per-route chunks | on navigation to that route | monitors, monitor-detail, monitor-form, channels, incidents, status-page editor, members, api-keys, billing, settings, account |
| `status` entry (`status.html`) | public status page only | a separate build, never imports the authed app (section 8) |

Bundle budget (gzipped, enforced in CI as a size check on the build output):

| Target | Budget |
|--------|--------|
| `app` initial entry (shell + first route), JS | <= 60 KB |
| any lazy route chunk, JS | <= 40 KB |
| `status` public entry (whole page), JS | <= 30 KB |
| app CSS (Tailwind utilities used + the two daisyUI themes) | <= 40 KB |
| `status` CSS | <= 25 KB |

These are starting budgets; CI fails the build if a chunk grows past its budget so bloat is caught at the PR, not in production. Lit + the shared shell sit comfortably under 60 KB, so the budget leaves room for the first route. Tailwind emits only the utility classes actually used and daisyUI is pinned to two themes (section 9.3), so the CSS stays small; the budget catches a regression (for example pulling in all daisyUI themes).

### 2.3 nginx serving model (decision)

Decision: nginx serves the SPA static assets and reverse-proxies `/api` and `/auth` to the api service on the same origin. The SPA never calls a separate api origin. nginx also serves SPA-fallback (any unknown non-asset, non-`/api`, non-`/auth` path returns `index.html`) so deep links into client routes work.

```
browser
  | https://app.pulse.app/...
  v
+-------------------- nginx (RFC-000 1.1, 11.1) --------------------+
|  location /api/   -> proxy_pass api service                       |
|  location /auth/  -> proxy_pass api service (OAuth + refresh)     |
|  location /assets -> static, long cache, content-hashed           |
|  location /       -> try_files $uri /index.html  (SPA fallback)   |
+------------------------------------------------------------------+
  status pages: status-page read path (RFC-000 1.1), served from
  {org-slug}.pulse.app, cache-first, the separate `status` build (section 8)
```

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| nginx serves static + reverse-proxies `/api` same-origin (chosen) | chosen | Same-origin is what makes RFC-003's cookie auth work cleanly: the httpOnly `pulse_at`/`pulse_rt` cookies are first-party, `SameSite=Lax` is effective, and there is no CORS preflight on every call. RFC-003 section 4.4 explicitly assumes "the SPA is served same-origin behind the same nginx that proxies `/api`." It also lets nginx cache and own the resilient status-page path (RFC-000 section 3) |
| SPA calls the api origin directly (`api.pulse.app`) from `app.pulse.app` | rejected | Cross-origin cookies need `SameSite=None; Secure` plus CORS with credentials on every endpoint, widening CSRF surface and adding a preflight round trip to each call. It directly contradicts RFC-003's same-origin cookie decision. No upside for our single-frontend topology |
| nginx serves static only, SPA uses bearer tokens in JS to a separate api | rejected | Putting the token in JS is exactly the XSS exposure RFC-003 section 4.4 rejected. The cookie model requires same-origin |

This is consistent with RFC-000 section 3 ("nginx serves the built SPA static assets and proxies `/api` to the api service") and RFC-003's same-origin cookie premise. The base-path support already in `router.ts` and `index.html` (the `<base href>` rewrite) is kept for self-host sub-path deployments; in the hosted SaaS the base is `/`.

### 2.4 Dev vs prod

Dev keeps the existing Vite proxy (`vite.config.ts` proxies `/api` and adds `/auth`) so HMR works against a local api. Prod is the nginx model above. The api client's path handling is identical in both because both serve `/api` at the origin root.

### 2.5 Styling and theming: Tailwind + daisyUI in light DOM (decision)

Decision: style the SPA with Tailwind CSS v4 plus daisyUI v5 (daisyUI's `data-theme` themes provide the palette and a small component layer). Lit components render in light DOM (`createRenderRoot()` returns `this`) so the global Tailwind/daisyUI classes apply to their markup. This replaces the hand-rolled `tokens.css` palette and the per-component shadow-DOM CSS.

This reverses the foundation's earlier "no component library, hand-rolled tokens, encapsulated shadow DOM" choice. The reason: hand-rolling and maintaining a palette, spacing scale, dark theme, and every control's CSS by hand is slow and drifts; daisyUI gives a consistent themed component layer and Tailwind gives utilities, both small and well understood, and the team prefers it.

Why daisyUI/Tailwind need light DOM:

| Fact | Consequence |
|------|-------------|
| Tailwind utilities and daisyUI component classes (`.btn`, `.card`, `bg-base-100`) live in one global stylesheet | global classes do not cross a shadow boundary, so a shadow-DOM component cannot see them |
| daisyUI theme tokens are CSS custom properties on `[data-theme]` | custom properties DO inherit across shadow boundaries, so theme colors would reach shadow DOM, but the classes that consume them would not |
| The app is one first-party SPA, not a distributed component library | style encapsulation buys little here; the cost of bridging it is not worth paying |

The shadow-DOM bridge options considered:

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| Light DOM: `createRenderRoot()` returns `this` (chosen) | chosen | The idiomatic daisyUI setup and the simplest: global classes and `data-theme` just work, no per-component plumbing, the build is one CSS file. The app is not a reusable widget library, so losing encapsulation is acceptable. Costs: `form-field` stops using `<slot>` (it renders its control directly), and `confirm-dialog`'s focus trap queries `this` instead of `this.shadowRoot`; both are small, localized changes |
| Shadow DOM + a shared compiled Tailwind/daisyUI stylesheet adopted by a base class | rejected | Preserves encapsulation and slots, but adds a base class and a build step to compile and adopt the sheet in every component, and daisyUI is designed global-first. More plumbing for encapsulation we do not need |
| Keep the hand-rolled `tokens.css`, no Tailwind/daisyUI | rejected | The status quo we are moving away from; slow to extend and drifts |

Implications across the codebase:

| Area | Change |
|------|--------|
| Render root | a small base element sets `createRenderRoot()` to return `this`; components extend it instead of `LitElement` directly |
| Component CSS | the per-component `static styles` blocks are removed; styling moves to Tailwind/daisyUI classes in the templates |
| `form-field` | renders its control directly (children passed in markup), not via `<slot>`; it still renders the per-field error from the envelope |
| `confirm-dialog` | unchanged behavior; the focus trap and active-element queries use `this` (light DOM) instead of `this.shadowRoot` |
| Global CSS | one app stylesheet imports Tailwind and the daisyUI plugin; `tokens.css`/`global.css` are folded into it or removed |
| Public status page | same stack; its own entry imports the same CSS pipeline, restricted to what it uses (still under the status CSS budget) |

---

## 3. Auth handling

### 3.1 Login flow (social, redirect-based)

RFC-003 is social-only (Google, GitHub), no passwords. The v1 username/password `login-view` is replaced by a view with two redirect buttons. Login is a full-page redirect, not an XHR, because OAuth needs the browser to leave to the provider and come back.

```
login-view                 nginx -> api                     provider
  | click "Sign in with Google"
  | window.location = /auth/google/login?return_to=/orgs/...   (full nav)
  |------------------------------------------------------------>|
  |                          ... RFC-003 section 2.5 round trip ...
  |                          api sets httpOnly pulse_at + pulse_rt + pulse_csrf
  |                          302 to return_to (allowlisted) or app home
  |<------------------------------------------------------------|
  | SPA boots, app-root calls GET /api/v1/me -> 200 -> logged in
```

The SPA never sees the code, the token exchange, or the tokens. It only initiates the redirect and, on return, discovers it is logged in via `/me`. `return_to` is passed so an invitation deep link survives the round trip (RFC-003 section 2.2 allowlists it server-side).

### 3.2 Session bootstrap

`app-root` on connect calls `GET /api/v1/me`, the canonical bootstrap path (RFC-012 section 5.1; RFC-003 cites the same path). The response carries the render-only claims plus the org list, so the switcher has its data on first paint:

```ts
// api/types.ts - Me replaces the v1 { username }
export interface Me {
  user_id: string;
  email: string;
  name: string;
  avatar_url: string | null;
  orgs: OrgMembership[];            // every org the user belongs to, with role
}
export interface OrgMembership {
  org_id: string;                   // RFC-012 id codec (opaque string)
  name: string;
  slug: string;
  role: "owner" | "admin" | "member" | "viewer";
  plan: "free" | "starter" | "team" | "business";
}
```

This is the "claims-for-render" shape RFC-003 section 4.4 and Q4 ask RFC-013 to own: the SPA reads identity for display from `/me`, never from a token (the token stays httpOnly). `session.ts` holds this; `checked` still gates the initial render so the login screen does not flash during bootstrap (the existing pattern carries).

### 3.3 The 401 -> refresh -> retry interceptor (decision)

The v1 client does "401 -> clear session -> go to /login" inline in `request()`. That carries the right shape (one central place) but must evolve: a 401 usually just means the 15-minute access token expired, and RFC-003 has an opaque refresh token in the path-scoped `/auth` cookie. So the interceptor tries to refresh once and replays the original request before giving up.

```
request(path, opts)
  -> fetch (cookies auto-sent; X-CSRF-Token echoed on unsafe methods)
  -> 401?
       no  -> normal path (2xx, or throw ApiError from the envelope)
       yes -> single-flight refresh:
                POST /auth/refresh  (sends path-scoped pulse_rt cookie)
                  ok  -> api rotated cookies; retry the ORIGINAL request once
                           retry 401 again -> give up: session.clear(); navigate(login)
                  fail-> give up: session.clear(); navigate(login)
```

Rules that make this correct and not a refresh storm:

| Rule | Why |
|------|-----|
| Single-flight refresh | if ten requests 401 at once (token just expired), they await one shared refresh promise, not ten parallel `/auth/refresh` calls. The refresh result resolves all of them, then each retries once |
| Retry exactly once | a 401 that survives a successful refresh is a real auth failure (revoked, removed, reuse-detected family revoke per RFC-003 section 4.1), so we stop and go to login. No retry loop |
| Refresh failure is terminal | RFC-003 reuse-detection or an expired refresh returns non-2xx from `/auth/refresh`; we clear and redirect, no retry |
| `/auth/refresh` and `/auth/login` themselves never trigger the interceptor | avoids recursion |
| 403 is not 401 | a 403 (`forbidden` from the role gate, or `entitlement_exceeded`) is surfaced to the view, never refreshed. Refresh cannot fix authorization |

This keeps the "one central place auth expiry is handled" property of the foundation while honoring RFC-003's short-access-token + refresh design, so a user is not bounced to login every 15 minutes.

### 3.4 CSRF

Because auth is cookie-borne, RFC-003 section 4.5 mandates double-submit CSRF. The client reads the non-httpOnly `pulse_csrf` cookie and echoes it as `X-CSRF-Token` on every unsafe method (POST/PUT/PATCH/DELETE). GET/HEAD send nothing extra. This is the SPA half of RFC-003's double-submit; api compares header to cookie and also checks `Origin`. The CSRF cookie is the only cookie JS reads; the tokens stay unreadable.

### 3.5 Logout and log out all devices

| Action | Call | UI |
|--------|------|----|
| Log out (this device) | `POST /auth/logout` then `session.clear()` and redirect to login | the nav button (carried from `app-nav`) |
| Log out all devices | `POST /api/v1/account/logout-all` (RFC-003 section 4.3) then same local clear | Account settings, with a confirm-dialog |

Both clear the local session holder and navigate to login; the cookies are cleared server-side. The existing `onLogout` in `app-nav` carries, pointed at the new endpoint.

### 3.6 Auth contract summary

| Concern | SPA behavior | Owned by |
|---------|--------------|----------|
| Token storage | none in JS; httpOnly cookies | RFC-003 |
| Login | full-page redirect to `/auth/{provider}/login` | RFC-003 |
| Session truth | `GET /api/v1/me` 200 == logged in | this RFC |
| Expiry | refresh-then-retry once, else login | this RFC |
| CSRF | echo `pulse_csrf` -> `X-CSRF-Token` on unsafe methods | this RFC + RFC-003 |
| Logout / log-out-all | POST then local clear | RFC-003 endpoints |

---

## 4. Multi-org and the org switcher

### 4.1 Active-org model (URL path is the authority)

RFC-003 section 6.2 fixes the active org as request-supplied in the path under `/orgs/{org_id}` for the SPA, checked against membership every request, with no token reissue on switch. The SPA mirrors this exactly: the active org is whatever `{orgId}` is in the current route, full stop. There is no separate client-side "active org" variable that can drift from the URL.

| Property | Decision |
|----------|----------|
| Where active org lives | the URL path segment `/orgs/{orgId}/...`; the route is the single source of truth |
| What the api client does | it reads `{orgId}` from the current route and prefixes org-scoped calls as `/api/v1/orgs/{orgId}/...`; account-scoped calls (`/me`, account settings, log-out-all) are not org-prefixed |
| What a switch does | navigate to the same logical view under the new org id (or that org's home), which re-fetches data for the new org. No token reissue (RFC-003 section 3.2), no server session mutation |
| Two-tab safety | two tabs on different orgs just have different URLs; nothing global is mutated, so they do not fight (RFC-003 rejected the server-side mutable active org for this reason) |
| Last-used org | a non-authoritative cookie/localStorage hint may remember the last org so a bare visit to `/` lands there; the path still wins for any actual request |

### 4.2 The org switcher component

`<org-switcher>` mounts in `app-nav`. It lists `session.me.orgs` (already loaded at bootstrap), shows the active org and its role, and offers "Create organization" (available to any role per PRD-001). Selecting an org calls `navigate("/orgs/{newOrgId}")` (its home), which the router resolves and which re-fetches. Switching is a pure client navigation; nothing is written server-side.

### 4.3 Role-aware UI (mirrors authz, server authoritative)

The active org's `role` (from `/me`) drives show/hide/disable of actions, mirroring the RFC-003 section 7.2 / PRD-001 matrix so a viewer does not see a "Delete monitor" button that would only 403. This is a usability mirror, never a security boundary: every guarded action still hits the server, which re-checks the role (RFC-003 section 6.3, role read fresh per request). A small `can(action)` helper reads the active-org role against the same capability table:

| Role | What the UI exposes (summary, full matrix in RFC-003 7.2) |
|------|------------------------------------------------------------|
| viewer | read everything; no create/edit/delete, no test, no incident updates, no settings |
| member | + create/edit/delete monitors and channels, send test, annotate incidents, create/edit/publish status pages |
| admin | + invite/remove members, change roles (not owner), revoke invites, manual-close incidents, view audit, edit org settings, create/revoke api keys, view billing |
| owner | + transfer ownership, manage billing/payment, delete org; these are owner-only and never reachable by an api key |

If `/me` and the server ever disagree (a role just changed), the server wins and the UI shows the resulting 403 in the standard error surface (section 10), and a `/me` refresh corrects the menus.

---

## 5. Routing (decision: keep the hand-rolled router, extended)

### 5.1 Decision

Decision: keep the existing hand-rolled History-API router (`router.ts`) and extend it; do not adopt `@lit-labs/router` or another library.

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| Keep + extend the hand-rolled router (chosen) | chosen | It already does everything the SaaS needs: History API, `:id` params, query parsing, base-path support, in-app link interception, a `navigate()` singleton, and a single `outlet()` re-render hook the shell already uses. It is ~170 lines, zero dependency, fully understood, and the foundation is built on it. The only gaps are async/lazy `render` (for code-splitting) and a route guard hook for auth/org, both small additive changes |
| `@lit-labs/router` | rejected | It would replace a working, understood, zero-dep router to gain little we lack. It is also still labs-stage. Swapping the router touches `main.ts`, `app-root`, `app-nav`, and every `navigate()` caller for no functional win. We would adopt it only if we needed nested/outlet routing or data-loading conventions we do not |
| A heavier SPA router | rejected | Over-built for a flat-ish route table; pulls a dependency and a mental model the team does not need |

### 5.2 Additive changes to the router

| Change | Why |
|--------|-----|
| `render` may return a `Promise<TemplateResult>` or a template that lazy-imports its view | enables route-level code-splitting (section 2.2); the outlet renders a loading state until the chunk resolves |
| an optional per-route `guard(ctx)` returning a redirect path or null | lets a route require auth (redirect to login if `!session.isLoggedIn`) and require membership in `{orgId}` (redirect to org home or a "no access" view) before render |
| `/orgs/:orgId/...` patterns | the org dimension; the existing `:param` capture already supports this with no engine change |

### 5.3 Route table

Org-scoped routes live under `/orgs/:orgId`; account and auth routes are global; the public status page is a different build (section 8), not in this table.

| Path | View | Min role (UI mirror) |
|------|------|----------------------|
| `/login` | `<login-view>` (social buttons) | public |
| `/invite/:token` | `<invite-accept-view>` | authed (login leg if cold, RFC-003 2.6) |
| `/account` | `<account-settings-view>` (profile, linked providers, sessions, log-out-all, delete account, orgs list) | self |
| `/orgs/:orgId` | `<monitors-list-view>` (org home) | viewer+ |
| `/orgs/:orgId/monitors/new` | `<monitor-form-view>` create | member+ |
| `/orgs/:orgId/monitors/:id` | `<monitor-detail-view>` | viewer+ |
| `/orgs/:orgId/monitors/:id/edit` | `<monitor-form-view>` edit | member+ |
| `/orgs/:orgId/channels` | `<channels-view>` | viewer+ (edit member+) |
| `/orgs/:orgId/incidents` | `<incidents-view>` | viewer+ (annotate member+, close admin+) |
| `/orgs/:orgId/status-pages` | `<status-pages-list-view>` | viewer+ |
| `/orgs/:orgId/status-pages/:id/edit` | `<status-page-editor-view>` | member+ |
| `/orgs/:orgId/members` | `<members-view>` (list, invite, roles, transfer) | viewer reads, admin+ manages |
| `/orgs/:orgId/api-keys` | `<api-keys-view>` | admin+ |
| `/orgs/:orgId/billing` | `<billing-view>` (plan, usage, upgrade) | admin reads, owner manages |
| `/orgs/:orgId/settings` | `<org-settings-view>` (name, slug, defaults, SSRF display, audit, delete) | admin+ (delete owner) |
| `*` | not-found | public |

The `:orgId` (and any resource id) is an opaque string per the RFC-012 id codec; the SPA treats ids as strings end to end (the v1 `number` ids in `types.ts` become strings under RFC-012).

---

## 6. State management

### 6.1 Decision: dependency-light, one small context

Decision: keep the foundation's per-view `@state` plus the reactive `session` holder, and add exactly one light shared context (`@lit/context`) carrying session + active-org + entitlements. No Redux, no MobX, no global store framework.

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| Per-view `@state` + a small `@lit/context` for session/org/entitlements (chosen) | chosen | The foundation already proves per-view local state works and the `session` holder already does pub/sub. The only thing the larger app adds is the need for many deep components (nav, switcher, every guarded button, every upsell) to read the same session/role/entitlements without prop-drilling. `@lit/context` is the idiomatic Lit answer: ~1 KB, provider on `app-root`, consumers via a decorator. It is the smallest possible step up |
| Redux / a global store library | rejected | Heavy, ceremony for an app whose server is the source of truth and whose per-view data is fetched on demand. We are not managing complex client-derived state |
| Keep only the bare pub/sub `session` singleton | rejected | Works but forces either prop-drilling role/entitlements through the tree or every component importing the singleton directly; context is cleaner for the cross-cutting reads and is still dependency-light |

The context value:

```ts
interface AppContext {
  me: Me;                          // identity + orgs (from /me)
  activeOrg: OrgMembership;        // derived from the route :orgId
  role: Role;                      // activeOrg.role, drives can()
  entitlements: Entitlements;      // active org's plan limits + current usage (section 6.3)
  refreshMe(): Promise<void>;      // re-pull /me after role/org change
}
```

`app-root` provides it (extending its existing bootstrap), feature views consume it. `can(action)` and the entitlement helpers read from here.

### 6.2 Caching and refetch for lists

| Concern | Decision |
|---------|----------|
| List freshness | fetch on view mount; the api read path is replica-backed and eventually consistent (RFC-000 section 8), so a short staleness is expected and fine for a monitoring dashboard |
| Detail polling | monitor-detail and the monitors list poll on a modest interval (for example 15-30s) so status updates appear without a manual reload; polling pauses when the tab is hidden (`visibilitychange`) |
| Pagination | the existing `Page<T>` cursor (`items` + `next_cursor`) carries; lists (results, incidents, audit, members, api-keys) use cursor paging, "load more" appends |
| Mutations | after a successful create/edit/delete, refetch the affected list (or optimistically update then reconcile); no client cache to invalidate beyond the view |
| No global cache layer | views own their data; we are not building a normalized client cache. If a future need appears we add it behind the api client, not in views |

### 6.3 Surfacing entitlement limits (PRD-006)

The active org's entitlements (plan caps + current usage) come from a billing/usage endpoint and live in the context. The UI uses them to prevent dead-end actions and to upsell, mirroring the server gates (RFC-000 section 12, RFC-009):

| UI surface | Behavior |
|------------|----------|
| At a cap | the create action is disabled with an inline "you have reached your plan limit" and an upgrade link, instead of letting the user fill a form that will 4xx. Example: monitors used == `monitors_cap` disables "New monitor" |
| Plan-floor interval | the monitor form clamps the interval input's minimum to the plan's `min_interval_seconds` and shows the floor (for example "1 min on Team"); going lower shows the upsell |
| Region picker | only `regions_allowed` are selectable; premium regions on a non-premium plan are shown locked with an upsell; the count is capped at `regions_per_monitor_cap` |
| Custom domain | the status-page editor hides/locks custom domain off Team/Business |
| Read-only API (Free) | the api-keys view notes Free keys are read-only |
| Server still authoritative | even with all this, the server returns the entitlement error codes (`monitor_limit_reached`, `interval_below_plan_floor`, `interval_below_hard_floor`, `region_not_in_plan`, `region_count_exceeded`, `seat_limit_reached`, `status_page_limit_reached`, `custom_domain_not_in_plan`, `api_write_not_in_plan`) inside the standard envelope, and the UI renders that error with an upsell if a write slips through |

Plan tier anchors the UI renders against (from PRD-006):

Display names Free / Hobby / Professional / Custom; internal codes free / starter / team / business (pricing.html is the source of truth).

| Dimension | Free | Hobby | Professional | Custom |
|-----------|------|---------|------|----------|
| Monitors cap | 10 | 25 | 50 | custom |
| Min interval | 15 min | 5 min | 1 min | down to 30s |
| Regions/monitor | 1 | 1 | 4 | all |
| Seats included | 1 | 3 | 10 | unlimited |
| Retention | 7d | 30d | 90d | 180d |
| Status pages | 1 | 3 | 10 | custom |
| Custom domain | no | no | yes | yes |
| API | none | read-only | full | full |

---

## 7. Feature views

Each surface is one or more Lit components. Shared building blocks: `status-badge` (reused, plus a `coverage-degraded` variant), `confirm-dialog` (reused for every destructive action), a `<latency-chart>` (uPlot, decision D12) and an uptime sparkline, a `<data-table>` (TanStack Table + daisyUI styling, decision D13) for sortable/paginated lists, a `<form-field>` wrapper that renders the per-field error from the envelope, and an `<upsell-banner>`.

### 7.1 Monitors (PRD-002, PRD-007)

| Component | Contents |
|-----------|----------|
| `<monitors-list-view>` | rows: name, `status` (up/down/disabled/pending) via status-badge, last check time, last latency, `<uptime-sparkline>`, enable toggle, coverage-degraded indicator when raised; "New monitor" (member+, disabled at cap with upsell) |
| `<monitor-detail-view>` | `<latency-chart>` (SVG), uptime % for 24h/7d/90d, per-region status, check-results history table filterable by region, incident timeline with annotations, "Check now" (member+, handles `409` if already running), edit/delete (member+) |
| `<monitor-form-view>` | create/edit. Fields and validation mirror PRD appendix A (section 10): name (<=200), url (http/https), method, headers (key/value/secret, no dup keys, <=50), body (POST/PUT/PATCH only), `expected_status_codes` (explicit or `*xx`), `timeout_seconds` (1..60), `interval_seconds` (>=30 hard floor, >= timeout, >= plan floor), `max_latency_ms`, `body_contains`, `failure_threshold` (>=1), `notification_channel_ids` (multi-select of existing channels), `regions` (picker limited to plan), `down_policy` (any/quorum/all picker, default quorum). Secret header values are write-only: omit to keep, "" to clear (the `*_set` discipline) |

### 7.2 Channels (PRD-003)

| Component | Contents |
|-----------|----------|
| `<channels-view>` | list with name, type (slack/discord/webhook/email), enabled; CRUD (member+); secrets shown only as "configured" via `*_set`, never the value |
| channel form | type-specific config: Slack/Discord `webhook_url` (secret), Webhook `url` (secret) + `custom_headers` (keys shown, values secret), Email `host`/`port`/`username`/`password` (secret)/`from`/`to`/`tls`. Send-test action (member+) that shows delivery success/failure and reason, blocked on a disabled channel |

### 7.3 Incidents (PRD-002)

`<incidents-view>`: org-wide list of open + recently closed, with monitor, started_at, ended_at, duration, cause (failure_reason), close_reason. Annotate (member+) posts a timestamped attributed update. Manual close (admin+) sets `close_reason = manual`, records `closed_by`, fires no recovery alert, and uses a confirm-dialog.

### 7.4 Status pages (editor) (PRD-004)

`<status-pages-list-view>` (count vs cap, "New page" disabled at cap with upsell) and `<status-page-editor-view>`: name (internal), slug (unique in org), `display_monitors` (pick monitors, set mandatory `display_name`, reorder, remove), branding (org name, logo, light/dark theme, accent color), draft/published toggle, custom domain (Team+ only), and a public-link preview that renders the exact public projection (section 8). The editor never exposes internal URLs in the public preview.

### 7.5 Members (PRD-001)

`<members-view>`: member list with roles and joined_at (all roles read); pending invitations with created_at/expires_at/inviter and resend/revoke (admin+); invite by email + target role (admin+); change role and remove (admin+, never to/from owner, never the last owner, with the "transfer ownership first" guard message); transfer ownership and step-down (owner). All destructive actions use confirm-dialog.

### 7.6 API keys (PRD-005)

`<api-keys-view>` (admin+): list with name, role (member/admin), prefix, created-by, created, last-used; create flow shows the full secret exactly once in a copy-once panel with a clear "you will not see this again" warning, then only the prefix remains; revoke is immediate (confirm-dialog). The bearer format and role scoping (member/admin only) are noted in the create UI. Outbound org webhooks (if surfaced in v1) live here or in settings: target url, event types, signing secret shown once, last-delivery status.

### 7.7 Billing and usage (PRD-006)

`<billing-view>`: current plan, usage meters (monitors, seats, status pages as used/cap with at-limit visual), interval floor, retention, region availability. Upgrade/manage launches Stripe checkout/portal by redirecting to a server-provided URL (owner manages, admin reads). Downgrade shows a checklist of blocking conditions (over-cap monitors/seats/pages/regions/custom-domain) that the owner must clear first; clampable conditions (interval, region set) need no UI action. The downgrade button stays disabled until usage is under the target plan; nothing is auto-deleted.

### 7.8 Settings and account (PRD-001)

`<org-settings-view>` (admin+): name, slug, default check settings, read-only SSRF policy display, audit log (admin+, cursor-paged), delete org (owner, confirm, enters 14-day grace). `<account-settings-view>` (self): display name, primary email (read-only), linked providers (connect Google/GitHub via OAuth, disconnect refused if it is the last), sessions and log-out-all, delete account, and the list of orgs the user belongs to with per-row role and a leave action.

### 7.9 Onboarding (North Star)

A new org lands in a guided first-monitor flow (create monitor -> optional channel -> see first check result), because time-to-first-monitor under ~2 minutes is the activation metric (master PRD section 14). The flow reuses the monitor form and channel form; it is a thin wrapper, not separate components, so there is one source of truth for the fields.

---

## 8. Public status page rendering (decision)

### 8.1 Decision: a separate lightweight build

Decision: the public status page is a separate Vite entry (`status.html` + its own bundle) that never imports the authed app, served unauthenticated and cache-first by the status-page read path (RFC-000 section 1.1, 2.1) at `{org-slug}.pulse.app[/{page-slug}]`.

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| Separate lightweight build (chosen) | chosen | Resilience: PRD-004 and PRD-012 require the status page to stay up even when write paths degrade. A separate tiny bundle with no auth, no org switcher, no api client coupling has the smallest possible failure surface and the smallest payload (budget <= 30 KB), which matters because it is cached and served by nginx/CDN off the read path. SEO: a focused entry can set its own `<title>`/description/favicon and is trivially indexable. It cannot be dragged down by a regression in the authed app, and it shares nothing that would force the authed app's auth/CSRF logic onto a public page |
| Same SPA, different route + bundle | rejected | A public route inside the authed app risks pulling auth/session/context code into the public path (or at least the same router and client), and any bootstrap that touches `/me` or cookies is wrong for an unauthenticated, cacheable page. Code-splitting reduces but does not remove the shared-foundation coupling; resilience and SEO both prefer a clean separate entry |
| Server-rendered status page (no SPA at all) | considered, deferred | The strongest resilience/SEO story is server-rendered HTML from the status-page read path. The read serving is owned by api/RFC-012, not this RFC. The separate static build is the frontend-owned step that already gives cache-first resilience and indexability; if product sets a higher status-page SLO (RFC-000 open question 1), a server-rendered or pre-rendered variant is the next move and the separate entry makes that swap localized |

### 8.2 What it renders

Public projection only, friendly names, no internals (PRD-004): overall banner ("All systems operational" / "Partial outage" / "Major outage" derived from the visible monitor mix), each displayed monitor by `display_name` with Operational/Down (status never by color alone, always text + icon), uptime summary (24h/7d/90d, 90d clamped to plan retention and labeled if shorter), and an incident timeline (open: start time; closed: start + duration; public updates: text + UTC timestamp, no staff names). It shows the single reduced verdict per monitor, never per-region detail, region names, or coverage-degraded text. Internal URL, method, headers, assertions, channels, ids are never present in the payload it consumes.

### 8.3 Resilience behavior

| Concern | Behavior |
|---------|----------|
| Cache-first | the page consumes a cacheable read endpoint; nginx/CDN serves last-known-good so a primary write outage does not blank the page (stale-but-served beats unavailable) |
| No write dependency | the public build has no login, no mutations, no CSRF, no `/me`; nothing it does can be blocked by a degraded write path |
| Draft/unpublished | a draft or unknown slug returns the same not-found as a nonexistent page (no existence leak), handled by the read path |
| Branding | logo, accent, theme come from the page's published branding; rendered with the branding CSS variables (section 9) |
| Edge cases | deleted monitor drops from the list and the banner recomputes; zero/all-disabled monitors show an empty state with "All systems operational"; a pending monitor renders Operational |

---

## 9. Accessibility, i18n-readiness, theming

### 9.1 Accessibility baseline

| Area | Baseline |
|------|----------|
| Status never color-only | status-badge and the public page always pair color with a text label and icon (PRD-004 requirement; the badge already shows text) |
| Keyboard | all interactive controls reachable and operable by keyboard; confirm-dialog already handles Escape and focus; dialogs trap focus and restore it on close |
| Semantics | proper roles/labels (`role="dialog" aria-modal` already on confirm-dialog), form inputs associated with `<label>`, live-region announcements for async results (test-send outcome, save success) |
| Contrast | token palette meets WCAG AA in both themes; verified for the status-page light/dark branding |

### 9.2 i18n-readiness

v1 ships English only, but copy is not hardcoded inline as a habit: user-facing strings route through a thin `t(key)` helper (a flat key->string map for now) so a real i18n layer can drop in later without rewriting every component. Dates render via `Intl.DateTimeFormat` from the RFC3339 UTC wire strings, so locale formatting is already correct; the wire stays UTC.

### 9.3 Theming

Theming is daisyUI's `data-theme`, not a hand-rolled palette (section 2.5). The app pins two themes, `light` and `dark`, and sets `data-theme` on `<html>`; dark follows `prefers-color-scheme` by default and a toggle can override it. daisyUI's semantic tokens (`base-100`, `base-content`, `primary`, `error`, and so on) are used everywhere instead of raw hex, so both themes stay consistent and a control never has to be styled twice.

| Surface | Theming |
|---------|---------|
| Product app | the two daisyUI themes (`light`, `dark`); no per-org product theming. Status colors (up/down/disabled/pending/coverage-degraded) map to daisyUI semantic colors (`success`/`error`/`neutral`/`warning` plus one accent), always paired with a text label (section 9.1) |
| Public status page | org branding only: logo, single accent color, light/dark theme. Implemented by setting the daisyUI theme and overriding a small set of brand custom properties (the accent color, the logo) from the page's published branding. No custom fonts, CSS, or layout (PRD-004) |

Only the two themes are emitted, which keeps the CSS within the budget in section 2.2. Overlay/backdrop colors come from the daisyUI theme (for example the `modal` backdrop), so there are no hardcoded `rgba(...)` literals to keep in sync across themes.

---

## 10. Error handling and UX

### 10.1 The error envelope

Every non-2xx carries the standard envelope under `error`: `{ code, message, fields? }` (master PRD section 9; the foundation's `ApiError` already models this). The client throws `ApiError(status, body)` carrying `code`, `message`, and optional per-field `fields`.

| Code | UI treatment |
|------|--------------|
| `validation_failed` | render `fields[name]` inline under each field via `<form-field>`; `message` as a form-level summary; mirror PRD appendix A rules client-side so most errors are caught before submit |
| `unauthenticated` | handled by the interceptor (refresh-retry then login), not shown as a form error |
| `forbidden` | "you do not have permission"; usually prevented by the role-aware UI, this is the backstop |
| `entitlement_exceeded` and the specific cap codes | render the upsell (section 6.3) with an upgrade link, not a raw error |
| `conflict` (for example check-now `409`) | a friendly "already running" / "name taken" message |
| `rate_limited` | "slow down, try again shortly"; the client may read `Retry-After` |
| `not_found` | a not-found view or inline empty state |

### 10.2 Loading, empty, error states

Every list and detail view renders three states explicitly: loading (skeleton or the shell's existing loading style), empty (a helpful empty state with the primary action, for example "Create your first monitor"), and error (the envelope message with a retry). This is a house rule for every feature view, not an afterthought, because empty states are the onboarding path.

### 10.3 Onboarding UX

The first-monitor flow (section 7.9) optimizes the activation metric: minimal required fields, sensible defaults (the form defaults already match PRD-002 defaults: method GET, timeout 10, interval 60, threshold 1, down_policy quorum), and an immediate "check now" so the user sees a first result fast.

---

## 11. Testing

| Layer | What | Tool |
|-------|------|------|
| Component smoke tests | each feature view mounts, renders its loading/empty/error/data states, and fires the right api call on the right action; status-badge, confirm-dialog, form-field, upsell-banner render correctly per props | `@open-wc/testing` + `@web/test-runner` (real-browser web-component testing) |
| Auth interceptor test | the single-flight refresh-then-retry: a 401 triggers one `/auth/refresh`, the original request replays once on success, ten concurrent 401s share one refresh, a 401 after refresh redirects to login, `/auth/refresh` itself never recurses, CSRF header is echoed on unsafe methods | unit test against a mocked fetch |
| Router test | param extraction, `/orgs/:orgId` scoping, guard redirects (no auth -> login, no membership -> org home), lazy chunk loading state | unit test |
| Role/entitlement mirror | `can(action)` matches the matrix; at-cap disables the right action | unit test |
| Public status build | renders banner/monitors/uptime/timeline from a fixture payload, exposes no internal fields, no auth code present in the bundle | component test + a bundle-content assertion |
| Covered by the API contract instead | request/response shapes, error envelope codes, pagination, entitlement enforcement, authz decisions, RLS isolation are the server's contract (RFC-012, RFC-003, RFC-001). The SPA tests that it renders and calls correctly, not that the server enforces correctly; the server has its own suites for that |

The split: the frontend tests behavior it owns (rendering, the interceptor, routing, the UI mirror), and trusts the API contract (RFC-012) and the server-side suites (cross-tenant isolation, authz matrix, entitlement gates) for everything the server is authoritative on. We do not re-test server enforcement from the browser.

---

## 12. Open questions and dependencies

### 12.1 Open questions

| # | Question | Lean |
|---|----------|------|
| Q1 | Exact bootstrap path: RFC-003 references `/auth/me`; this RFC uses `/api/v1/me`. Which is canonical? | RESOLVED: `/api/v1/me` (versioned, under the proxied `/api`). RFC-012 section 5.1 lists it as canonical; RFC-003 cites it |
| Q2 | Does `/me` return the org list and per-org role/plan, or does the switcher need a second call? | return it in `/me` so the switcher and role-aware UI have data on first paint (section 3.2); RFC-012 to confirm the shape |
| Q3 | Server-rendered vs static public status page if product sets a higher status-page SLO (RFC-000 open question 1) | start with the separate static build (section 8); the entry separation makes a later SSR/pre-render swap localized |
| Q4 | API key prefix string in the create UI: `pulse_sk_` (RFC-003) vs `pulse_live_` (PRD-005). | render whatever RFC-012 standardizes; track RFC-003 Q1 |
| Q5 | Outbound org webhooks UI placement (api-keys view vs org settings) and whether it ships in v1 GA | settings, if v1; confirm with PRD-005 phasing |
| Q6 | Polling interval and whether to add SSE/websocket later for live status instead of polling | polling for v1 (section 6.2); live transport is a later additive change |

### 12.2 Dependencies

| RFC | Direction | What |
|-----|-----------|------|
| RFC-003 (auth) | this RFC depends on it | cookie token delivery, `/auth/*` endpoints, refresh semantics, CSRF cookie, `/me` claims-for-render, org-context via `/orgs/{id}`, log-out-all |
| RFC-012 (API) | this RFC depends on it | the REST contract, the id codec (string ids), the error envelope codes, cursor pagination, the entitlement error codes, the exact `/me` and per-endpoint paths, the status-page read endpoint shape |
| RFC-009 (entitlements) | this RFC mirrors it | the plan caps/usage the billing view and the at-cap/upsell logic render against |
| RFC-000 (architecture) | this RFC conforms to it | same-origin nginx serving, the status-page read path, the reuse map |
| RFC-011 (deploy) | this RFC depends on it | the nginx config (static + `/api` proxy + SPA fallback), the wildcard `{org-slug}.pulse.app` TLS and the status-page serving, the CI bundle-budget check |

### 12.3 Deviations flagged

| Deviation | Note |
|-----------|------|
| `Me.username` (v1) -> `Me { user_id, email, name, avatar_url, orgs }` | the v1 wire type is replaced for the social-login multi-org model (section 3.2) |
| login = username/password (v1) -> social redirect | RFC-003 is social-only; `login-view` is rebuilt (section 3.1) |
| 401 -> straight to login (v1 client) -> refresh-then-retry (section 3.3) | required by RFC-003's short access token + refresh |
| numeric resource ids (v1 `types.ts`) -> opaque string ids | per the RFC-012 id codec (section 5.3) |
| SPA bootstrap path `/api/v1/me` | resolved as canonical (RFC-012 section 5.1); RFC-003 and this RFC both cite `/api/v1/me` |

---

## 13. Decisions summary

| # | Decision | Rejected alternative |
|---|----------|----------------------|
| D1 | nginx serves SPA static + reverse-proxies `/api` and `/auth` same-origin | SPA calls a separate api origin (cross-origin cookies, CORS, contradicts RFC-003) |
| D2 | No token in JS; auth via httpOnly cookies; logged-in == `/me` 200 | token in JS memory/localStorage (XSS-reachable) |
| D3 | 401 -> single-flight refresh -> retry once -> else login | retry loop; per-request parallel refresh; straight-to-login on every 401 |
| D4 | CSRF via echoing `pulse_csrf` into `X-CSRF-Token` on unsafe methods | no CSRF defense (cookie auth needs it) |
| D5 | Active org is the URL path `/orgs/{orgId}`; switch = navigate, no token reissue | client-side or server-side mutable active-org variable (two-tab drift) |
| D6 | Keep + extend the hand-rolled router | adopt `@lit-labs/router` or a heavier router for no functional gain |
| D7 | Per-view `@state` + one `@lit/context` for session/org/entitlements | Redux/global store (heavy); bare singleton + prop-drilling |
| D8 | Public status page = separate lightweight build, cache-first, no auth code | a route inside the authed SPA (resilience/SEO coupling) |
| D9 | Route-level code-splitting with enforced bundle budgets (JS and CSS) | one monolithic bundle for the now-larger app |
| D10 | Style with Tailwind v4 + daisyUI v5; theming via daisyUI `data-theme` (light/dark) | hand-rolled `tokens.css` palette + per-component CSS (slow, drifts) |
| D11 | Lit components render in light DOM so global Tailwind/daisyUI classes apply | shadow DOM + a shared adopted stylesheet (more plumbing for encapsulation we do not need) |
| D12 | Charts via uPlot (~12 KB gz), colors read from daisyUI theme vars | hand-rolled SVG (no axes/tooltips); ApexCharts (daisyUI's own pack, but ~120 KB, over budget); Chart.js (~60 KB, over the route-chunk budget) |
| D13 | Interactive tables via TanStack Table (`@tanstack/table-core`, headless) styled with the daisyUI `.table` | daisyUI `.table` alone (CSS only, no sort/filter/paginate); a React-only table lib (we are on Lit) |
