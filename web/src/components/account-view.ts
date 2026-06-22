import { html } from "lit";
import { customElement, state } from "lit/decorators.js";
import { AppElement } from "./base.js";
import { client, ApiError } from "../api/client.js";
import { navigate } from "../router.js";
import { session } from "../state/session.js";
import {
  availableLocales,
  currentLocale,
  setLocale,
  t,
  type Locale,
} from "../i18n.js";
import { toast } from "../toast.js";
import { icon } from "../icons.js";
import type { Identity, IdentityProviderName, MeUpdate } from "../api/types.js";

import "./form-field.js";

// The list of timezones offered. Intl.supportedValuesOf gives the full IANA list
// in modern browsers; we fall back to a small common set when it is missing so the
// select is never empty.
function timezones(): string[] {
  const intl = Intl as unknown as {
    supportedValuesOf?: (key: string) => string[];
  };
  if (typeof intl.supportedValuesOf === "function") {
    try {
      return intl.supportedValuesOf("timeZone");
    } catch {
      // fall through to the small fallback list
    }
  }
  return [
    "UTC",
    "America/New_York",
    "America/Los_Angeles",
    "Europe/London",
    "Europe/Berlin",
    "Europe/Madrid",
    "Asia/Tokyo",
    "Australia/Sydney",
  ];
}

const PROVIDERS: IdentityProviderName[] = ["google", "github"];

// Account settings (RFC-013 section 9.3). Three blocks: profile (name + locale +
// timezone, saved via PATCH /me), linked providers (GET /me/identities, connect
// via the /auth/{provider}/link redirect, unlink via DELETE with a guard against
// removing the last one), and session actions (log out, log out of all devices).
//
// Profile reads its initial values from the session /me. On a locale change we
// also flip the UI locale immediately via i18n so the page reflects the choice
// before the save round-trips.
@customElement("account-view")
export class AccountView extends AppElement {
  @state() private name = "";
  @state() private locale = "";
  @state() private timezone = "";
  @state() private saving = false;
  @state() private saveError: ApiError | null = null;
  @state() private initialized = false;

  @state() private identities: Identity[] | null = null;
  @state() private identitiesError = false;
  @state() private unlinking: IdentityProviderName | null = null;

  override connectedCallback(): void {
    super.connectedCallback();
    this.seedFromSession();
    void this.loadIdentities();
  }

  // Seed the editable fields once from /me. We only do this on first paint so we
  // do not stomp the user's edits when the context updates for an unrelated reason.
  private seedFromSession(): void {
    const me = session.me;
    if (!me || this.initialized) return;
    this.name = me.name;
    this.locale = me.locale;
    this.timezone = me.timezone;
    this.initialized = true;
  }

  override updated(): void {
    this.seedFromSession();
  }

  private async loadIdentities(): Promise<void> {
    this.identitiesError = false;
    try {
      this.identities = await client.listIdentities();
    } catch {
      this.identitiesError = true;
      this.identities = null;
    }
  }

  // --- profile ---

  private onNameInput = (e: Event): void => {
    this.name = (e.target as HTMLInputElement).value;
  };

  private onLocaleChange = (e: Event): void => {
    const value = (e.target as HTMLSelectElement).value as Locale;
    this.locale = value;
    // reflect the choice in the UI right away; the save persists it server-side
    setLocale(value);
  };

  private onTimezoneChange = (e: Event): void => {
    this.timezone = (e.target as HTMLSelectElement).value;
  };

  private async saveProfile(e: Event): Promise<void> {
    e.preventDefault();
    if (this.saving) return;
    this.saving = true;
    this.saveError = null;
    try {
      const input: MeUpdate = {
        name: this.name.trim(),
        locale: this.locale,
        timezone: this.timezone,
      };
      const me = await client.updateMe(input);
      session.setMe(me);
      toast(t("account.saved"), "success");
    } catch (err) {
      this.saveError = err instanceof ApiError ? err : null;
      if (!this.saveError) toast(t("state.error"), "error");
    } finally {
      this.saving = false;
    }
  }

  // --- identities ---

  private connect(provider: IdentityProviderName): void {
    // linking is a full-page OAuth redirect, same as login; on return the app
    // re-bootstraps and the identity shows up in the list
    window.location.assign(`/auth/${provider}/link`);
  }

  private async unlink(provider: IdentityProviderName): Promise<void> {
    if (this.unlinking) return;
    // client-side guard: never let the user remove their only sign-in method.
    // The server enforces this too (409), this just stops the request early.
    if ((this.identities?.length ?? 0) <= 1) {
      toast(t("account.unlinkLast"), "error");
      return;
    }
    this.unlinking = provider;
    try {
      await client.unlinkIdentity(provider);
      toast(t("account.unlinked"), "success");
      await this.loadIdentities();
    } catch (err) {
      const msg =
        err instanceof ApiError && err.status === 409
          ? t("account.unlinkLast")
          : err instanceof ApiError
            ? err.message
            : t("state.error");
      toast(msg, "error");
    } finally {
      this.unlinking = null;
    }
  }

  // --- session actions ---

  private async logout(): Promise<void> {
    try {
      await client.logout();
    } finally {
      session.clear();
      navigate("/login");
    }
  }

