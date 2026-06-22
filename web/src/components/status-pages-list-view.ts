import { html } from "lit";
import { customElement, query, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { can } from "../state/can.js";
import { t } from "../i18n.js";
import { toast } from "../toast.js";
import { publicStatusUrl } from "./status-page-url.js";
import type { StatusPage } from "../api/types.js";

import { icon } from "../icons.js";
import "./data-table.js";
import "./confirm-dialog.js";
import type { DataColumn } from "./data-table.js";
import type { ConfirmDialog } from "./confirm-dialog.js";

// Status pages list (PRD-004, RFC-013 section 8). Lists the org's status pages
// with their published state and public link, and offers create/edit/delete plus
// a publish/unpublish toggle. Mutations show to member+ (can("statuspage.write"))
// and are hidden for a viewer. A 402 on create is surfaced as an inline upsell.
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
      toast(err instanceof ApiError ? err.message : t("state.error"), "error");
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
      toast(err instanceof ApiError ? err.message : t("state.error"), "error");
    } finally {
      this.toggling = null;
    }
  }

  private newButton() {
    if (!this.canWrite) return "";
    return html`<a
      class="btn btn-primary btn-sm gap-1.5"
      href=${`${this.base}/status-pages/new`}
    >
      ${icon("plus", "size-4")}${t("statusPages.new")}
    </a>`;
  }

  override render() {
    return html`
      <div class="flex flex-col gap-4">
        <div class="flex items-center justify-between">
          <h1 class="text-2xl font-bold">${t("statusPages.heading")}</h1>
          ${this.newButton()}
        </div>
        ${this.body()}
        <confirm-dialog
          heading=${t("statusPages.deleteHeading")}
          message=${t("statusPages.deleteMessage")}
          confirmLabel=${t("statusPages.delete")}
          ?danger=${true}
          @confirm=${this.onDeleteConfirmed}
          @cancel=${() => (this.pendingDelete = null)}
        ></confirm-dialog>
      </div>
    `;
  }

  private body() {
    if (this.loading && this.pages === null) {
      return html`<div class="flex flex-col gap-2" aria-busy="true">
        ${Array.from({ length: 4 }).map(
          () => html`<div class="skeleton h-12 w-full"></div>`,
        )}
      </div>`;
    }

    if (this.error) {
      return html`<div role="alert" class="alert alert-error">
        <span>${this.error}</span>
        <button class="btn btn-sm" @click=${this.retry}>${t("state.retry")}</button>
      </div>`;
    }

    if (!this.pages || this.pages.length === 0) {
      return html`<div
        class="rounded-box border border-dashed border-base-300 p-12 flex flex-col items-center text-center gap-3"
      >
        <span class="text-primary/70">${icon("globe", "size-10")}</span>
        <div>
          <p class="font-semibold text-lg">${t("statusPages.empty")}</p>
          <p class="text-base-content/60 mt-1">${t("statusPages.emptyHint")}</p>
        </div>
        ${this.canWrite
          ? html`<a
              class="btn btn-primary btn-sm gap-1.5"
              href=${`${this.base}/status-pages/new`}
              >${icon("plus", "size-4")}${t("statusPages.new")}</a
            >`
          : ""}
      </div>`;
    }

    return html`<data-table
      .columns=${this.columns()}
      .data=${this.pages}
      .pageSize=${15}
    ></data-table>`;
  }

  private columns(): DataColumn[] {
    const base = this.base;
    const cols: DataColumn[] = [
      {
        id: "name",
        header: t("statusPages.colName"),
        accessor: (r) => (r as StatusPage).name,
        sortable: true,
        cell: (r) => {
          const p = r as StatusPage;
          return this.canWrite
            ? html`<a
                class="link link-hover font-medium"
                href=${`${base}/status-pages/${p.id}/edit`}
                >${p.name}</a
              >`
            : html`<span class="font-medium">${p.name}</span>`;
        },
      },
      {
        id: "slug",
        header: t("statusPages.colSlug"),
        accessor: (r) => (r as StatusPage).slug,
        sortable: true,
        cell: (r) =>
          html`<code class="text-sm">${(r as StatusPage).slug}</code>`,
      },
      {
        id: "state",
        header: t("statusPages.colState"),
        accessor: (r) => ((r as StatusPage).state === "published" ? 1 : 0),
        sortable: true,
        cell: (r) =>
          (r as StatusPage).state === "published"
            ? html`<span class="badge badge-success badge-sm"
                >${t("statusPages.statePublished")}</span
              >`
            : html`<span class="badge badge-ghost badge-sm"
                >${t("statusPages.stateDraft")}</span
              >`,
      },
      {
        id: "url",
        header: t("statusPages.colPublicUrl"),
        cell: (r) => {
          const p = r as StatusPage;
          if (p.state !== "published") {
            return html`<span class="text-base-content/50 text-sm"
              >${t("statusPages.draftNoUrl")}</span
            >`;
          }
          return html`<a
            class="link link-hover inline-flex items-center gap-1 text-sm"
            href=${publicStatusUrl(p.slug)}
            target="_blank"
            rel="noopener noreferrer"
            >${t("statusPages.viewPublic")}${icon("externalLink", "size-3.5")}</a
          >`;
        },
      },
    ];

    if (this.canWrite) {
      cols.push({
        id: "actions",
        header: "",
        class: "text-right",
        cell: (r) => this.rowActions(r as StatusPage),
      });
    }
    return cols;
  }

  private rowActions(p: StatusPage) {
    const published = p.state === "published";
    return html`<div class="flex items-center justify-end gap-1">
      <button
        class="btn btn-sm btn-ghost gap-1.5"
        ?disabled=${this.toggling === p.id}
        @click=${() => this.togglePublish(p)}
      >
        ${this.toggling === p.id
          ? html`<span class="loading loading-spinner loading-xs"></span>`
          : ""}
        ${t(published ? "statusPages.unpublish" : "statusPages.publish")}
      </button>
      <a
        class="btn btn-sm btn-ghost btn-square"
        href=${`${this.base}/status-pages/${p.id}/edit`}
        aria-label=${t("statusPages.edit")}
        >${icon("edit", "size-4")}</a
      ><button
        class="btn btn-sm btn-ghost btn-square"
        aria-label=${t("statusPages.delete")}
        @click=${() => this.askDelete(p)}
      >
        ${icon("trash", "size-4")}
      </button>
    </div>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "status-pages-list-view": StatusPagesListView;
  }
}
