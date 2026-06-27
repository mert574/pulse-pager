import { html } from "lit";
import { customElement, property, state } from "lit/decorators.js";
import { provide } from "@lit/context";
import { AppElement } from "./base.js";
import type { Router } from "../router.js";
import { client, ApiError } from "../api/client.js";
import { session } from "../state/session.js";
import { navigate, currentRelativePath } from "../router.js";
import { t, type MessageKey } from "../i18n.js";
import { formatDuration } from "../format.js";
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
        class="min-h-screen flex items-center justify-center gap-2 text-ink3"
      >
        <span
          class="inline-block size-4 animate-spin rounded-full border-2 border-current border-t-transparent"
        ></span>
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

    // Owned responsive shell (no daisyUI). The fixed wrapper owns the viewport so
    // the content column is the single scroller and the folio can stick. On lg the
    // sidebar is a static flex column; on mobile it slides in off-canvas, driven by
    // the #app-drawer checkbox via Tailwind peer-checked, with a tap-to-close
    // backdrop. closeDrawer() unchecks it after a navigation.
    return html`
      <div class="fixed inset-0 overflow-hidden flex">
        <input id="app-drawer" type="checkbox" class="peer sr-only" />
        <label
          for="app-drawer"
          aria-label="Close navigation"
          class="fixed inset-0 z-30 bg-black/40 opacity-0 pointer-events-none transition-opacity peer-checked:opacity-100 peer-checked:pointer-events-auto lg:hidden"
        ></label>
        <div
          class="fixed inset-y-0 left-0 z-40 h-full shrink-0 -translate-x-full transition-transform peer-checked:translate-x-0 lg:static lg:z-auto lg:translate-x-0"
        >
          <app-nav></app-nav>
        </div>
        <div class="flex flex-1 flex-col h-full min-h-0 min-w-0">
          <header
            class="lg:hidden shrink-0 flex items-center gap-2 border-b border-line bg-bg px-3 py-2"
          >
            <label
              for="app-drawer"
              class="grid place-items-center size-9 border border-line text-ink"
              aria-label="Open navigation"
            >
              ${icon("menu", "size-5")}
            </label>
            <a
              href=${this.homePath()}
              class="flex items-center gap-2 font-disp font-black uppercase tracking-[-0.04em] text-ink hover:no-underline"
            >
              <img src="logo.svg" alt="" class="size-5 logo-on-light" />
              <img src="logo-dark.svg" alt="" class="size-5 logo-on-dark" />
              Pulse Pager
            </a>
          </header>
          ${this.folio()}
          <div class="flex flex-1 flex-col min-h-0 overflow-y-auto">
            <main class="flex-1 min-w-0 w-full px-6 lg:px-10 py-7">
              ${this.router.outlet()}
            </main>
            ${this.colophon()}
          </div>
        </div>
      </div>
      <toast-host></toast-host>
    `;
  }

  // Sticky marquee dateline across the top of the content column. It carries the
  // account's live vitals straight from the app context, so no extra fetch and no
  // fabricated numbers: the dateline, the active org and its plan, then the
  // entitlement usage (monitors / seats / status pages used vs cap, the fastest
  // allowed check interval, and how long history is kept). Off an org route, or
  // before entitlements load, it falls back to the brand lines. The set is rendered
  // twice so the -50% loop is seamless.
  private folio() {
    const org = this.appCtx.activeOrg;
    const ent = this.appCtx.entitlements;
    const dateline = new Date().toLocaleDateString(undefined, {
      weekday: "short",
      day: "numeric",
      month: "short",
      year: "numeric",
    });
    const facts = ["Pulse Pager", dateline, ...(org ? [org.name] : [])];
    if (ent) {
      facts.push(
        `${t(`plan.${ent.plan}` as MessageKey)} plan`,
        `${ent.monitors_used} / ${ent.monitors_cap} monitors`,
        `${ent.seats_used} / ${ent.seats_cap} seats`,
      );
      if (ent.status_pages_cap > 0) {
        facts.push(`${ent.status_pages_used} / ${ent.status_pages_cap} status pages`);
      }
      facts.push(
        `${formatDuration(ent.min_interval_seconds)} min interval`,
        `${ent.retention_days}-day history`,
      );
    } else {
      facts.push("Uptime monitoring", "Know before your customers do");
    }
    const run = facts.map((f) => html`<b>${f}</b><i></i>`);
    return html`<div class="pulse-folio">
      <div class="pulse-folio-track">${run}${run}</div>
    </div>`;
  }

  // Broadsheet colophon under every page: outlined wordmark, four link columns
  // (mirroring what the docs site actually offers), and the license baseline.
  private colophon() {
    const org = this.appCtx.activeOrg;
    const base = org ? `/orgs/${org.org_id}` : "";
    const col = (
      title: string,
      links: { label: string; href: string }[],
    ) => html`<div class="p-[22px] px-10 border-r border-hair last:border-r-0">
      <h4
        class="font-mono text-[10px] tracking-[0.16em] uppercase text-ink3 font-semibold mb-3"
      >
        ${title}
      </h4>
      ${links.map(
        (l) =>
          html`<a
            href=${l.href}
            class="block text-ink2 hover:text-brand text-[13px] py-1 hover:no-underline"
            >${l.label}</a
          >`,
      )}
    </div>`;

    return html`<footer class="mt-12">
      <div class="pulse-cmark">Pulse Pager</div>
      <div class="grid grid-cols-2 lg:grid-cols-4 border-t border-hair">
        ${col("Product", [
          { label: t("nav.monitors"), href: base || "/" },
          { label: t("nav.incidents"), href: `${base}/incidents` },
          { label: t("nav.statusPages"), href: `${base}/status-pages` },
          { label: t("nav.channels"), href: `${base}/channels` },
        ])}
        ${col("Developers", [
          { label: "API reference", href: "https://pulsepager.com/api.html" },
          {
            label: "Authentication",
            href: "https://pulsepager.com/guides/authentication.html",
          },
        ])}
        ${col("Project", [
          { label: "GitHub", href: "https://github.com/mert574/pulse-pager" },
          { label: "Pricing", href: "https://pulsepager.com/pricing.html" },
        ])}
        ${col("Legal", [
          { label: "Terms", href: "https://pulsepager.com/terms.html" },
          { label: "Privacy", href: "https://pulsepager.com/privacy.html" },
          { label: "Contact", href: "mailto:hi@pulsepager.com" },
        ])}
      </div>
      <div
        class="flex items-center justify-between gap-5 flex-wrap h-[58px] px-10 border-t border-line font-mono text-[11px] text-ink3 uppercase tracking-[0.08em]"
      >
        <span>(c) 2026 Pulse Pager / Elastic License 2.0</span>
        <span class="flex items-center gap-3">
          <a
            href="https://pulsepager.com"
            class="text-ink2 hover:text-brand hover:no-underline"
            >pulsepager.com</a
          >
          <span class="size-[11px] bg-brand"></span>
        </span>
      </div>
    </footer>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "app-root": AppRoot;
  }
}
