import { html } from "lit";
import { customElement } from "lit/decorators.js";
import { AppElement } from "./base.js";
import { t } from "../i18n.js";

// Social-only login (RFC-003 is passwordless: Google + GitHub). Login is a
// full-page redirect, not an XHR, because OAuth needs the browser to leave to the
// provider and come back (RFC-013 section 3.1). The SPA never sees the code or the
// tokens; on return it discovers it is logged in via GET /api/v1/me.
//
// return_to carries the path the user was trying to reach (e.g. an invite deep
// link) so it survives the round trip; the server allowlists it (RFC-003 2.2).
//
// If the OAuth callback bounced back with ?error=... (the IdP denied, or the
// server rejected the exchange), we show that as a localized message above the
// buttons so the user knows the attempt failed.
@customElement("login-view")
export class LoginView extends AppElement {
  private returnTo(): string {
    const param = new URLSearchParams(window.location.search).get("return_to");
    return param && param.startsWith("/") ? param : "/";
  }

  // The error code the callback may have put on the URL. We only know a couple of
  // codes; anything else falls back to a generic "could not sign in" message.
  private errorCode(): string | null {
    return new URLSearchParams(window.location.search).get("error");
  }

  private errorMessage(code: string): string {
    if (code === "access_denied") return t("login.errorDenied");
    return t("login.errorGeneric");
  }

  // The single place we hand off to a full-page redirect. Pulled out so a test can
  // override it on the instance (window.location.assign is read-only in browsers).
  protected redirect(url: string): void {
    window.location.assign(url);
  }

  private signIn(provider: "google" | "github"): void {
    const returnTo = encodeURIComponent(this.returnTo());
    this.redirect(`/auth/${provider}/login?return_to=${returnTo}`);
  }

  // Dev-only shortcut so local development without configured OAuth still works.
  // The real api exposes POST /auth/dev/login (guarded by PULSE_DEV_LOGIN); it
  // creates or loads a real user plus personal org in Postgres and sets the same
  // session cookies the OAuth callback does. We ask for an email so each developer
  // gets their own real account. This button only renders under Vite's DEV build.
  private async devSignIn(): Promise<void> {
    const email = window.prompt("Dev sign in: enter your email", "");
    if (!email) return;
    const resp = await fetch("/auth/dev/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ email }),
    });
    if (resp.ok) {
      // Full reload so app-root re-bootstraps the session via GET /api/v1/me.
      this.redirect(this.returnTo());
    } else {
      window.alert(`Dev sign in failed (${resp.status})`);
    }
  }

  override render() {
    const err = this.errorCode();
    return html`
      <div class="min-h-screen flex items-center justify-center p-4">
        <div class="card w-full max-w-sm bg-base-100 shadow-md border border-base-300">
          <div class="card-body items-center text-center gap-4">
            <div class="flex items-center gap-2">
              <img src="logo.svg" alt="" class="size-8 logo-on-light" />
              <img src="logo-dark.svg" alt="" class="size-8 logo-on-dark" />
              <h1 class="text-2xl font-bold brand-name">Pulse Pager</h1>
            </div>
            <p class="text-base-content/60 text-sm">${t("login.tagline")}</p>
            ${err
              ? html`<div role="alert" class="alert alert-error w-full text-sm">
                  <span>${this.errorMessage(err)}</span>
                </div>`
              : ""}
            <div class="w-full flex flex-col gap-3">
              <button class="btn btn-block" @click=${() => this.signIn("google")}>
                ${t("login.google")}
              </button>
              <button class="btn btn-block" @click=${() => this.signIn("github")}>
                ${t("login.github")}
              </button>
              ${import.meta.env.DEV
                ? html`<button
                    class="btn btn-ghost btn-sm btn-block"
                    @click=${() => this.devSignIn()}
                  >
                    ${t("login.dev")}
                  </button>`
                : ""}
            </div>
          </div>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "login-view": LoginView;
  }
}
