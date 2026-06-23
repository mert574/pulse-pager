import { html } from "lit";
import { customElement, query, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { can } from "../state/can.js";
import { t } from "../i18n.js";
import { toast } from "../toast.js";
import { icon } from "../icons.js";
import type { ApiKey, ApiKeyCreated, Role } from "../api/types.js";

import "./data-table.js";
import "./confirm-dialog.js";
import "./form-field.js";
import "./relative-time.js";
import type { DataColumn } from "./data-table.js";
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
      toast(err instanceof ApiError ? err.message : t("state.error"), "error");
    } finally {
      this.busyKey = null;
    }
  }

  // --- render ---

  override render() {
    if (!this.hasAccess) {
      return html`<div role="status" class="alert alert-info">
        <span>${t("apiKeys.noAccess")}</span>
      </div>`;
    }

    // Entitlements not resolved yet: show a skeleton rather than flash the upgrade
    // prompt before we know the plan's API access.
    if (!this.ctx?.entitlements) {
      return html`<div class="skeleton h-24 w-full" aria-busy="true"></div>`;
    }

    // Plan gate: the Free tier has no API access, so it gets the upgrade prompt in
    // place of the key-management UI (server rejects key creation too).
    if (!this.planAllowsApi) {
      return html`
        <div class="flex flex-col gap-3">
          <h1 class="text-2xl font-bold">${t("apiKeys.heading")}</h1>
          <div role="status" class="alert alert-warning">
            <span>${t("apiKeys.upgrade")}</span>
            ${this.orgId
              ? html`<a
                  class="btn btn-sm whitespace-nowrap"
                  href=${`/orgs/${this.orgId}/billing`}
                  >${t("upsell.upgrade")}</a
                >`
              : ""}
          </div>
        </div>
      `;
    }

    return html`
      <div class="flex flex-col gap-4">
        <div class="flex items-center justify-between">
          <h1 class="text-2xl font-bold">${t("apiKeys.heading")}</h1>
          <button class="btn btn-primary btn-sm gap-1.5" @click=${this.openCreate}>
            ${icon("plus", "size-4")}${t("apiKeys.new")}
          </button>
        </div>
        <a
          class="link link-hover inline-flex w-fit items-center gap-1.5 text-sm text-base-content/70"
          href="/api/docs"
          target="_blank"
          rel="noopener noreferrer"
        >
          ${icon("externalLink", "size-4")}${t("apiKeys.docs")}
        </a>
        ${this.apiWriteAllowed
          ? ""
          : html`<div role="status" class="alert alert-info">
              <span>${t("apiKeys.readOnlyNote")}</span>
            </div>`}
        ${this.body()}
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

  private body() {
    if (this.loading && this.keys === null) {
      return html`<div class="flex flex-col gap-2" aria-busy="true">
        ${Array.from({ length: 4 }).map(
          () => html`<div class="skeleton h-12 w-full"></div>`,
        )}
      </div>`;
    }

    if (this.error) {
      return html`<div role="alert" class="alert alert-error">
        <span>${this.error}</span>
        <button class="btn btn-sm" @click=${this.retry}>
          ${t("state.retry")}
        </button>
      </div>`;
    }

    if (!this.keys || this.keys.length === 0) {
      return html`<div
        class="rounded-box border border-dashed border-base-300 p-12 flex flex-col items-center text-center gap-3"
      >
        <span class="text-primary/70">${icon("key", "size-10")}</span>
        <div>
          <p class="font-semibold text-lg">${t("apiKeys.empty")}</p>
          <p class="text-base-content/60 mt-1">${t("apiKeys.emptyHint")}</p>
        </div>
        <button class="btn btn-primary btn-sm gap-1.5" @click=${this.openCreate}>
          ${icon("plus", "size-4")}${t("apiKeys.new")}
        </button>
      </div>`;
    }

    return html`<data-table
      .columns=${this.columns()}
      .data=${this.keys}
      .pageSize=${15}
    ></data-table>`;
  }

  private columns(): DataColumn[] {
    return [
      {
        id: "name",
        header: t("apiKeys.colName"),
        accessor: (r) => (r as ApiKey).name,
        sortable: true,
        cell: (r) =>
          html`<span class="font-medium">${(r as ApiKey).name}</span>`,
      },
      {
        id: "prefix",
        header: t("apiKeys.colPrefix"),
        accessor: (r) => (r as ApiKey).prefix,
        sortable: true,
        cell: (r) =>
          html`<code class="text-sm">${(r as ApiKey).prefix}</code>`,
      },
      {
        id: "role",
        header: t("apiKeys.colRole"),
        accessor: (r) => (r as ApiKey).role,
        sortable: true,
        cell: (r) =>
          html`<span class="badge badge-ghost badge-sm"
            >${this.roleLabel((r as ApiKey).role)}</span
          >`,
      },
      {
        id: "created",
        header: t("apiKeys.colCreated"),
        accessor: (r) => (r as ApiKey).created_at,
        sortable: true,
        cell: (r) =>
          html`<relative-time .datetime=${(r as ApiKey).created_at}></relative-time>`,
      },
      {
        id: "lastUsed",
        header: t("apiKeys.colLastUsed"),
        accessor: (r) => (r as ApiKey).last_used_at ?? "",
        sortable: true,
        cell: (r) => {
          const v = (r as ApiKey).last_used_at;
          return v
            ? html`<relative-time .datetime=${v}></relative-time>`
            : html`<span class="text-base-content/50">${t("apiKeys.never")}</span>`;
        },
      },
      {
        id: "actions",
        header: "",
        class: "text-right",
        cell: (r) => this.rowActions(r as ApiKey),
      },
    ];
  }

  private rowActions(key: ApiKey) {
    return html`<div class="flex items-center justify-end gap-1">
      <button
        class="btn btn-sm btn-ghost btn-square"
        aria-label=${t("apiKeys.revoke")}
        ?disabled=${this.busyKey === key.id}
        @click=${() => this.askRevoke(key)}
      >
        ${icon("trash", "size-4")}
      </button>
    </div>`;
  }

  // The create dialog. A daisyUI modal driven by createOpen (the same modal-open
  // markup confirm-dialog uses), so the test can find .modal-open.
  private createDialog() {
    if (!this.createOpen) return html``;
    return html`
      <div
        class="modal modal-open"
        role="dialog"
        aria-modal="true"
        aria-labelledby="ak-create-heading"
      >
        <div class="modal-box">
          <h3 id="ak-create-heading" class="text-lg font-bold">
            ${t("apiKeys.createHeading")}
          </h3>
          <form class="flex flex-col gap-3 py-4" @submit=${this.onCreate}>
            <form-field
              label=${t("apiKeys.name")}
              fieldName="ak-name"
              .error=${this.createError?.fields?.name ?? null}
              .control=${html`<input
                id="ak-name"
                type="text"
                class="input w-full"
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
                class="select w-full"
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
            <div class="modal-action">
              <button
                type="button"
                class="btn"
                ?disabled=${this.creating}
                @click=${this.closeCreate}
              >
                ${t("dialog.cancel")}
              </button>
              <button
                type="submit"
                class="btn btn-primary"
                ?disabled=${this.creating}
              >
                ${this.creating ? t("apiKeys.creating") : t("apiKeys.create")}
              </button>
            </div>
          </form>
        </div>
        <div class="modal-backdrop" @click=${this.closeCreate}></div>
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
        class="modal modal-open"
        role="dialog"
        aria-modal="true"
        aria-labelledby="ak-secret-heading"
      >
        <div class="modal-box">
          <h3 id="ak-secret-heading" class="text-lg font-bold">
            ${t("apiKeys.secretHeading")}
          </h3>
          <div role="alert" class="alert alert-warning my-4">
            <span>${t("apiKeys.secretWarning")}</span>
          </div>
          <div class="flex items-center gap-2">
            <code
              class="flex-1 break-all rounded-box bg-base-200 p-3 text-sm"
              data-secret
              >${created.secret}</code
            >
            <button
              class="btn btn-square"
              aria-label=${t("apiKeys.copy")}
              @click=${this.copySecret}
            >
              ${icon("copy", "size-4")}
            </button>
          </div>
          ${this.copied
            ? html`<p class="mt-2 text-sm text-success">
                ${t("apiKeys.copied")}
              </p>`
            : ""}
          <div class="modal-action">
            <button class="btn btn-primary" @click=${this.dismissSecret}>
              ${t("apiKeys.secretDone")}
            </button>
          </div>
        </div>
        <div class="modal-backdrop" @click=${this.dismissSecret}></div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "api-keys-view": ApiKeysView;
  }
}
