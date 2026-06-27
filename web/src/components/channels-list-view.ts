import { html, nothing } from "lit";
import { customElement, query, state } from "lit/decorators.js";
import {
  pageHeader,
  errorBox,
  emptyState,
  skeletonRows,
  spinner,
} from "./ui.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { can } from "../state/can.js";
import { t, tDynamic } from "../i18n.js";
import { toast, toastError } from "../toast.js";
import type { Channel } from "../api/types.js";

import { icon } from "../icons.js";
import "./pulse-ledger.js";
import "./confirm-dialog.js";
import type { ConfirmDialog } from "./confirm-dialog.js";

// Channels list (PRD-006, RFC-013 section 7). Fetches the active org's channels
// and renders the broadsheet redesign: a masthead, a hero glance (total, the
// by-type split and how many are enabled), then an editorial ledger of indexed
// rows. "New channel" plus the per-row edit, delete and send-test actions show to
// member+ and are hidden for a viewer (can("channel.write") / can("channel.test")).
// Secrets are never listed here; only name, type and the enabled flag are shown.
// The server re-checks every action (RFC-013 section 4.3).
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
      toastError(err, t("state.error"));
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
    if (!this.canWrite) return nothing;
    return html`<a class="pulse-btn" href=${`${this.base}/channels/new`}>
      ${icon("plus", "size-4")}${t("channels.new")}
    </a>`;
  }

  override render() {
    return html`
      <div class="-mx-6 lg:-mx-10 -my-7">
        ${pageHeader(t("channels.heading"), this.newChannelButton())}
        <div class="px-6 lg:px-10 py-7">${this.body()}</div>
      </div>
      <confirm-dialog
        heading=${t("channels.deleteHeading")}
        message=${t("channels.deleteMessage")}
        confirmLabel=${t("channels.delete")}
        ?danger=${true}
        @confirm=${this.onDeleteConfirmed}
        @cancel=${() => (this.pendingDelete = null)}
      ></confirm-dialog>
    `;
  }

  private body() {
    if (this.loading && this.channels === null) {
      return skeletonRows(5);
    }

    if (this.error) {
      return errorBox(this.error, () => this.retry(), t("state.retry"));
    }

    if (!this.channels || this.channels.length === 0) {
      return emptyState(
        icon("bell", "size-10"),
        t("channels.empty"),
        t("channels.emptyHint"),
        this.canWrite
          ? html`<a class="pulse-btn" href=${`${this.base}/channels/new`}
              >${icon("plus", "size-4")}${t("channels.new")}</a
            >`
          : undefined,
      );
    }

    return html`<pulse-ledger
      .items=${this.channels}
      .renderRow=${(item: unknown) => this.row(item as Channel)}
    ></pulse-ledger>`;
  }

  // One patch-panel port: the destination TYPE leads in a bordered mono column (the
  // "port"), a connector dot wires it to the channel name, and a live/off indicator
  // plus the member-only actions trail. A disabled channel reads as an unpatched,
  // dormant port (hollow dot, muted type).
  private row(c: Channel) {
    const live = c.enabled;
    return html`<div
      data-channel-row
      class="grid grid-cols-[164px_1fr_auto] items-stretch border-b border-hair"
    >
      <div
        class="flex items-center pl-6 lg:pl-10 pr-4 py-5 border-r border-line ${live
          ? "bg-paper"
          : ""}"
      >
        <span
          class="font-mono text-[12px] font-bold uppercase tracking-[0.08em] whitespace-nowrap ${live
            ? "text-brand"
            : "text-ink3"}"
          >${c.type}</span
        >
      </div>
      <div class="flex items-center gap-4 px-5 py-5 min-w-0">
        <span
          class="size-2.5 shrink-0 ${live ? "bg-up" : "border border-ink3"}"
          aria-hidden="true"
        ></span>
        ${this.canWrite
          ? html`<a
              class="font-disp font-extrabold text-[17px] tracking-[-0.025em] text-ink hover:text-brand hover:no-underline truncate"
              href=${`${this.base}/channels/${c.id}/edit`}
              >${c.name}</a
            >`
          : html`<span
              class="font-disp font-extrabold text-[17px] tracking-[-0.025em] text-ink truncate"
              >${c.name}</span
            >`}
      </div>
      <div class="flex items-center justify-end gap-4 pr-6 lg:pr-10 pl-4 py-5">
        <span
          class="font-mono text-[11px] uppercase tracking-[0.1em] ${live
            ? "text-up"
            : "text-ink3"}"
          >${live
            ? tDynamic("channels.live", "Live", {})
            : tDynamic("channels.off", "Off", {})}</span
        >
        ${this.rowActions(c)}
      </div>
    </div>`;
  }

  private rowActions(c: Channel) {
    if (!this.canWrite && !this.canTest) return nothing;
    return html`<div class="flex items-center gap-1.5">
      ${this.canTest
        ? html`<button
            class="pulse-iconbtn"
            aria-label=${t("channels.test")}
            ?disabled=${this.testing === c.id}
            @click=${() => this.sendTest(c)}
          >
            ${this.testing === c.id ? spinner() : icon("bell", "size-4")}
          </button>`
        : ""}
      ${this.canWrite
        ? html`<a
              class="pulse-iconbtn"
              href=${`${this.base}/channels/${c.id}/edit`}
              aria-label=${t("channels.edit")}
              >${icon("edit", "size-4")}</a
            ><button
              class="pulse-iconbtn hover:text-down"
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
