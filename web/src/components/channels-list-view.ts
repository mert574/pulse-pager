import { html } from "lit";
import { customElement, query, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { can } from "../state/can.js";
import { t } from "../i18n.js";
import { toast } from "../toast.js";
import type { Channel } from "../api/types.js";

import { icon } from "../icons.js";
import "./data-table.js";
import "./confirm-dialog.js";
import type { DataColumn } from "./data-table.js";
import type { ConfirmDialog } from "./confirm-dialog.js";

// Channels list (PRD-006, RFC-013 section 7). Fetches the active org's channels
// and renders loading, empty (with the primary action), error (with retry), and
// the data table. "New channel" plus the per-row edit, delete and send-test
// actions are shown to member+ and hidden for a viewer (can("channel.write") /
// can("channel.test")). Secrets are never listed here; only name, type and the
// enabled flag are shown. The server re-checks every action (RFC-013 section 4.3).
@customElement("channels-list-view")
export class ChannelsListView extends AppElement {
  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private channels: Channel[] | null = null;
  @state() private loading = false;
  @state() private error: string | null = null;
  // the channel id queued for deletion, surfaced by the confirm dialog
  @state() private pendingDelete: Channel | null = null;
  // the channel id whose test send is in flight, so its button shows a spinner
  @state() private testing: string | null = null;

  private loadedOrgId: string | null = null;
  @query("confirm-dialog") private deleteDialog!: ConfirmDialog;

  override updated(): void {
    const orgId = this.ctx?.activeOrg?.org_id ?? null;
    if (orgId && orgId !== this.loadedOrgId) void this.load(orgId);
  }

  private async load(orgId: string): Promise<void> {
    this.loadedOrgId = orgId;
    this.loading = true;
    this.error = null;
    try {
      this.channels = await client.listChannels(orgId);
    } catch (err) {
      this.error = err instanceof ApiError ? err.message : t("state.error");
      this.channels = null;
    } finally {
      this.loading = false;
    }
  }

  private retry(): void {
    const orgId = this.ctx?.activeOrg?.org_id;
    if (orgId) void this.load(orgId);
  }

  private get base(): string {
    return `/orgs/${this.ctx?.activeOrg?.org_id ?? ""}`;
  }

  private get canWrite(): boolean {
    return can(this.ctx?.role ?? null, "channel.write");
  }
  private get canTest(): boolean {
    return can(this.ctx?.role ?? null, "channel.test");
  }

  private askDelete(channel: Channel): void {
    this.pendingDelete = channel;
    // the dialog is always rendered (see render()); just open it
    this.deleteDialog.open();
  }

  private async onDeleteConfirmed(): Promise<void> {
    const orgId = this.ctx?.activeOrg?.org_id;
    const channel = this.pendingDelete;
    this.pendingDelete = null;
    if (!orgId || !channel) return;
    try {
      await client.deleteChannel(orgId, channel.id);
      toast(t("channels.deleted"), "success");
      this.channels = (this.channels ?? []).filter((c) => c.id !== channel.id);
    } catch (err) {
      toast(err instanceof ApiError ? err.message : t("state.error"), "error");
    }
  }

  private async sendTest(channel: Channel): Promise<void> {
    const orgId = this.ctx?.activeOrg?.org_id;
    if (!orgId) return;
    this.testing = channel.id;
    try {
      await client.testChannel(orgId, channel.id);
      toast(t("channels.testSent"), "success");
    } catch (err) {
      toast(
        err instanceof ApiError ? err.message : t("channels.testFailed"),
        "error",
      );
    } finally {
      this.testing = null;
    }
  }

  private newChannelButton() {
    if (!this.canWrite) return "";
    return html`<a
      class="btn btn-primary btn-sm gap-1.5"
      href=${`${this.base}/channels/new`}
    >
      ${icon("plus", "size-4")}${t("channels.new")}
    </a>`;
  }

  override render() {
    return html`
      <div class="flex flex-col gap-4">
        <div class="flex items-center justify-between">
          <h1 class="text-2xl font-bold">${t("channels.heading")}</h1>
          ${this.newChannelButton()}
        </div>
        ${this.body()}
        <confirm-dialog
          heading=${t("channels.deleteHeading")}
          message=${t("channels.deleteMessage")}
          confirmLabel=${t("channels.delete")}
          ?danger=${true}
          @confirm=${this.onDeleteConfirmed}
          @cancel=${() => (this.pendingDelete = null)}
        ></confirm-dialog>
      </div>
    `;
  }

  private body() {
    if (this.loading && this.channels === null) {
      return html`<div class="flex flex-col gap-2" aria-busy="true">
        ${Array.from({ length: 5 }).map(
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

    if (!this.channels || this.channels.length === 0) {
      return html`<div
        class="rounded-box border border-dashed border-base-300 p-12 flex flex-col items-center text-center gap-3"
      >
        <span class="text-primary/70">${icon("bell", "size-10")}</span>
        <div>
          <p class="font-semibold text-lg">${t("channels.empty")}</p>
          <p class="text-base-content/60 mt-1">${t("channels.emptyHint")}</p>
        </div>
        ${this.canWrite
          ? html`<a
              class="btn btn-primary btn-sm gap-1.5"
              href=${`${this.base}/channels/new`}
              >${icon("plus", "size-4")}${t("channels.new")}</a
            >`
          : ""}
      </div>`;
    }

    return html`<data-table
      .columns=${this.columns()}
      .data=${this.channels}
      .pageSize=${15}
    ></data-table>`;
  }

  private columns(): DataColumn[] {
    const base = this.base;
    const cols: DataColumn[] = [
      {
        id: "name",
        header: t("channels.colName"),
        accessor: (r) => (r as Channel).name,
        sortable: true,
        cell: (r) => {
          const c = r as Channel;
          return this.canWrite
            ? html`<a
                class="link link-hover font-medium"
                href=${`${base}/channels/${c.id}/edit`}
                >${c.name}</a
              >`
            : html`<span class="font-medium">${c.name}</span>`;
        },
      },
      {
        id: "type",
        header: t("channels.colType"),
        accessor: (r) => (r as Channel).type,
        sortable: true,
        cell: (r) =>
          html`<span class="badge badge-ghost badge-sm"
            >${(r as Channel).type}</span
          >`,
      },
      {
        id: "enabled",
        header: t("channels.colEnabled"),
        accessor: (r) => ((r as Channel).enabled ? 1 : 0),
        sortable: true,
        cell: (r) =>
          (r as Channel).enabled
            ? html`<span class="badge badge-success badge-sm"
                >${t("channels.yes")}</span
              >`
            : html`<span class="badge badge-ghost badge-sm"
                >${t("channels.no")}</span
              >`,
      },
    ];

    if (this.canWrite || this.canTest) {
      cols.push({
        id: "actions",
        header: "",
        class: "text-right",
        cell: (r) => this.rowActions(r as Channel),
      });
    }
    return cols;
  }

  private rowActions(c: Channel) {
    return html`<div class="flex items-center justify-end gap-1">
      ${this.canTest
        ? html`<button
            class="btn btn-sm btn-ghost gap-1.5"
            ?disabled=${this.testing === c.id}
            @click=${() => this.sendTest(c)}
          >
            ${this.testing === c.id
              ? html`<span class="loading loading-spinner loading-xs"></span
                  >${t("channels.testing")}`
              : html`${icon("bell", "size-4")}${t("channels.test")}`}
          </button>`
        : ""}
      ${this.canWrite
        ? html`<a
              class="btn btn-sm btn-ghost btn-square"
              href=${`${this.base}/channels/${c.id}/edit`}
              aria-label=${t("channels.edit")}
              >${icon("edit", "size-4")}</a
            ><button
              class="btn btn-sm btn-ghost btn-square"
              aria-label=${t("channels.delete")}
              @click=${() => this.askDelete(c)}
            >
              ${icon("trash", "size-4")}
            </button>`
        : ""}
    </div>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "channels-list-view": ChannelsListView;
  }
}
