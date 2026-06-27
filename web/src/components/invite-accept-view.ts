import { html } from "lit";
import { customElement, property, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, rememberLastOrg, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { navigate } from "../router.js";
import { session } from "../state/session.js";
import { t, tDynamic } from "../i18n.js";
import { toast } from "../toast.js";
import { icon } from "../icons.js";
import { spinner } from "./ui.js";
import type { InvitationPreview, Role } from "../api/types.js";

// Invitation accept page (RFC-003 2.6), route /invitations/:token. Reachable
// pre-login: it loads the token preview (org name, role, inviter) without a
// session and shows "Join {org} as {role}?". If the user is not signed in, the
// accept button routes through login with return_to set to this page so they land
// back here afterwards. When signed in, accept POSTs the token; the server checks
// the signed-in verified email matches the invited email and returns the new
// membership, after which we refresh /me and navigate into the org.
//
// Errors are localized: a 403 is an email mismatch, a 404/409 is expired/invalid.
@customElement("invite-accept-view")
export class InviteAcceptView extends AppElement {
  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @property({ type: String }) token = "";

  @state() private preview: InvitationPreview | null = null;
  @state() private loading = true;
  @state() private loadError: string | null = null;
  @state() private accepting = false;
  @state() private acceptError: string | null = null;

  private loadedToken: string | null = null;

  override updated(): void {
    if (this.token && this.token !== this.loadedToken) void this.load();
  }

  private async load(): Promise<void> {
    this.loadedToken = this.token;
    this.loading = true;
    this.loadError = null;
    try {
      this.preview = await client.getInvitationPreview(this.token);
    } catch (err) {
      this.preview = null;
      this.loadError = this.errorMessage(err, t("accept.errNotFound"));
    } finally {
      this.loading = false;
    }
  }

  // Map an API error to a localized message: 403 -> email mismatch, 404/409 ->
  // expired/invalid, anything else -> the given fallback. Server codes are run
  // through tDynamic so a localized code wins over the generic copy.
  private errorMessage(err: unknown, fallback: string): string {
    if (!(err instanceof ApiError)) return t("state.error");
    if (err.status === 403) return t("accept.errMismatch");
    if (err.status === 404 || err.status === 409) return t("accept.errExpired");
    return tDynamic(err.code, err.message || fallback, err.params);
  }

  private roleLabel(role: Role): string {
    return t(`role.${role}` as const);
  }

  private get loggedIn(): boolean {
    return session.isLoggedIn;
  }

  // Send a logged-out user to login, preserving this page as the return target so
  // they come straight back to accept after signing in.
  private signIn(): void {
    const returnTo = encodeURIComponent(`/invitations/${this.token}`);
    navigate(`/login?return_to=${returnTo}`);
  }

  private async accept(): Promise<void> {
    if (this.accepting) return;
    this.accepting = true;
    this.acceptError = null;
    try {
      const org = await client.acceptInvitation(this.token);
      rememberLastOrg(org.org_id);
      await this.ctx.refreshMe();
      toast(
        tDynamic("accept.accepted", "", {
          org: this.preview?.org_name ?? org.name,
        }),
        "success",
      );
      navigate(`/orgs/${org.org_id}`);
    } catch (err) {
      this.acceptError = this.errorMessage(err, t("accept.errExpired"));
    } finally {
      this.accepting = false;
    }
  }

  override render() {
    return html`
      <div class="min-h-screen grid place-items-center p-6 bg-bg">
        <div class="w-full max-w-sm flex flex-col gap-6">
          <!-- Editorial header: a mono kicker over a heavy Archivo heading. -->
          <div class="flex items-center gap-3">
            <span class="text-brand">${icon("users", "size-8")}</span>
            <div class="flex flex-col gap-1">
              <span class="pulse-label">${tDynamic("accept.kicker", "Invitation")}</span>
              <h1
                class="m-0 font-disp font-black uppercase tracking-[-0.04em] leading-[0.85] text-[30px]"
              >
                ${t("accept.heading")}
              </h1>
            </div>
          </div>
          <div class="border border-hair bg-bg p-7">${this.body()}</div>
        </div>
      </div>
    `;
  }

  private body() {
    if (this.loading) {
      return html`<span class="text-ink3">${spinner()}</span>`;
    }

    if (this.loadError || !this.preview) {
      return html`<div
        role="alert"
        class="border border-down px-4 py-3 w-full text-sm text-down"
      >
        <span>${this.loadError ?? t("accept.errNotFound")}</span>
      </div>`;
    }

    const p = this.preview;
    return html`
      <div class="flex flex-col gap-5">
        <div>
          <div class="pulse-label">${tDynamic("accept.joinLabel", "Join")}</div>
          <div
            class="font-disp font-black text-[28px] tracking-[-0.035em] leading-[0.95] mt-1.5"
          >
            ${p.org_name}
          </div>
          <div class="mt-2.5 flex flex-wrap items-center gap-2.5">
            <span class="pulse-tag">${this.roleLabel(p.role)}</span>
            ${p.inviter_name
              ? html`<span class="font-mono text-[11.5px] text-ink3">
                  ${tDynamic("accept.invitedBy", "", { name: p.inviter_name })}
                </span>`
              : ""}
          </div>
        </div>
        <p class="font-mono text-[11.5px] text-ink3">
          ${tDynamic("accept.invitedEmail", "", { email: p.email })}
        </p>
        ${this.acceptError
          ? html`<div
              role="alert"
              class="border border-down px-4 py-3 w-full text-sm text-down"
            >
              <span>${this.acceptError}</span>
            </div>`
          : ""}
        ${this.loggedIn ? this.acceptActions() : this.signInActions()}
      </div>
    `;
  }

  private acceptActions() {
    return html`<div class="w-full flex flex-col gap-2">
      <button
        class="pulse-btn w-full"
        ?disabled=${this.accepting}
        @click=${this.accept}
      >
        ${this.accepting ? t("accept.accepting") : t("accept.accept")}
      </button>
      <a class="pulse-btn pulse-btn-ghost pulse-btn-sm w-full" href="/">
        ${t("accept.decline")}
      </a>
    </div>`;
  }

  private signInActions() {
    return html`<div class="w-full flex flex-col gap-2">
      <p class="text-ink3 text-sm">${t("accept.signInPrompt")}</p>
      <button class="pulse-btn w-full" @click=${this.signIn}>
        ${t("accept.signIn")}
      </button>
    </div>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "invite-accept-view": InviteAcceptView;
  }
}
