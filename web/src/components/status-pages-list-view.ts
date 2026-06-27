import { html, nothing } from "lit";
import { customElement, query, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { can } from "../state/can.js";
import { t, tDynamic } from "../i18n.js";
import { toast, toastError } from "../toast.js";
import { publicStatusUrl } from "./status-page-url.js";
import {
  pageHeader,
  errorBox,
  emptyState,
  skeletonRows,
  spinner,
} from "./ui.js";
import type { StatusPage } from "../api/types.js";

import { icon } from "../icons.js";
import "./confirm-dialog.js";
import type { ConfirmDialog } from "./confirm-dialog.js";

// Status pages list (PRD-004, RFC-013 section 8). A newsstand: every status page
// is a front-page preview card in a responsive grid. The page name is the masthead,
// the public slug reads as the strap underneath (an external link when published, a
// "Draft" stamp when not), and the publish/edit/delete actions sit in a footer.
// A draft card is dashed and dimmed so it reads as not-yet-public at a glance.
// Mutations show to member+ (can("statuspage.write")) and are hidden for a viewer.
// The server re-checks every action (RFC-013 section 4.3).
@customElement("status-pages-list-view")
export class StatusPagesListView extends AppElement {
  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private pages: StatusPage[] | null = null;
  @state() private loading = false;
  @state() private error: string | null = null;
  @state() private pendingDelete: StatusPage | null = null;
  // the page id whose publish toggle is in flight, so its control shows a spinner
  @state() private toggling: string | null = null;

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
      this.pages = await client.listStatusPages(orgId);
    } catch (err) {
      this.error = err instanceof ApiError ? err.message : t("state.error");
      this.pages = null;
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
    return can(this.ctx?.role ?? null, "statuspage.write");
  }

  private askDelete(page: StatusPage): void {
    this.pendingDelete = page;
    this.deleteDialog.open();
  }

  private async onDeleteConfirmed(): Promise<void> {
    const orgId = this.ctx?.activeOrg?.org_id;
    const page = this.pendingDelete;
    this.pendingDelete = null;
    if (!orgId || !page) return;
    try {
      await client.deleteStatusPage(orgId, page.id);
      toast(t("statusPages.deleted"), "success");
      this.pages = (this.pages ?? []).filter((p) => p.id !== page.id);
    } catch (err) {
      toastError(err, t("state.error"));
    }
  }

  private async togglePublish(page: StatusPage): Promise<void> {
    const orgId = this.ctx?.activeOrg?.org_id;
    if (!orgId) return;
    const publish = page.state !== "published";
    this.toggling = page.id;
    try {
      const updated = await client.publishStatusPage(orgId, page.id, publish);
      toast(t(publish ? "statusPages.published" : "statusPages.unpublished"), "success");
      this.pages = (this.pages ?? []).map((p) => (p.id === page.id ? updated : p));
    } catch (err) {
      toastError(err, t("state.error"));
    } finally {
      this.toggling = null;
    }
  }

  private newButton() {
    if (!this.canWrite) return nothing;
    return html`<a class="pulse-btn" href=${`${this.base}/status-pages/new`}>
      ${icon("plus", "size-4")}${t("statusPages.new")}
    </a>`;
  }

  override render() {
    return html`
      <div class="-mx-6 lg:-mx-10 -my-7">
        ${pageHeader(t("statusPages.heading"), this.newButton())}
        <div class="px-6 lg:px-10 py-7">${this.body()}</div>
      </div>
      <confirm-dialog
        heading=${t("statusPages.deleteHeading")}
        message=${t("statusPages.deleteMessage")}
        confirmLabel=${t("statusPages.delete")}
        ?danger=${true}
        @confirm=${this.onDeleteConfirmed}
        @cancel=${() => (this.pendingDelete = null)}
      ></confirm-dialog>
    `;
  }

  private body() {
    if (this.loading && this.pages === null) {
      return skeletonRows(4);
    }

    if (this.error) {
      return errorBox(this.error, () => this.retry(), t("state.retry"));
    }

    if (!this.pages || this.pages.length === 0) {
      return emptyState(
        icon("globe", "size-10"),
        t("statusPages.empty"),
        t("statusPages.emptyHint"),
        this.canWrite
          ? html`<a class="pulse-btn" href=${`${this.base}/status-pages/new`}
              >${icon("plus", "size-4")}${t("statusPages.new")}</a
            >`
          : undefined,
      );
    }

    // The newsstand: a gallery of front-page previews, one card per page.
    return html`<div
      class="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-3 gap-5"
    >
      ${this.pages.map((p) => this.card(p))}
    </div>`;
  }

  // One front-page preview. Published reads as a solid masthead with its public
  // link; a draft is dashed and dimmed with a stamp in place of the link, so the
  // two states are obvious across the gallery.
  private card(p: StatusPage) {
    const published = p.state === "published";
    const monitorCount = p.display_monitors.length;
    return html`<article
      data-status-page-card
      class="flex flex-col ${published
        ? "border border-line bg-bg"
        : "border border-dashed border-hair opacity-75"}"
    >
      <div
        class="flex items-center justify-between gap-3 px-5 pt-4 pb-3 border-b border-hair"
      >
        ${published
          ? html`<span class="pulse-state text-up"
              ><span class="pulse-state-sq bg-up"></span
              >${t("statusPages.statePublished")}</span
            >`
          : html`<span class="pulse-state text-deg"
              ><span class="pulse-state-sq bg-deg"></span
              >${t("statusPages.stateDraft")}</span
            >`}
        <span class="font-mono text-[11px] text-ink3 whitespace-nowrap">
          ${tDynamic("statusPages.monitorCount", "{n} monitors", {
            n: monitorCount,
          })}
        </span>
      </div>

      <div class="flex flex-col gap-4 px-5 pt-5 pb-5 flex-1">
        <div class="min-w-0">
          ${this.canWrite
            ? html`<a
                class="font-disp font-black uppercase tracking-[-0.04em] leading-[0.9] text-[27px] break-words ${published
                  ? "text-ink hover:text-brand"
                  : "text-ink2 hover:text-brand"} hover:no-underline"
                href=${`${this.base}/status-pages/${p.id}/edit`}
                >${p.name}</a
              >`
            : html`<span
                class="font-disp font-black uppercase tracking-[-0.04em] leading-[0.9] text-[27px] break-words ${published
                  ? "text-ink"
                  : "text-ink2"}"
                >${p.name}</span
              >`}
        </div>

        ${published
          ? html`<a
              class="inline-flex w-fit max-w-full items-center gap-1.5 font-mono text-[12.5px] text-brand hover:no-underline"
              href=${publicStatusUrl(p.slug)}
              target="_blank"
              rel="noopener noreferrer"
              >${icon("globe", "size-3.5 shrink-0")}<span class="truncate"
                >${p.slug}</span
              >${icon("externalLink", "size-3.5 shrink-0")}</a
            >`
          : html`<div
              class="inline-flex w-fit items-center gap-2 border border-dashed border-hair px-2.5 py-1"
            >
              <span
                class="font-disp font-black uppercase tracking-[0.04em] text-[11px] text-deg"
                >${t("statusPages.stateDraft")}</span
              >
              <span class="font-mono text-[10.5px] text-ink3"
                >${t("statusPages.draftNoUrl")}</span
              >
            </div>`}
      </div>

      ${this.canWrite ? this.cardActions(p, published) : nothing}
    </article>`;
  }

  private cardActions(p: StatusPage, published: boolean) {
    return html`<div
      class="flex items-center justify-between gap-2 border-t border-hair px-4 py-3"
    >
      <button
        class="pulse-btn pulse-btn-ghost pulse-btn-sm"
        ?disabled=${this.toggling === p.id}
        @click=${() => this.togglePublish(p)}
      >
        ${this.toggling === p.id ? spinner() : ""}
        ${t(published ? "statusPages.unpublish" : "statusPages.publish")}
      </button>
      <div class="flex items-center gap-1">
        <a
          class="pulse-iconbtn"
          href=${`${this.base}/status-pages/${p.id}/edit`}
          aria-label=${t("statusPages.edit")}
          >${icon("edit", "size-4")}</a
        ><button
          class="pulse-iconbtn hover:text-down"
          aria-label=${t("statusPages.delete")}
          @click=${() => this.askDelete(p)}
        >
          ${icon("trash", "size-4")}
        </button>
      </div>
    </div>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "status-pages-list-view": StatusPagesListView;
  }
}
