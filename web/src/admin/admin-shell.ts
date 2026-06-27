import { html } from "lit";
import { customElement } from "lit/decorators.js";
import { AppElement } from "../components/base.js";
import { t } from "../i18n.js";
import { icon } from "../icons.js";
import "../components/theme-toggle.js";
import "../components/admin-view.js";

// Minimal chrome for the operator admin app: a brand header and the admin view.
// No org switcher, no customer nav, no router; this app has one page. Kept separate
// from the customer SPA shell so the customer bundle ships no admin code at all.
@customElement("admin-shell")
export class AdminShell extends AppElement {
  // The customer app lives on its own origin. In prod the admin host is
  // admin.pulsepager.com, so swap the prefix to app.pulsepager.com; in local dev
  // (no admin. prefix) the app is just the site root (index.html).
  private get appUrl(): string {
    const { protocol, hostname, host } = window.location;
    if (hostname.startsWith("admin.")) {
      return `${protocol}//${host.replace(/^admin\./, "app.")}/`;
    }
    return "/";
  }

  override render() {
    return html`
      <div class="min-h-screen bg-bg">
        <!-- Tight editorial frame: the whole operator app sits in one bordered
             broadsheet column with side rules, so the control room reads as a single
             instrument panel rather than a full-width dashboard. -->
        <div
          class="mx-auto w-full max-w-[1480px] min-h-screen border-x border-line flex flex-col"
        >
          <!-- Masthead: a heavy Archivo wordmark over the operator kicker, with the
               back-to-app link and theme toggle on the right. -->
          <header
            class="flex flex-wrap items-end justify-between gap-5 px-6 lg:px-10 pt-7 lg:pt-[30px] pb-6 lg:pb-[26px] border-b border-line"
          >
            <div class="flex items-center gap-3">
              <img src="logo.svg" alt="" class="size-8 logo-on-light" />
              <img src="logo-dark.svg" alt="" class="size-8 logo-on-dark" />
              <div class="flex flex-col gap-1">
                <span class="pulse-label">${t("admin.subtitle")}</span>
                <h1
                  class="m-0 font-disp font-black uppercase tracking-[-0.045em] leading-[0.82] text-[34px] lg:text-[48px]"
                >
                  ${t("admin.title")}
                </h1>
              </div>
            </div>
            <div class="flex items-center gap-2">
              <a href=${this.appUrl} class="pulse-btn pulse-btn-ghost pulse-btn-sm">
                ${icon("externalLink", "size-4")} ${t("admin.backToApp")}
              </a>
              <theme-toggle></theme-toggle>
            </div>
          </header>
          <main class="flex-1">
            <admin-view></admin-view>
          </main>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "admin-shell": AdminShell;
  }
}
