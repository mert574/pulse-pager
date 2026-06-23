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
      <div class="min-h-screen bg-base-100">
        <header class="border-b border-base-300">
          <div
            class="mx-auto flex w-full max-w-5xl items-center justify-between px-6 py-3"
          >
            <div
              class="flex items-center gap-2 whitespace-nowrap text-lg font-bold text-primary"
            >
              <img src="logo.svg" alt="" class="size-6 logo-on-light" />
              <img src="logo-dark.svg" alt="" class="size-6 logo-on-dark" />
              <span>Pulse Admin</span>
            </div>
            <div class="flex items-center gap-2">
              <a href=${this.appUrl} class="btn btn-ghost btn-sm gap-2">
                ${icon("externalLink", "size-4")} ${t("admin.backToApp")}
              </a>
              <theme-toggle></theme-toggle>
            </div>
          </div>
        </header>
        <main class="mx-auto w-full max-w-5xl px-6 py-6">
          <admin-view></admin-view>
        </main>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "admin-shell": AdminShell;
  }
}
