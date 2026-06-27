import { html, nothing } from "lit";
import { customElement, state } from "lit/decorators.js";
import { AppElement } from "./base.js";
import { t } from "../i18n.js";
import { icon } from "../icons.js";
import { client, ApiError } from "../api/client.js";

// Where the marketing site serves the legal pages. The login app lives on a
// different origin (app.*), so these are absolute links to the public site.
const TERMS_URL = "https://pulsepager.com/terms.html";
const PRIVACY_URL = "https://pulsepager.com/privacy.html";

// Login (RFC-003 is passwordless: GitHub OAuth plus magic-link email; Google is
// hidden until it is configured). OAuth is a full-page redirect, not an XHR,
// because it needs the browser to leave to the provider and come back (RFC-013
// section 3.1). The SPA never sees the code or the tokens; on return it discovers
// it is logged in via GET /api/v1/me. Every sign-in method is disabled until the
// user ticks the Terms + Privacy consent box, so consent is captured at sign-in.
//
// Magic-link is an XHR to POST /auth/email/start: the server emails a one-time link
// and we show a neutral "check your email" confirmation. The server is
// enumeration-safe, so we never tell the user whether the email had an account; a
// rate-limit reads the same as success here so the limit does not leak.
//
// return_to carries the path the user was trying to reach (e.g. an invite deep
// link) so it survives the round trip; the server allowlists it (RFC-003 2.2).
//
// If the OAuth callback bounced back with ?error=... (the IdP denied, or the
// server rejected the exchange), we show that as a localized message above the
// buttons so the user knows the attempt failed.
@customElement("login-view")
export class LoginView extends AppElement {
  // Passwordless email login state: the typed email, an in-flight flag for the
  // button, and whether we've shown the neutral "check your email" confirmation.
  @state() private email = "";
  @state() private emailSending = false;
  @state() private emailSent = false;
  // The user must accept the Terms + Privacy before any sign-in method is enabled,
  // so consent is recorded at the moment of sign-up/sign-in (protects us legally).
  @state() private agreed = false;

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

  private signInGithub(): void {
    if (!this.agreed) return;
    const returnTo = encodeURIComponent(this.returnTo());
    this.redirect(`/auth/github/login?return_to=${returnTo}`);
  }

  // Ask the server to email a one-time sign-in link. The server is
  // enumeration-safe, so we always end on the same neutral confirmation and never
  // tell the user whether the email had an account. A rate-limit (429) reads the
  // same as success on purpose, so it does not leak the limit; only a clear input
  // error (422 bad email) is surfaced as the generic error.
  private async emailLogin(e: Event): Promise<void> {
    e.preventDefault();
    const email = this.email.trim();
    if (!email || this.emailSending || !this.agreed) return;
    this.emailSending = true;
    try {
      await client.startEmailLogin(email);
      this.emailSent = true;
    } catch (err) {
      // A 429 is neutral too (do not reveal the limit). Anything else that is not a
      // plain validation error also lands on the neutral confirmation, so the page
      // never leaks whether the address exists.
      if (err instanceof ApiError && err.status === 422) {
        // bad email shape: keep the form open so the user can correct it.
        this.emailSent = false;
      } else {
        this.emailSent = true;
      }
    } finally {
      this.emailSending = false;
    }
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

  // The agreement label, with Terms and Privacy as links. Built from i18n parts so
  // each locale orders the sentence correctly (German puts "zu" at the end).
  private agreeLabel() {
    const link = (href: string, label: string) => html`<a
      href=${href}
      target="_blank"
      rel="noopener"
      class="text-brand"
      @click=${(e: Event) => e.stopPropagation()}
      >${label}</a
    >`;
    return html`${t("login.agreePrefix")}
      ${link(TERMS_URL, t("login.agreeTerms"))} ${t("login.agreeMid")}
      ${link(PRIVACY_URL, t("login.agreePrivacy"))}${t("login.agreeSuffix")}`;
  }

  override render() {
    const err = this.errorCode();
    return html`
      <div class="min-h-screen grid place-items-center p-6 bg-bg">
        <div class="w-full max-w-sm flex flex-col gap-7">
          <!-- Editorial wordmark: a heavy two-line Archivo mark over a mono tagline. -->
          <div class="flex flex-col gap-3">
            <div class="flex items-center gap-3">
              <img src="logo.svg" alt="" class="size-11 logo-on-light" />
              <img src="logo-dark.svg" alt="" class="size-11 logo-on-dark" />
              <h1
                class="m-0 font-disp font-black uppercase tracking-[-0.045em] leading-[0.78] text-[40px] brand-name"
              >
                Pulse<br />Pager
              </h1>
            </div>
            <p
              class="font-mono text-[10.5px] tracking-[0.16em] uppercase text-ink3"
            >
              ${t("login.tagline")}
            </p>
          </div>

          <div class="border border-hair bg-bg p-7 flex flex-col gap-5">
            ${err
              ? html`<div
                  role="alert"
                  class="border border-down px-4 py-3 w-full text-sm text-down"
                >
                  <span>${this.errorMessage(err)}</span>
                </div>`
              : nothing}

            <!-- Until this is ticked every sign-in method below stays disabled. -->
            <label
              class="flex items-start gap-2.5 text-xs text-ink2 cursor-pointer"
            >
              <input
                type="checkbox"
                class="size-4 accent-brand shrink-0 mt-0.5"
                data-agree
                .checked=${this.agreed}
                @change=${(e: Event) =>
                  (this.agreed = (e.target as HTMLInputElement).checked)}
              />
              <span>${this.agreeLabel()}</span>
            </label>

            <div class="flex flex-col gap-3">
              ${this.emailSent
                ? html`<div
                    role="status"
                    class="border border-hair bg-paper px-4 py-3 w-full text-sm text-ink2"
                  >
                    <span>${t("login.emailSent")}</span>
                  </div>`
                : html`<form
                    class="w-full flex flex-col gap-2.5"
                    @submit=${(e: Event) => this.emailLogin(e)}
                  >
                    <label class="pulse-label" for="login-email"
                      >${t("login.emailLabel")}</label
                    >
                    <input
                      id="login-email"
                      type="email"
                      required
                      class="pulse-input w-full"
                      aria-label=${t("login.emailLabel")}
                      placeholder=${t("login.emailPlaceholder")}
                      .value=${this.email}
                      @input=${(e: Event) =>
                        (this.email = (e.target as HTMLInputElement).value)}
                    />
                    <button
                      type="submit"
                      class="pulse-btn w-full"
                      ?disabled=${this.emailSending || !this.agreed}
                    >
                      ${icon("mail", "size-4")}${this.emailSending
                        ? t("login.emailSending")
                        : t("login.emailSubmit")}
                    </button>
                  </form>`}

              <div
                class="flex items-center gap-3 font-mono text-[10px] uppercase tracking-[0.14em] text-ink3"
              >
                <span class="flex-1 border-t border-hair"></span>
                ${t("login.emailOr")}
                <span class="flex-1 border-t border-hair"></span>
              </div>

              <button
                class="pulse-btn pulse-btn-ghost w-full"
                data-provider="github"
                ?disabled=${!this.agreed}
                @click=${() => this.signInGithub()}
              >
                ${icon("github", "size-5")} ${t("login.github")}
              </button>

              ${import.meta.env.DEV
                ? html`<button
                    class="pulse-btn pulse-btn-ghost pulse-btn-sm w-full"
                    @click=${() => this.devSignIn()}
                  >
                    ${t("login.dev")}
                  </button>`
                : nothing}
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