  private async logoutAll(): Promise<void> {
    try {
      await client.logoutAll();
    } finally {
      session.clear();
      navigate("/login");
    }
  }

  // --- render ---

  private providerLabel(p: IdentityProviderName): string {
    return p === "google" ? t("account.google") : t("account.github");
  }

  override render() {
    return html`
      <div class="flex flex-col gap-8 max-w-2xl">
        <h1 class="text-2xl font-bold">${t("account.heading")}</h1>
        ${this.profileSection()} ${this.identitiesSection()}
        ${this.sessionsSection()}
      </div>
    `;
  }

  private profileSection() {
    return html`
      <section class="flex flex-col gap-4">
        <h2 class="text-lg font-semibold">${t("account.profile")}</h2>
        ${this.saveError && !this.saveError.fields
          ? html`<div role="alert" class="alert alert-error">
              <span>${this.saveError.message}</span>
            </div>`
          : ""}
        <form class="flex flex-col gap-4" @submit=${this.saveProfile}>
          <form-field
            label=${t("account.name")}
            fieldName="name"
            .error=${this.saveError?.fields?.name ?? null}
            .control=${html`<input
              id="name"
              class="input w-full"
              .value=${this.name}
              @input=${this.onNameInput}
              autocomplete="name"
            />`}
          ></form-field>

          <form-field
            label=${t("account.email")}
            fieldName="email"
            hint=${t("account.emailHint")}
            .control=${html`<input
              id="email"
              class="input w-full"
              .value=${session.me?.email ?? ""}
              disabled
            />`}
          ></form-field>

          <form-field
            label=${t("account.language")}
            fieldName="locale"
            .error=${this.saveError?.fields?.locale ?? null}
            .control=${html`<select
              id="locale"
              class="select w-full"
              .value=${this.locale}
              @change=${this.onLocaleChange}
            >
              ${availableLocales.map(
                (l) =>
                  html`<option
                    value=${l.code}
                    ?selected=${l.code === (this.locale || currentLocale())}
                  >
                    ${l.name}
                  </option>`,
              )}
            </select>`}
          ></form-field>

          <form-field
            label=${t("account.timezone")}
            fieldName="timezone"
            .error=${this.saveError?.fields?.timezone ?? null}
            .control=${html`<select
              id="timezone"
              class="select w-full"
              .value=${this.timezone}
              @change=${this.onTimezoneChange}
            >
              ${timezones().map(
                (tz) =>
                  html`<option value=${tz} ?selected=${tz === this.timezone}>
                    ${tz}
                  </option>`,
              )}
            </select>`}
          ></form-field>

          <div>
            <button
              type="submit"
              class="btn btn-primary"
              ?disabled=${this.saving}
            >
              ${this.saving ? t("account.saving") : t("account.save")}
            </button>
          </div>
        </form>
      </section>
    `;
  }

  private identitiesSection() {
    return html`
      <section class="flex flex-col gap-4">
        <h2 class="text-lg font-semibold">${t("account.providers")}</h2>
        <p class="text-base-content/60 text-sm">
          ${t("account.providersHint")}
        </p>
        ${this.identitiesError
          ? html`<div role="alert" class="alert alert-error">
              <span>${t("state.error")}</span>
              <button class="btn btn-sm" @click=${() => this.loadIdentities()}>
                ${t("state.retry")}
              </button>
            </div>`
          : this.identities === null
            ? html`<div class="skeleton h-12 w-full"></div>`
            : this.providerRows()}
      </section>
    `;
  }

  private providerRows() {
    const linked = this.identities ?? [];
    const canUnlink = linked.length > 1;
    return html`<ul class="flex flex-col divide-y divide-base-300 rounded-box border border-base-300">
      ${PROVIDERS.map((p) => {
        const id = linked.find((i) => i.provider === p);
        return html`<li class="flex items-center justify-between gap-3 p-3">
          <span class="font-medium">${this.providerLabel(p)}</span>
          ${id
            ? html`<div class="flex items-center gap-2">
                <span class="badge badge-success badge-sm gap-1"
                  >${icon("check", "size-3")}${t("account.connected")}</span
                >
                <button
                  class="btn btn-sm btn-ghost"
                  ?disabled=${!canUnlink || this.unlinking === p}
                  title=${!canUnlink ? t("account.unlinkLast") : ""}
                  @click=${() => this.unlink(p)}
                >
                  ${t("account.unlink")}
                </button>
              </div>`
            : html`<button
                class="btn btn-sm"
                @click=${() => this.connect(p)}
              >
                ${t("account.connect")}
              </button>`}
        </li>`;
      })}
    </ul>`;
  }

  private sessionsSection() {
    return html`
      <section class="flex flex-col gap-4">
        <h2 class="text-lg font-semibold">${t("account.sessions")}</h2>
        <div class="flex flex-wrap gap-2">
          <button class="btn gap-2" @click=${this.logout}>
            ${icon("logout", "size-4 opacity-70")}${t("account.logout")}
          </button>
          <button class="btn btn-outline btn-error" @click=${this.logoutAll}>
            ${t("account.logoutAll")}
          </button>
        </div>
        <p class="text-base-content/60 text-sm">${t("account.logoutAllHint")}</p>
      </section>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "account-view": AccountView;
  }
}
