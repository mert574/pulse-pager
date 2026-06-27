import { html, nothing } from "lit";
import { customElement, query, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { can } from "../state/can.js";
import { t, tDynamic } from "../i18n.js";
import { toast, toastError } from "../toast.js";
import { icon } from "../icons.js";
import {
  pageHeader,
  errorBox,
  emptyState,
  skeletonRows,
} from "./ui.js";
import type { ApiKey, ApiKeyCreated, Role } from "../api/types.js";

import "./confirm-dialog.js";
import "./form-field.js";
import "./relative-time.js";
import type { ConfirmDialog } from "./confirm-dialog.js";

// API keys (PRD-001 App A, RFC-013 section 4.3). Owner/admin only: a member or
// viewer who reaches this route sees a "managed by owners/admins" message instead
// of an error, and the nav entry is already hidden for them (can("apikey.manage")).
// The server re-checks and 403s the list call for anyone below admin anyway.
//
// The create dialog offers member or admin only; owner is never offered (an API
// key is never owner-equivalent) and the server rejects it too. On create the
// response carries the full pulse_sk_ secret exactly once: we show it in a copy
// panel with a "you will not see this again" warning, then drop it on dismiss.
// The secret is never put in the list, which only ever has the prefix.
@customElement("api-keys-view")
export class ApiKeysView extends AppElement {
  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private keys: ApiKey[] | null = null;
  @state() private loading = false;
  @state() private error: string | null = null;

  // the key id queued for revoke, surfaced by the confirm dialog
  @state() private pendingRevoke: ApiKey | null = null;
  @state() private busyKey: string | null = null;

  // create dialog state
  @state() private createOpen = false;
  @state() private newName = "";
  @state() private newRole: Role = "member";
  @state() private creating = false;
  @state() private createError: ApiError | null = null;

  // the just-created secret, shown once then dropped on dismiss. While set, the
  // one-time panel is shown in place of the create dialog.
  @state() private created: ApiKeyCreated | null = null;
  @state() private copied = false;

  private loadedOrgId: string | null = null;

  @query("#revoke-dialog") private revokeDialog!: ConfirmDialog;

  // Roles offered in the create picker. owner is never offered; the server rejects
  // it too. This just keeps it out of the UI.
  private readonly assignableRoles: Role[] = ["member", "admin"];

  private get orgId(): string | null {
    return this.ctx?.activeOrg?.org_id ?? null;
  }

  private get hasAccess(): boolean {
    return can(this.ctx?.role ?? null, "apikey.manage");
  }

  // The plan's API entitlement comes straight from the server (no plan-name reading
  // in the FE, which would duplicate and drift from the backend matrix). Null until
  // the entitlements call resolves; render shows a loading state until then.
  private get planAllowsApi(): boolean {
    return this.ctx?.entitlements?.api_access_allowed ?? false;
  }
  private get apiWriteAllowed(): boolean {
    return this.ctx?.entitlements?.api_write_allowed ?? false;
  }

  override updated(): void {
    if (!this.hasAccess || !this.ctx?.entitlements?.api_access_allowed) return;
    const orgId = this.orgId;
    if (orgId && orgId !== this.loadedOrgId) void this.load(orgId);
  }

  private async load(orgId: string): Promise<void> {
    this.loadedOrgId = orgId;
    this.loading = true;
    this.error = null;
    try {
      this.keys = await client.listApiKeys(orgId);
    } catch (err) {
      this.error = err instanceof ApiError ? err.message : t("state.error");
      this.keys = null;
    } finally {
      this.loading = false;
    }
  }

  private retry(): void {
    if (this.orgId) {
      this.loadedOrgId = null;
      void this.load(this.orgId);
    }
  }

  private roleLabel(role: Role): string {
    return t(`role.${role}` as const);
  }

  // --- create ---

  private openCreate(): void {
    this.newName = "";
    this.newRole = "member";
    this.createError = null;
    this.createOpen = true;
  }

  private closeCreate(): void {
    this.createOpen = false;
    this.createError = null;
  }

  private async onCreate(e: Event): Promise<void> {
    e.preventDefault();
    const orgId = this.orgId;
    if (!orgId || this.creating) return;
    const name = this.newName.trim();
    if (!name) {
      this.createError = new ApiError(422, {
        code: "validation_failed",
        message: t("apiKeys.errName"),
        fields: { name: t("apiKeys.errName") },
      });
      return;
    }
    this.creating = true;
    this.createError = null;
    try {
      const created = await client.createApiKey(orgId, name, this.newRole);
      // append the new key (without its secret) to the list, then swap the create
      // dialog for the one-time secret panel.
      this.keys = [created.key, ...(this.keys ?? [])];
      this.created = created;
      this.copied = false;
      this.createOpen = false;
      toast(t("apiKeys.created"), "success");
    } catch (err) {
      if (err instanceof ApiError) {
        this.createError = err;
        toast(err.message, "error");
      } else {
        toast(t("state.error"), "error");
      }
    } finally {
      this.creating = false;
    }
  }

  // Dismiss the one-time secret panel. The secret is dropped here and is never
  // recoverable; the list keeps only the prefix.
  private dismissSecret(): void {
    this.created = null;
    this.copied = false;
  }

  private async copySecret(): Promise<void> {
    const secret = this.created?.secret;
    if (!secret) return;
    try {
      await navigator.clipboard.writeText(secret);
      this.copied = true;
      toast(t("apiKeys.copied"), "success");
    } catch {
      toast(t("apiKeys.copyFailed"), "error");
    }
  }

  // --- revoke ---

  private askRevoke(key: ApiKey): void {
    this.pendingRevoke = key;
    this.revokeDialog.open();
  }

  private async onRevokeConfirmed(): Promise<void> {
    const orgId = this.orgId;
    const key = this.pendingRevoke;
    this.pendingRevoke = null;
    if (!orgId || !key) return;
    this.busyKey = key.id;
    try {
      await client.revokeApiKey(orgId, key.id);
      this.keys = (this.keys ?? []).filter((k) => k.id !== key.id);
      toast(t("apiKeys.revoked"), "success");
    } catch (err) {
      toastError(err, t("state.error"));
    } finally {
      this.busyKey = null;
    }
  }

  // --- render ---

  override render() {
    if (!this.hasAccess) {
      return html`<div
        role="status"
        class="border border-hair bg-paper px-4 py-3 text-ink2"
      >
        ${t("apiKeys.noAccess")}
      </div>`;
    }

    // Entitlements not resolved yet: show a skeleton rather than flash the upgrade
    // prompt before we know the plan's API access.
    if (!this.ctx?.entitlements) {
      return html`<div
        class="h-24 w-full bg-paper animate-pulse"
        aria-busy="true"
      ></div>`;
    }

    const actions = this.planAllowsApi ? this.newButton() : nothing;
    return html`<div class="-mx-6 lg:-mx-10 -my-7">
      ${pageHeader(t("apiKeys.heading"), actions)}
      <div class="px-6 lg:px-10 py-7">${this.body()}</div>
    </div>`;
  }

  private newButton() {
    return html`<button class="pulse-btn" @click=${this.openCreate}>
      ${icon("plus", "size-4")}${t("apiKeys.new")}
    </button>`;
  }

  private body() {
    // Plan gate: the Free tier has no API access, so it gets the upgrade prompt in
    // place of the key-management UI (server rejects key creation too).
    if (!this.planAllowsApi) {
      return html`<div
        role="status"
        class="flex flex-wrap items-center gap-3 border border-deg px-4 py-3 text-deg"
      >
        <span>${t("apiKeys.upgrade")}</span>
        ${this.orgId
          ? html`<a
              class="pulse-btn pulse-btn-sm whitespace-nowrap"
              href=${`/orgs/${this.orgId}/billing`}
              >${t("upsell.upgrade")}</a
            >`
          : ""}
      </div>`;
    }

    return html`
      <div class="flex flex-col gap-4">
        <a
          class="inline-flex w-fit items-center gap-1.5 text-sm text-ink2 hover:text-brand hover:no-underline"
          href="/api/docs"
          target="_blank"
          rel="noopener noreferrer"
        >
          ${icon("externalLink", "size-4")}${t("apiKeys.docs")}
        </a>
        ${this.apiWriteAllowed
          ? ""
          : html`<div
              role="status"
              class="border border-hair bg-paper px-4 py-3 text-ink2"
            >
              ${t("apiKeys.readOnlyNote")}
            </div>`}
        ${this.keyList()}
        ${this.createDialog()} ${this.secretPanel()}
        <confirm-dialog
          id="revoke-dialog"
          heading=${t("apiKeys.revokeHeading")}
          message=${t("apiKeys.revokeMessage")}
          confirmLabel=${t("apiKeys.revoke")}
          ?danger=${true}
          @confirm=${this.onRevokeConfirmed}
          @cancel=${() => (this.pendingRevoke = null)}
        ></confirm-dialog>
      </div>
    `;
  }

  private keyList() {
    if (this.loading && this.keys === null) {
      return skeletonRows(4);
    }

    if (this.error) {
      return errorBox(this.error, () => this.retry(), t("state.retry"));
    }

    if (!this.keys || this.keys.length === 0) {
      return emptyState(
        icon("key", "size-10"),
        t("apiKeys.empty"),
        t("apiKeys.emptyHint"),
        html`<button class="pulse-btn" @click=${this.openCreate}>
          ${icon("plus", "size-4")}${t("apiKeys.new")}
        </button>`,
      );
    }

    // A terminal-style block: a bordered paper surface with a title strip, then one
    // monospace line per credential. The prefix leads each line, prompt and all.
    return html`<div data-key-list class="border border-line bg-paper">
      <div
        class="flex items-center justify-between gap-3 border-b border-hair px-4 lg:px-6 py-2.5 font-mono text-[10.5px] uppercase tracking-[0.16em] text-ink3"
      >
        <span>${tDynamic("apiKeys.terminalTitle", "Credentials", {})}</span>
        <span
          >${String(this.keys.length).padStart(2, "0")}
          ${tDynamic("apiKeys.statActive", "active", {})}</span
        >
      </div>
      ${this.keys.map((key) => this.row(key))}
    </div>`;
  }

  // One credential line. The masked prefix leads in prominent mono (with a "$"
  // prompt), the name and role scope sit underneath, and the created / last-used
  // meta plus the revoke action trail. last_used_at reads "—" when the key has
  // never been used.
  private row(key: ApiKey) {
    const lastUsed = key.last_used_at;
    return html`<div
      data-api-key-row
      class="flex flex-col gap-2 px-4 lg:px-6 py-4 border-b border-hair last:border-b-0 sm:flex-row sm:items-center sm:gap-5"
    >
      <div class="flex flex-col gap-1.5 min-w-0 flex-1">
        <div class="flex items-center gap-2 min-w-0">
          <span class="font-mono text-[13px] text-up select-none" aria-hidden="true"
            >$</span
          >
          <code
            class="font-mono font-bold text-[14.5px] text-brand truncate"
            >${key.prefix}…</code
          >
        </div>
        <div class="flex items-center gap-2.5 flex-wrap pl-[18px] min-w-0">
          <span
            class="font-disp font-extrabold text-[15px] tracking-[-0.02em] text-ink truncate"
            >${key.name}</span
          >
          <span class="pulse-tag whitespace-nowrap"
            >${this.roleLabel(key.role)}</span
          >
        </div>
      </div>
      <div
        class="flex items-center justify-between gap-4 pl-[18px] sm:pl-0 sm:justify-end"
      >
        <div
          class="flex flex-col items-start sm:items-end font-mono text-[10.5px] text-ink3 leading-snug whitespace-nowrap"
        >
          <span
            >${t("apiKeys.colCreated")}
            <relative-time .datetime=${key.created_at}></relative-time
          ></span>
          <span
            >${t("apiKeys.colLastUsed")}
            ${lastUsed
              ? html`<relative-time .datetime=${lastUsed}></relative-time>`
              : "—"}</span
          >
        </div>
        <button
          class="pulse-iconbtn hover:text-down"
          aria-label=${t("apiKeys.revoke")}
          ?disabled=${this.busyKey === key.id}
          @click=${() => this.askRevoke(key)}
        >
          ${icon("trash", "size-4")}
        </button>
      </div>
    </div>`;
  }

  // The create dialog. Driven by createOpen, with the same overlay/panel markup
  // confirm-dialog uses (a .pulse-dialog panel over a dimmed backdrop).
  private createDialog() {
    if (!this.createOpen) return html``;
    return html`
      <div
        class="fixed inset-0 z-50 flex items-center justify-center p-4"
        role="dialog"
        aria-modal="true"
        aria-labelledby="ak-create-heading"
      >
        <div class="absolute inset-0 bg-black/40" @click=${this.closeCreate}></div>
        <div
          class="pulse-dialog relative w-full max-w-md border border-line bg-bg p-6 flex flex-col gap-4"
        >
          <h3
            id="ak-create-heading"
            class="font-disp font-extrabold text-lg uppercase tracking-[-0.01em]"
          >
            ${t("apiKeys.createHeading")}
          </h3>
          <form class="flex flex-col gap-3" @submit=${this.onCreate}>
            <form-field
              label=${t("apiKeys.name")}
              fieldName="ak-name"
              .error=${this.createError?.fields?.name ?? null}
              .control=${html`<input
                id="ak-name"
                type="text"
                class="pulse-input w-full"
                .value=${this.newName}
                @input=${(e: Event) =>
                  (this.newName = (e.target as HTMLInputElement).value)}
                autocomplete="off"
              />`}
            ></form-field>
            <form-field
              label=${t("apiKeys.role")}
              fieldName="ak-role"
              .control=${html`<select
                id="ak-role"
                class="pulse-input w-full"
                .value=${this.newRole}
                @change=${(e: Event) =>
                  (this.newRole = (e.target as HTMLSelectElement).value as Role)}
              >
                ${this.assignableRoles.map(
                  (role) =>
                    html`<option value=${role} ?selected=${role === this.newRole}>
                      ${this.roleLabel(role)}
                    </option>`,
                )}
              </select>`}
            ></form-field>
            <div class="flex justify-end gap-2">
              <button
                type="button"
                class="pulse-btn pulse-btn-ghost"
                ?disabled=${this.creating}
                @click=${this.closeCreate}
              >
                ${t("dialog.cancel")}
              </button>
              <button
                type="submit"
                class="pulse-btn"
                ?disabled=${this.creating}
              >
                ${this.creating ? t("apiKeys.creating") : t("apiKeys.create")}
              </button>
            </div>
          </form>
        </div>
      </div>
    `;
  }

  // The one-time secret panel. Prominent, with the full secret, a copy control,
  // and a "you will not see this again" warning. Dismissing drops the secret.
  private secretPanel() {
    const created = this.created;
    if (!created) return html``;
    return html`
      <div
        class="fixed inset-0 z-50 flex items-center justify-center p-4"
        role="dialog"
        aria-modal="true"
        aria-labelledby="ak-secret-heading"
      >
        <div
          class="absolute inset-0 bg-black/40"
          @click=${this.dismissSecret}
        ></div>
        <div
          class="pulse-dialog relative w-full max-w-md border border-line bg-bg p-6 flex flex-col gap-4"
        >
          <h3
            id="ak-secret-heading"
            class="font-disp font-extrabold text-lg uppercase tracking-[-0.01em]"
          >
            ${t("apiKeys.secretHeading")}
          </h3>
          <div
            role="alert"
            data-warning
            class="border border-deg px-4 py-3 text-deg"
          >
            ${t("apiKeys.secretWarning")}
          </div>
          <div class="flex items-center gap-2">
            <code
              class="flex-1 break-all bg-paper p-3 text-sm font-mono"
              data-secret
              >${created.secret}</code
            >
            <button
              class="pulse-btn pulse-btn-ghost"
              aria-label=${t("apiKeys.copy")}
              @click=${this.copySecret}
            >
              ${icon("copy", "size-4")}
            </button>
          </div>
          ${this.copied
            ? html`<p class="text-sm text-up">${t("apiKeys.copied")}</p>`
            : ""}
          <div class="flex justify-end gap-2">
            <button class="pulse-btn" @click=${this.dismissSecret}>
              ${t("apiKeys.secretDone")}
            </button>
          </div>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "api-keys-view": ApiKeysView;
  }
}
