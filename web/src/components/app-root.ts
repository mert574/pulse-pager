import { html } from "lit";
import { customElement, property, state } from "lit/decorators.js";
import { provide } from "@lit/context";
import { AppElement } from "./base.js";
import type { Router } from "../router.js";
import { client, ApiError } from "../api/client.js";
import { session } from "../state/session.js";
import { navigate, currentRelativePath } from "../router.js";
import { t } from "../i18n.js";
import { icon } from "../icons.js";
import {
  appContext,
  activeOrgIdFromPath,
  lastOrgHint,
  rememberLastOrg,
  type AppContext,
} from "../state/context.js";

import "./app-nav.js";
import "./login-view.js";
import "./confirm-dialog.js";
import "./toast-host.js";

// App shell and the single provider of the app context (RFC-013 section 6). On
// connect it bootstraps the session via GET /api/v1/me. While that runs it shows a
// loading state so the login screen does not flash. When logged in it renders the
// nav plus the router outlet; when logged out it shows the login view.
//
// It owns the derived context value: identity from /me, the active org from the
// route :orgId, the role, and the active org's entitlements. It recomputes on
// navigation and on session change, and re-fetches entitlements when the active
// org changes.
@customElement("app-root")
export class AppRoot extends AppElement {
  // set by main.ts; drives the outlet
  @property({ attribute: false }) router!: Router;

  @state() private route = 0; // bumped to force a re-render on navigation

  @provide({ context: appContext })
  @state()
  private appCtx: AppContext = this.emptyContext();

  private activeOrgId: string | null = null;
  private unsubscribe?: () => void;

  private emptyContext(): AppContext {
    return {
      me: null,
      activeOrg: null,
      role: null,
      entitlements: null,
      refreshMe: () => this.bootstrap(),
    };
  }

  override connectedCallback(): void {
    super.connectedCallback();
    this.unsubscribe = session.subscribe(() => {
      this.syncContext();
      this.requestUpdate();
    });
    void this.bootstrap();
  }

  override disconnectedCallback(): void {
    this.unsubscribe?.();
    super.disconnectedCallback();
  }

  // main.ts calls this after setting .router so the outlet re-renders on every
  // navigation and the context tracks the active org from the new path.
  startRouter(): void {
    this.router.start(() => {
      this.route++;
      this.syncContext();
      this.closeDrawer();
    });
  }

  // Close the mobile drawer after a navigation (no effect on lg, where the drawer
  // is always open). The toggle lives in this element's light DOM.
  private closeDrawer(): void {
    const toggle = this.querySelector<HTMLInputElement>("#app-drawer");
    if (toggle) toggle.checked = false;
  }

  private async bootstrap(): Promise<void> {
    try {
      const me = await client.me();
      session.setMe(me); // triggers syncContext via the subscription
      // if we landed on /login (or the bare root) while authed, send to a home org
      const rel = currentRelativePath();
      if (rel === "/login" || rel === "/") {
        navigate(this.homePath());
      }
    } catch (err) {
      // 401 is the expected logged-out case; the client already cleared the
      // session and routed to /login.
      //
      // Known limitation (deliberate for v1): any OTHER /me failure (5xx, network
      // blip) is also treated as logged-out and drops the user to the login
      // screen, rather than showing a "couldn't load, retry" state. Acceptable
      // for now; revisit with a dedicated bootstrap-error/retry view so a
      // transient server error does not look like a logout.
      if (!(err instanceof ApiError && err.status === 401)) {
        session.clear();
      }
      if (currentRelativePath() !== "/login") {
        navigate("/login");
      }
    }
  }

  // Where an authed user with no explicit destination lands: the last org used if
  // still a member, else the first org, else account (no org yet).
  private homePath(): string {
    const me = session.me;
    if (!me || me.orgs.length === 0) return "/account";
    const hint = lastOrgHint();
    const org =
      (hint && me.orgs.find((o) => o.org_id === hint)) || me.orgs[0];
    return `/orgs/${org.org_id}`;
  }

  // Rebuild the context from the current session and route. Preserves the cached
  // entitlements only while the active org is unchanged; a changed org clears them
  // and kicks off a fresh fetch.
  private syncContext(): void {
    const me = session.me;
    const orgId = me ? activeOrgIdFromPath(currentRelativePath()) : null;
    const activeOrg =
      orgId && me ? me.orgs.find((o) => o.org_id === orgId) ?? null : null;
    const role = activeOrg?.role ?? null;
    const entitlements =
      orgId === this.activeOrgId ? this.appCtx.entitlements : null;

    this.appCtx = {
      me,
      activeOrg,
      role,
      entitlements,
      refreshMe: () => this.bootstrap(),
    };

    if (orgId !== this.activeOrgId) {
      this.activeOrgId = orgId;
      if (activeOrg) {
        rememberLastOrg(activeOrg.org_id);
        void this.loadEntitlements(activeOrg.org_id);
      }
    }
  }

  private async loadEntitlements(orgId: string): Promise<void> {
    try {
      const ent = await client.entitlements(orgId);
      // guard against a stale response after a fast org switch
      if (this.activeOrgId === orgId) {
        this.appCtx = { ...this.appCtx, entitlements: ent };
      }
    } catch {
      // non-fatal: the UI still works, falling back to server-authoritative
      // entitlement errors if a write slips past the at-cap mirror
    }
  }

  override render() {
    if (!session.checked) {
      return html`<div
        class="min-h-screen flex items-center justify-center gap-2 text-base-content/60"
      >
        <span class="loading loading-spinner loading-sm"></span>
        ${t("state.loading")}
      </div>`;
    }

    // The invitation accept page is reachable pre-login: it loads the token
    // preview without a session and routes through login itself when the user
    // chooses to accept. So a logged-out visitor on that path gets the outlet
    // (which renders the accept view), not the login screen.
    if (!session.isLoggedIn) {
      if (currentRelativePath().startsWith("/invitations/")) {
        return html`${this.router.outlet()}<toast-host></toast-host>`;
      }
      return html`<login-view></login-view>`;
    }

    return html`
      <div class="drawer lg:drawer-open min-h-screen">
        <input id="app-drawer" type="checkbox" class="drawer-toggle" />
        <div class="drawer-content flex flex-col min-h-screen">
          <header
            class="navbar lg:hidden min-h-0 gap-1 border-b border-base-300 bg-base-100 px-2 py-1"
          >
            <label
              for="app-drawer"
              class="btn btn-ghost btn-square btn-sm"
              aria-label="Open navigation"
            >
              ${icon("menu", "size-5")}
            </label>
            <a
              href=${this.homePath()}
              class="px-1 text-lg font-bold hover:no-underline brand-name"
              >Pulse Pager</a
            >
          </header>
          <main class="flex-1 min-w-0 w-full max-w-6xl mx-auto p-6 lg:p-8">
            ${this.router.outlet()}
          </main>
          <footer class="text-center text-xs text-base-content/60 p-4">
            (c) 2026 Pulse Pager. Know before your customers do.
          </footer>
        </div>
        <div class="drawer-side z-20">
          <label
            for="app-drawer"
            class="drawer-overlay"
            aria-label="Close navigation"
          ></label>
          <app-nav></app-nav>
        </div>
      </div>
      <toast-host></toast-host>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "app-root": AppRoot;
  }
}
