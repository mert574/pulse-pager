import { html } from "lit";
import { customElement, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { client } from "../api/client.js";
import { session } from "../state/session.js";
import { navigate, currentRelativePath } from "../router.js";
import { appContext, lastOrgHint, type AppContext } from "../state/context.js";
import { can } from "../state/can.js";
import { t } from "../i18n.js";
import { icon, type IconName } from "../icons.js";
import type { OrgMembership } from "../api/types.js";

import "./org-switcher.js";
import "./theme-toggle.js";

// Left sidebar navigation (daisyUI menu). It mounts the org switcher and the
// org-scoped SaaS nav (RFC-013 section 4, 7). Links are built under the active
// org's /orgs/:orgId path so they switch with the org; role-gated sections
// (api-keys, billing, settings) are shown per the can() UI mirror. The router
// intercepts the link clicks, so plain anchors are fine.
@customElement("app-nav")
export class AppNav extends AppElement {
  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private currentPath = currentRelativePath();

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener("popstate", this.onNav);
    document.addEventListener("click", this.onNav);
  }

  override disconnectedCallback(): void {
    window.removeEventListener("popstate", this.onNav);
    document.removeEventListener("click", this.onNav);
    super.disconnectedCallback();
  }

  private onNav = (): void => {
    // run after the router's own handler updates the URL
    queueMicrotask(() => {
      this.currentPath = currentRelativePath();
    });
  };

  private async onLogout(): Promise<void> {
    try {
      await client.logout();
    } finally {
      session.clear();
      navigate("/login");
    }
  }

  // A section link is active when the path equals it or sits under it. The section
  // hrefs do not prefix each other, so this is unambiguous for them.
  private isActive(href: string): boolean {
    return this.currentPath === href || this.currentPath.startsWith(href + "/");
  }

  // The Monitors link is the org home (/orgs/:orgId), which is a prefix of EVERY
  // org route, so the generic prefix test would mark it active everywhere. It is
  // active only on the home itself or a /monitors sub-path.
  private isMonitorsActive(base: string): boolean {
    return (
      this.currentPath === base ||
      this.currentPath.startsWith(`${base}/monitors`)
    );
  }

  private link(href: string, label: string, active: boolean, name: IconName) {
    return html`<li>
      <a href=${href} class=${active ? "menu-active" : ""}>
        ${icon(name, "size-4 opacity-70")}<span>${label}</span>
      </a>
    </li>`;
  }

  // The org the sidebar nav is built for. On an org route it is the active org;
  // on a non-org route (/account, /orgs/new) it falls back to the last-used org
  // (or the first), so the nav and the brand link stay populated instead of the
  // sidebar going blank.
  private navOrg(): OrgMembership | null {
    const me = this.ctx?.me;
    if (!me || me.orgs.length === 0) return null;
    if (this.ctx.activeOrg) return this.ctx.activeOrg;
    const hint = lastOrgHint();
    return (hint && me.orgs.find((o) => o.org_id === hint)) || me.orgs[0];
  }

  override render() {
    const org = this.navOrg();
    const base = org ? `/orgs/${org.org_id}` : "";
    const role = org?.role ?? null;

    return html`
      <aside
        class="flex flex-col w-64 min-h-full bg-base-200 border-r border-base-300"
      >
        <a
          href=${base || "/"}
          class="flex items-center gap-2 px-4 py-4 text-lg font-bold tracking-tight text-primary hover:no-underline"
        >
          <img src="logo.svg" alt="" class="size-6 logo-on-light" />
          <img src="logo-dark.svg" alt="" class="size-6 logo-on-dark" />
          <span class="brand-name">Pulse Pager</span>
        </a>
        ${org
          ? html`
              <ul class="menu menu-lg w-full gap-0.5 flex-1">
                ${this.link(base, t("nav.monitors"), this.isMonitorsActive(base), "activity")}
                ${this.link(
                  `${base}/channels`,
                  t("nav.channels"),
                  this.isActive(`${base}/channels`),
                  "bell",
                )}
                ${this.link(
                  `${base}/incidents`,
                  t("nav.incidents"),
                  this.isActive(`${base}/incidents`),
                  "incident",
                )}
                ${this.link(
                  `${base}/status-pages`,
                  t("nav.statusPages"),
                  this.isActive(`${base}/status-pages`),
                  "globe",
                )}
                ${this.link(
                  `${base}/members`,
                  t("nav.members"),
                  this.isActive(`${base}/members`),
                  "users",
                )}
                ${can(role, "apikey.manage")
                  ? this.link(
                      `${base}/api-keys`,
                      t("nav.apiKeys"),
                      this.isActive(`${base}/api-keys`),
                      "key",
                    )
                  : ""}
                ${can(role, "billing.view")
                  ? this.link(
                      `${base}/billing`,
                      t("nav.billing"),
                      this.isActive(`${base}/billing`),
                      "billing",
                    )
                  : ""}
                ${can(role, "org.settings")
                  ? this.link(
                      `${base}/settings`,
                      t("nav.settings"),
                      this.isActive(`${base}/settings`),
                      "settings",
                    )
                  : ""}
              </ul>
            `
          : html`<div class="flex-1"></div>`}
        <div class="border-t border-base-300 p-3 flex flex-col gap-2">
          <org-switcher></org-switcher>
          <ul class="menu menu-lg w-full p-0">
            ${this.link("/account", t("nav.account"), this.isActive("/account"), "account")}
          </ul>
          <div class="flex items-center gap-2">
            <button
              class="btn btn-sm btn-ghost justify-start flex-1 gap-2"
              @click=${this.onLogout}
            >
              ${icon("logout", "size-4 opacity-70")}${t("nav.logout")}
            </button>
            <theme-toggle></theme-toggle>
          </div>
        </div>
      </aside>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "app-nav": AppNav;
  }
}
