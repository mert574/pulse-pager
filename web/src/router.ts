// Tiny hand-rolled router on the History API. No dependency. It maps the current
// pathname (relative to the base path) to a route, extracts ":id" style params,
// and renders the matched view into an outlet element. It listens for popstate
// and for in-app link clicks, and exposes navigate(path).
//
// Base path support: Pulse may run behind a reverse proxy at a sub-path. The
// base path is read once from the <base href> in index.html (the Go server
// rewrites that href to PULSE_BASE_PATH at serve time). All matching strips the
// base prefix, and navigate() adds it back, so deep links work behind a sub-path.

import type { TemplateResult } from "lit";
import { html } from "lit";
import { until } from "lit/directives/until.js";
import { t } from "./i18n.js";

export interface RouteContext {
  // path params extracted from the matched pattern, e.g. { id: "abc" }
  params: Record<string, string>;
  // raw query string params from location.search
  query: URLSearchParams;
}

// render may return a template directly, or a Promise of one. The async form is
// how route-level code-splitting works (RFC-013 section 2.2): the render does a
// dynamic import() of the feature view's module and returns its template, and the
// outlet shows a loading state until the chunk resolves.
export type RouteRender = (
  ctx: RouteContext,
) => TemplateResult | Promise<TemplateResult>;

// A guard runs before render. It returns a redirect path to send the user
// elsewhere (e.g. "/login" when not authed, the org home when not a member), or
// null to allow the route (RFC-013 section 5.2). It may be async so a guard can
// await a not-yet-resolved session bootstrap.
export type RouteGuard = (
  ctx: RouteContext,
) => string | null | Promise<string | null>;

export interface Route {
  // pattern like "/orgs/:orgId/monitors/:id" matched against the base-relative path
  pattern: string;
  render: RouteRender;
  guard?: RouteGuard;
}

interface CompiledRoute extends Route {
  regex: RegExp;
  paramNames: string[];
}

// Read the base path from <base href>. Returns a path that always starts with
// "/" and has no trailing slash (so "/" base becomes "").
function readBasePath(): string {
  const href = document.querySelector("base")?.getAttribute("href") ?? "/";
  try {
    // resolve against origin so a relative href still gives us a pathname
    const url = new URL(href, window.location.origin);
    let p = url.pathname;
    if (p.endsWith("/")) p = p.slice(0, -1);
    return p;
  } catch {
    return "";
  }
}

const basePath = readBasePath();

// Loading placeholder shown while a guarded or lazily-imported route resolves.
// A generic page skeleton (heading row + rows) rather than a spinner: on a fast
// network the chunk loads almost instantly, so a spinner just flashes for a frame
// before the view mounts its own skeleton. Matching the common list-view shape
// here makes the switch read as one continuous skeleton settling into content.
// Kept dependency-free; the aria-label still announces loading to screen readers.
function loading(): TemplateResult {
  return html`<div
    class="flex flex-col gap-4"
    aria-busy="true"
    aria-label=${t("state.loading")}
  >
    <div class="flex items-center justify-between">
      <div class="skeleton h-8 w-48"></div>
      <div class="skeleton h-8 w-28"></div>
    </div>
    <div class="flex flex-col gap-2">
      ${Array.from(
        { length: 6 },
        () => html`<div class="skeleton h-12 w-full"></div>`,
      )}
    </div>
  </div>`;
}

// Compile "/monitors/:id" into a regex with named param capture.
function compile(route: Route): CompiledRoute {
  const paramNames: string[] = [];
  const source = route.pattern
    .replace(/\/+$/, "")
    .replace(/:[a-zA-Z_][a-zA-Z0-9_]*/g, (m) => {
      paramNames.push(m.slice(1));
      return "([^/]+)";
    });
  const regex = new RegExp(`^${source || "/"}/?$`);
  return { ...route, regex, paramNames };
}

// Strip the base prefix from a full pathname, returning a base-relative path that
// always starts with "/".
function toRelative(pathname: string): string {
  let p = pathname;
  if (basePath && p.startsWith(basePath)) {
    p = p.slice(basePath.length);
  }
  if (!p.startsWith("/")) p = "/" + p;
  return p;
}

// Add the base prefix to a base-relative path for use with history.pushState.
function toAbsolute(path: string): string {
  const rel = path.startsWith("/") ? path : "/" + path;
  return (basePath + rel) || "/";
}

