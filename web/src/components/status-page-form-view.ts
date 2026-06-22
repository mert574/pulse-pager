import { html, type TemplateResult } from "lit";
import { customElement, property, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { navigate } from "../router.js";
import { toast } from "../toast.js";
import { t, tDynamic, type MessageKey } from "../i18n.js";
import { icon } from "../icons.js";
import { publicStatusUrl } from "./status-page-url.js";
import type {
  MonitorListItem,
  StatusPageInput,
  StatusPageMonitorEntry,
  StatusPageTheme,
} from "../api/types.js";

import "./form-field.js";
import "./upsell-banner.js";

const THEMES: StatusPageTheme[] = ["light", "dark"];
const THEME_LABEL: Record<StatusPageTheme, MessageKey> = {
  light: "statusPageForm.themeLight",
  dark: "statusPageForm.themeDark",
};

// lowercase letters, numbers, dashes; no leading/trailing dash
const SLUG_RE = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;

function defaultForm(): StatusPageInput {
  return {
    name: "",
    slug: "",
    logo_url: "",
    accent_color: "#2563eb",
    theme: "light",
    display_monitors: [],
  };
}

// Status page create/edit form (PRD-004). One component for both modes: no
// statusPageId = create, set (from /status-pages/:id/edit) = edit (loads first).
// It edits the page name, slug, branding (logo, accent, theme) and the displayed
// monitors (a multi-select of the org's monitors, each with an editable public
// display name and an order). A publish toggle is shown in edit mode; on create
// the page starts as a draft and publish happens from the list or after saving.
// Client validation runs first; server per-field errors (validation_failed) and a
// 402 status-page cap both surface (the cap as an inline upsell).
@customElement("status-page-form-view")
export class StatusPageFormView extends AppElement {
  // set from the route :id param in edit mode
  @property({ type: String }) statusPageId = "";

  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private form: StatusPageInput = defaultForm();
  @state() private errors: Record<string, string> = {};
  @state() private monitors: MonitorListItem[] = [];
  @state() private submitting = false;
  @state() private loading = false;
  @state() private loadError: string | null = null;
  // current published state in edit mode, toggled independently of the form save
  @state() private published = false;
  @state() private togglingPublish = false;
  // a 402 status_page_limit_reached message, rendered as an inline upsell
  @state() private capMessage: string | null = null;

  private loadedKey: string | null = null;

  private get orgId(): string | null {
    return this.ctx?.activeOrg?.org_id ?? null;
  }
  private get base(): string {
    return `/orgs/${this.orgId ?? ""}`;
  }
  private get isEdit(): boolean {
    return this.statusPageId !== "";
  }

  override updated(): void {
    const orgId = this.orgId;
    const key = orgId ? `${orgId}:${this.statusPageId}` : null;
    if (key && key !== this.loadedKey) void this.load();
  }

  private async load(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId) return;
    this.loadedKey = `${orgId}:${this.statusPageId}`;
    this.loading = true;
    this.loadError = null;
    try {
      const [monitors, page] = await Promise.all([
        client.listMonitors(orgId),
        this.isEdit ? client.getStatusPage(orgId, this.statusPageId) : Promise.resolve(null),
      ]);
      this.monitors = monitors;
      if (page) {
        this.form = {
          name: page.name,
          slug: page.slug,
          logo_url: page.logo_url,
          accent_color: page.accent_color,
          theme: page.theme,
          display_monitors: [...page.display_monitors]
            .sort((a, b) => a.order - b.order)
            .map((m) => ({ ...m })),
        };
        this.published = page.state === "published";
      }
    } catch (err) {
      this.loadError = err instanceof ApiError ? err.message : t("state.error");
    } finally {
      this.loading = false;
    }
  }

  private patch<K extends keyof StatusPageInput>(key: K, value: StatusPageInput[K]): void {
    this.form = { ...this.form, [key]: value };
  }

  // --- displayed-monitor list ---

  private isSelected(monitorId: string): boolean {
    return this.form.display_monitors.some((m) => m.monitor_id === monitorId);
  }

  private toggleMonitor(monitor: MonitorListItem): void {
    if (this.isSelected(monitor.id)) {
      const kept = this.form.display_monitors.filter((m) => m.monitor_id !== monitor.id);
      this.patch("display_monitors", this.reindex(kept));
    } else {
      const next: StatusPageMonitorEntry = {
        monitor_id: monitor.id,
        display_name: monitor.name,
        order: this.form.display_monitors.length,
      };
      this.patch("display_monitors", [...this.form.display_monitors, next]);
    }
  }

  private setDisplayName(monitorId: string, name: string): void {
    this.patch(
      "display_monitors",
      this.form.display_monitors.map((m) =>
        m.monitor_id === monitorId ? { ...m, display_name: name } : m,
      ),
    );
  }

  private move(index: number, delta: number): void {
    const list = [...this.form.display_monitors];
    const target = index + delta;
    if (target < 0 || target >= list.length) return;
    [list[index], list[target]] = [list[target], list[index]];
    this.patch("display_monitors", this.reindex(list));
  }

  // keep order contiguous (0..n-1) in list position after add/remove/move
  private reindex(list: StatusPageMonitorEntry[]): StatusPageMonitorEntry[] {
    return list.map((m, i) => ({ ...m, order: i }));
  }

  private monitorName(monitorId: string): string {
    return this.monitors.find((m) => m.id === monitorId)?.name ?? monitorId;
  }

  private validate(): Record<string, string> {
    const errs: Record<string, string> = {};
    if (!this.form.name.trim()) errs.name = t("statusPageForm.errName");
    if (!SLUG_RE.test(this.form.slug.trim())) errs.slug = t("statusPageForm.errSlug");
    const logo = this.form.logo_url.trim();
    if (logo && !/^https:\/\/.+/i.test(logo)) errs.logo_url = t("statusPageForm.errLogoUrl");
    return errs;
  }

  private async onSubmit(e: Event): Promise<void> {
    e.preventDefault();
    const orgId = this.orgId;
    if (!orgId) return;
    const errs = this.validate();
    if (Object.keys(errs).length) {
      this.errors = errs;
      return;
    }
    this.errors = {};
    this.capMessage = null;
    this.submitting = true;
    try {
      const result = this.isEdit
        ? await client.updateStatusPage(orgId, this.statusPageId, this.form)
        : await client.createStatusPage(orgId, this.form);
      toast(t(this.isEdit ? "statusPageForm.saved" : "statusPageForm.created"), "success");
      navigate(`${this.base}/status-pages/${result.id}/edit`);
    } catch (err) {
      if (err instanceof ApiError && err.status === 402) {
        this.capMessage = tDynamic(
          err.code,
          err.message || t("statusPageForm.capReached"),
          err.params,
        );
      } else if (err instanceof ApiError && err.code === "validation_failed" && err.fields) {
        this.errors = err.fields;
      } else if (err instanceof ApiError) {
        toast(err.message, "error");
      } else {
        toast(t("state.error"), "error");
      }
    } finally {
      this.submitting = false;
    }
  }

  private async onTogglePublish(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId || !this.isEdit) return;
    const next = !this.published;
    this.togglingPublish = true;
    try {
      const updated = await client.publishStatusPage(orgId, this.statusPageId, next);
      this.published = updated.state === "published";
      toast(t(this.published ? "statusPages.published" : "statusPages.unpublished"), "success");
    } catch (err) {
      toast(err instanceof ApiError ? err.message : t("state.error"), "error");
    } finally {
      this.togglingPublish = false;
    }
  }

  private async copyUrl(): Promise<void> {
    try {
      await navigator.clipboard.writeText(publicStatusUrl(this.form.slug));
      toast(t("statusPageForm.copied"), "success");
    } catch {
      // clipboard blocked: the URL is still shown for manual copy
    }
  }

  private field(
    name: keyof StatusPageInput,
    labelKey: MessageKey,
    helpKey: MessageKey,
    control: TemplateResult,
    hint = "",
  ) {
    return html`<form-field
      label=${t(labelKey)}
      fieldName=${name}
      .error=${this.errors[name] ?? null}
      .hint=${hint}
      .help=${t(helpKey)}
      .control=${control}
    ></form-field>`;
  }

  private card(titleKey: MessageKey, body: TemplateResult) {
    return html`<div class="card bg-base-100 border border-base-300 shadow-sm">
      <div class="card-body gap-4 p-5">
        <h2 class="font-semibold">${t(titleKey)}</h2>
        ${body}
      </div>
    </div>`;
  }

  override render() {
    if (this.loading && this.isEdit && !this.loadError) {
      return html`<div class="flex flex-col gap-4" aria-busy="true">
        <div class="skeleton h-9 w-64"></div>
        <div class="skeleton h-96 w-full"></div>
      </div>`;
    }
    if (this.loadError) {
      return html`<div role="alert" class="alert alert-error">
        <span>${this.loadError}</span>
        <button class="btn btn-sm" @click=${() => this.load()}>${t("state.retry")}</button>
      </div>`;
    }

    return html`
      <form class="flex flex-col gap-6 max-w-3xl" @submit=${this.onSubmit} novalidate>
        <h1 class="text-2xl font-bold">
          ${t(this.isEdit ? "statusPageForm.editHeading" : "statusPageForm.createHeading")}
        </h1>

        ${this.capMessage
          ? html`<upsell-banner
              .message=${this.capMessage}
              .upgradeHref=${`${this.base}/billing`}
            ></upsell-banner>`
          : ""}
        ${this.generalCard()} ${this.brandingCard()} ${this.monitorsCard()}
        ${this.isEdit ? this.publishCard() : ""}

        <div class="flex items-center gap-2">
          <button class="btn btn-primary" type="submit" ?disabled=${this.submitting}>
            ${this.submitting
              ? html`<span class="loading loading-spinner loading-sm"></span>`
              : ""}
            ${t(this.isEdit ? "statusPageForm.saveChanges" : "statusPageForm.create")}
          </button>
          <a class="btn btn-ghost" href=${`${this.base}/status-pages`}
            >${t("dialog.cancel")}</a
          >
        </div>
      </form>
    `;
  }

  private generalCard() {
    const f = this.form;
    return this.card(
      "statusPageForm.sectionGeneral",
      html`
        ${this.field(
          "name",
          "statusPageForm.name",
          "statusPageForm.helpName",
          html`<input
            id="name"
            class="input w-full"
            maxlength="200"
            .value=${f.name}
            @input=${(e: Event) => this.patch("name", (e.target as HTMLInputElement).value)}
          />`,
        )}
        ${this.field(
          "slug",
          "statusPageForm.slug",
          "statusPageForm.helpSlug",
          html`<input
            id="slug"
            class="input w-full"
            .value=${f.slug}
            @input=${(e: Event) =>
              this.patch("slug", (e.target as HTMLInputElement).value.toLowerCase())}
          />`,
        )}
        ${f.slug && SLUG_RE.test(f.slug) ? this.publicUrlRow() : ""}
      `,
    );
  }

  private publicUrlRow() {
    const url = publicStatusUrl(this.form.slug);
    return html`<div class="flex flex-col gap-1">
      <span class="text-sm text-base-content/60">${t("statusPageForm.publicUrl")}</span>
      <div class="flex items-center gap-2">
        <a
          class="link link-hover text-sm truncate"
          href=${url}
          target="_blank"
          rel="noopener noreferrer"
          >${url}</a
        >
        <button
          type="button"
          class="btn btn-xs btn-ghost gap-1"
          aria-label=${t("statusPageForm.copyUrl")}
          @click=${this.copyUrl}
        >
          ${icon("copy", "size-3.5")}
        </button>
      </div>
    </div>`;
  }

  private brandingCard() {
    const f = this.form;
    return this.card(
      "statusPageForm.sectionBranding",
      html`
        ${this.field(
          "logo_url",
          "statusPageForm.logoUrl",
          "statusPageForm.helpLogoUrl",
          html`<input
            id="logo_url"
            type="url"
            class="input w-full"
            placeholder="https://"
            .value=${f.logo_url}
            @input=${(e: Event) =>
              this.patch("logo_url", (e.target as HTMLInputElement).value)}
          />`,
        )}
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
          ${this.field(
            "accent_color",
            "statusPageForm.accentColor",
            "statusPageForm.helpAccentColor",
            html`<div class="flex items-center gap-2">
              <input
                id="accent_color"
                type="color"
                class="h-10 w-14 rounded border border-base-300 bg-base-100"
                .value=${f.accent_color}
                @input=${(e: Event) =>
                  this.patch("accent_color", (e.target as HTMLInputElement).value)}
              />
              <input
                class="input flex-1"
                .value=${f.accent_color}
                @input=${(e: Event) =>
                  this.patch("accent_color", (e.target as HTMLInputElement).value)}
              />
            </div>`,
          )}
          ${this.field(
            "theme",
            "statusPageForm.theme",
            "statusPageForm.helpAccentColor",
            html`<select
              id="theme"
              class="select w-full"
              .value=${f.theme}
              @change=${(e: Event) =>
                this.patch("theme", (e.target as HTMLSelectElement).value as StatusPageTheme)}
            >
              ${THEMES.map(
                (th) => html`<option value=${th}>${t(THEME_LABEL[th])}</option>`,
              )}
            </select>`,
          )}
        </div>
      `,
    );
  }

  private monitorsCard() {
    return this.card(
      "statusPageForm.sectionMonitors",
      html`
        <p class="text-sm text-base-content/60">${t("statusPageForm.monitorsHint")}</p>
        ${this.monitors.length === 0
          ? html`<p class="text-base-content/60 text-sm">${t("statusPageForm.noMonitors")}</p>`
          : html`<div class="flex flex-col gap-2">
              ${this.monitors.map(
                (m) => html`<label class="label cursor-pointer justify-start gap-2">
                  <input
                    type="checkbox"
                    class="checkbox checkbox-sm"
                    .checked=${this.isSelected(m.id)}
                    @change=${() => this.toggleMonitor(m)}
                  />
                  <span>${m.name}</span>
                </label>`,
              )}
            </div>`}
        ${this.form.display_monitors.length
          ? html`<div class="flex flex-col gap-2 border-t border-base-300 pt-3">
              ${this.form.display_monitors.map((entry, i) => this.selectedRow(entry, i))}
            </div>`
          : ""}
      `,
    );
  }

  private selectedRow(entry: StatusPageMonitorEntry, index: number) {
    const last = this.form.display_monitors.length - 1;
    return html`<div class="flex items-center gap-2">
      <span class="text-xs text-base-content/50 w-32 truncate" title=${this.monitorName(entry.monitor_id)}
        >${this.monitorName(entry.monitor_id)}</span
      >
      <input
        class="input input-sm flex-1"
        placeholder=${t("statusPageForm.displayName")}
        aria-label=${t("statusPageForm.displayName")}
        .value=${entry.display_name}
        @input=${(e: Event) =>
          this.setDisplayName(entry.monitor_id, (e.target as HTMLInputElement).value)}
      />
      <button
        type="button"
        class="btn btn-sm btn-ghost btn-square"
        aria-label=${t("statusPageForm.moveUp")}
        ?disabled=${index === 0}
        @click=${() => this.move(index, -1)}
      >
        ${icon("chevronUp", "size-4")}
      </button>
      <button
        type="button"
        class="btn btn-sm btn-ghost btn-square"
        aria-label=${t("statusPageForm.moveDown")}
        ?disabled=${index === last}
        @click=${() => this.move(index, 1)}
      >
        ${icon("chevronDown", "size-4")}
      </button>
    </div>`;
  }

  private publishCard() {
    return this.card(
      "statusPageForm.published",
      html`
        <label class="label cursor-pointer justify-start gap-3">
          <input
            type="checkbox"
            class="toggle toggle-sm"
            .checked=${this.published}
            ?disabled=${this.togglingPublish}
            @change=${this.onTogglePublish}
          />
          <span>${t("statusPageForm.published")}</span>
          ${this.togglingPublish
            ? html`<span class="loading loading-spinner loading-xs"></span>`
            : ""}
        </label>
        <p class="text-sm text-base-content/60">${t("statusPageForm.helpPublished")}</p>
      `,
    );
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "status-page-form-view": StatusPageFormView;
  }
}
