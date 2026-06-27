import { html, type TemplateResult } from "lit";
import { customElement, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { client } from "../api/client.js";
import { session } from "../state/session.js";
import { navigate, currentRelativePath } from "../router.js";
import { appContext, lastOrgHint, type AppContext } from "../state/context.js";
import { can } from "../state/can.js";
import { t } from "../i18n.js";
import { icon } from "../icons.js";
import type { OrgMembership } from "../api/types.js";

import "./org-switcher.js";
import "./theme-toggle.js";

// Left sidebar navigation (RFC-013 section 4, 7), Swiss design. Two indexed groups
// (Monitoring, Organization) under the org switcher, a fixed user footer, and a
// vertical dateline label. Links are built under the active org's /orgs/:orgId path
// so they switch with the org; role-gated sections (api-keys, billing, settings) are
// shown per the can() UI mirror. The router intercepts the link clicks, so plain
// anchors are fine. The index numbers are positional over the visible items.
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

  // One nav row: label left, a positional index chip right; the chip fills with the
  // brand color on the active row.
  private link(href: string, label: string, active: boolean, idx: number) {
    const n = String(idx).padStart(2, "0");
    return html`<a
      href=${href}
      aria-current=${active ? "page" : "false"}
      class="flex items-center justify-between py-2 text-[13.5px] font-medium border-b border-hair ${active
        ? "text-ink font-bold"
        : "text-ink2 hover:text-ink"}"
    >
      <span>${label}</span>
      <span
        class="font-mono text-[10.5px] px-[5px] py-px ${active
          ? "bg-brand text-cream"
          : "text-ink3"}"
        >${n}</span
      >
    </a>`;
  }

  // The org the sidebar nav is built for. On an org route it is the active org;
  // on a non-org route (/account, /orgs/new) it falls back to the last-used org
  // (or the first), so the nav and the brand link stay populated.
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
    const email = this.ctx?.me?.email ?? "";
    const initial = (email.trim()[0] ?? "?").toUpperCase();

    // Build the two groups as flat lists first so the index chips number the
    // actually-visible rows (gated rows do not leave gaps).
    let n = 0;
    const monitoring: TemplateResult[] = org
      ? [
          this.link(base, t("nav.monitors"), this.isMonitorsActive(base), ++n),
          this.link(
            `${base}/incidents`,
            t("nav.incidents"),
            this.isActive(`${base}/incidents`),
            ++n,
          ),
          this.link(
            `${base}/channels`,
            t("nav.channels"),
            this.isActive(`${base}/channels`),
            ++n,
          ),
          this.link(
            `${base}/status-pages`,
            t("nav.statusPages"),
            this.isActive(`${base}/status-pages`),
            ++n,
          ),
        ]
      : [];
    const organization: TemplateResult[] = org
      ? [
          this.link(
            `${base}/members`,
            t("nav.members"),
            this.isActive(`${base}/members`),
            ++n,
          ),
          ...(can(role, "apikey.manage")
            ? [
                this.link(
                  `${base}/api-keys`,
                  t("nav.apiKeys"),
                  this.isActive(`${base}/api-keys`),
                  ++n,
                ),
              ]
            : []),
          ...(can(role, "billing.view")
            ? [
                this.link(
                  `${base}/billing`,
                  t("nav.billing"),
                  this.isActive(`${base}/billing`),
                  ++n,
                ),
              ]
            : []),
          ...(can(role, "org.settings")
            ? [
                this.link(
                  `${base}/settings`,
                  t("nav.settings"),
                  this.isActive(`${base}/settings`),
                  ++n,
                ),
              ]
            : []),
        ]
      : [];

    return html`
      <aside
        class="relative flex flex-col w-[232px] min-h-full overflow-hidden bg-bg border-r border-line px-[22px] pt-6"
      >
        <a
          href=${base || "/"}
          class="flex items-center gap-2.5 mb-8 font-disp font-black text-[18px] uppercase tracking-[-0.04em] text-ink hover:no-underline"
        >
          <img src="logo.svg" alt="" class="size-5 logo-on-light" aria-hidden="true" />
          <img
            src="logo-dark.svg"
            alt=""
            class="size-5 logo-on-dark"
            aria-hidden="true"
          />
          <span>Pulse Pager</span>
        </a>

        ${org
          ? html`
              <div
                class="text-[10px] uppercase tracking-[0.18em] text-ink3 font-semibold mb-[7px]"
              >
                ${t("nav.sectionOrg")}
              </div>
              <div class="mb-7 pb-[18px] border-b border-line">
                <org-switcher></org-switcher>
              </div>

              <div
                class="text-[10px] uppercase tracking-[0.18em] text-ink3 font-bold mb-[9px]"
              >
                ${t("nav.sectionMonitoring")}
              </div>
              <nav class="flex flex-col mb-6">${monitoring}</nav>

              <div
                class="text-[10px] uppercase tracking-[0.18em] text-ink3 font-bold mb-[9px]"
              >
                ${t("nav.sectionOrg")}
              </div>
              <nav class="flex flex-col mb-6">${organization}</nav>
            `
          : html`<div class="flex-1"></div>`}

        <div class="flex-1"></div>

        <a
          href="/account"
          aria-current=${this.isActive("/account") ? "page" : "false"}
          class="flex items-center justify-between py-2 text-[13.5px] font-medium border-b border-hair ${this.isActive(
            "/account",
          )
            ? "text-ink font-bold"
            : "text-ink2 hover:text-ink"}"
        >
          <span>${t("nav.account")}</span>
        </a>

        <div
          class="flex items-center gap-[9px] h-[58px] -mx-[22px] px-[22px] border-t border-line text-[12px] text-ink2"
        >
          <span
            class="grid place-items-center size-6 bg-brand text-cream font-disp font-bold text-[11px]"
            aria-hidden="true"
            >${initial}</span
          >
          <span class="truncate flex-1">${email}</span>
          <theme-toggle></theme-toggle>
          <button
            class="text-ink3 hover:text-ink"
            title=${t("nav.logout")}
            aria-label=${t("nav.logout")}
            @click=${this.onLogout}
          >
            ${icon("logout", "size-4")}
          </button>
        </div>

        <div class="pulse-vlabel">Pulse Pager · Uptime</div>
      </aside>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "app-nav": AppNav;
  }
}