export class Router {
  private routes: CompiledRoute[] = [];
  private fallback?: Route;
  private onChange?: () => void;

  constructor(routes: Route[], fallback?: Route) {
    this.routes = routes.map(compile);
    this.fallback = fallback;
  }

  // start wires popstate + link interception and calls back on every change so
  // the outlet host can re-render.
  start(onChange: () => void): void {
    this.onChange = onChange;
    window.addEventListener("popstate", () => this.onChange?.());
    document.addEventListener("click", this.handleLinkClick);
  }

  // Intercept clicks on in-app links. An anchor opts in with data-link, or any
  // same-origin left click without a modifier or target is handled in-app.
  private handleLinkClick = (e: MouseEvent): void => {
    if (e.defaultPrevented || e.button !== 0) return;
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;

    const path = e.composedPath();
    const anchor = path.find(
      (el): el is HTMLAnchorElement => el instanceof HTMLAnchorElement,
    );
    if (!anchor) return;
    if (anchor.target && anchor.target !== "_self") return;
    if (anchor.hasAttribute("download")) return;

    const href = anchor.getAttribute("href");
    if (!href) return;
    // skip external, hash-only, and protocol links
    if (/^([a-z]+:)?\/\//i.test(href) || href.startsWith("#")) return;

    const url = new URL(anchor.href);
    if (url.origin !== window.location.origin) return;

    e.preventDefault();
    this.go(toRelative(url.pathname) + url.search);
  };

  // Match the current location and render the route, or the fallback.
  outlet(): TemplateResult {
    const rel = toRelative(window.location.pathname);
    const query = new URLSearchParams(window.location.search);

    for (const r of this.routes) {
      const m = r.regex.exec(rel);
      if (!m) continue;
      const params: Record<string, string> = {};
      r.paramNames.forEach((name, i) => {
        params[name] = decodeURIComponent(m[i + 1] ?? "");
      });
      return this.renderRoute(r, { params, query });
    }

    if (this.fallback) {
      return this.renderRoute(this.fallback, { params: {}, query });
    }
    return html`<p>${t("state.notFound")}</p>`;
  }

  // Run the guard (if any) then render. A synchronous guard (the common case:
  // requireAuth, membership checks) is evaluated inline so its route's template
  // is returned directly, with no until() wrapper and no loading frame. only an
  // async guard or an async (lazily-imported) render resolves through until().
  private renderRoute(route: Route, ctx: RouteContext): TemplateResult {
    if (route.guard) {
      const verdict = route.guard(ctx);
      if (verdict instanceof Promise) {
        return html`${until(
          verdict.then((redirect) =>
            redirect ? this.redirectTo(redirect) : this.renderView(route, ctx),
          ),
          loading(),
        )}`;
      }
      if (verdict) return this.redirectTo(verdict);
      // sync guard passed: fall through to render
    }
    return this.renderView(route, ctx);
  }

  // Render the view, resolving a lazily-imported (async) render through until()
  // with a loading state; a sync render is returned directly.
  private renderView(route: Route, ctx: RouteContext): TemplateResult {
    const out = route.render(ctx);
    return out instanceof Promise ? html`${until(out, loading())}` : out;
  }

  // Queue a navigation to run after this render settles (never during it) and
  // render nothing in the meantime, so a redirect does not flash a loading state.
  private redirectTo(path: string): TemplateResult {
    queueMicrotask(() => this.go(path));
    return html``;
  }

  // Push a new base-relative path and re-render.
  go(path: string): void {
    window.history.pushState({}, "", toAbsolute(path));
    this.onChange?.();
  }
}

// Module-level singleton so any component can navigate without prop drilling.
// app-root creates the real instance and registers it here on boot.
let active: Router | null = null;

export function registerRouter(r: Router): void {
  active = r;
}

// navigate(path) pushes a base-relative path. Components and the api client call
// this for programmatic navigation (for example 401 -> /login).
// The current location as a base-relative path (base prefix stripped). Used by
// the app context to derive the active org id from /orgs/:orgId.
export function currentRelativePath(): string {
  return toRelative(window.location.pathname);
}

export function navigate(path: string): void {
  if (active) {
    active.go(path);
  } else {
    // before the router exists (very early boot), fall back to a hard set
    window.history.pushState({}, "", toAbsolute(path));
  }
}
