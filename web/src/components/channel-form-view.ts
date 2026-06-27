import { html, nothing, type TemplateResult } from "lit";
import { customElement, property, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { navigate } from "../router.js";
import { toast } from "../toast.js";
import { t, tDynamic } from "../i18n.js";
import { icon } from "../icons.js";
import { pageShell, errorBox, spinner } from "./ui.js";
import type {
  CatalogField,
  Channel,
  ChannelInput,
  ChannelType,
  ChannelTypeCatalogEntry,
  Member,
} from "../api/types.js";

import "./form-field.js";
import "./upsell-banner.js";

// A secret field that is already stored comes back redacted from the API. We do
// not get the value; we get a marker. The form shows "configured" for these and
// leaves the input blank: typing a value replaces the secret, leaving it blank
// keeps the stored one unchanged (the key is omitted from the submitted config).
//
// formValues holds the editable state for every config field as a string (the
// raw input text); stringlist fields hold a string[]. secretCleared records a
// secret the user explicitly emptied so we can send "" to clear it rather than
// silently keeping the old one.

type FieldValue = string | string[];

// One titled form group's parts, packaged before it knows its running number.
interface FormSection {
  title: string;
  lead: string;
  body: TemplateResult;
}

// True when an edit-mode channel already has this secret stored. We accept either
// a "<key>_set" boolean marker or a present non-empty value at the key, mirroring
// however the API chose to redact it.
function secretIsConfigured(config: Record<string, unknown>, key: string): boolean {
  const marker = config[`${key}_set`];
  if (typeof marker === "boolean") return marker;
  const v = config[key];
  return v !== undefined && v !== null && v !== "";
}

// Channel create/edit form (PRD-006). One component for both modes: no channelId
// = create, channelId set (from /channels/:id/edit) = edit (loads the channel).
// The channel-type catalog (getChannelTypes) drives everything: the type picker
// (available types selectable, plan-gated ones disabled with an upsell), and the
// config form, which is rendered field-by-field from the selected type's
// config_fields with no hardcoding. Field labels and help are localized through
// the platform LocalizedString shape (tDynamic), and server field errors render
// under the matching field (RFC-013 section 10.1).
@customElement("channel-form-view")
export class ChannelFormView extends AppElement {
  // set from the route :id param in edit mode
  @property({ type: String }) channelId = "";

  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private catalog: ChannelTypeCatalogEntry[] = [];
  // The org's active members, for the Team-email channel's recipient picker. A
  // memberlist field stores selected user ids; only members can be picked, so a
  // channel can never email outside the org (the server re-checks on save + send).
  @state() private members: Member[] = [];
  @state() private selectedType: ChannelType | null = null;
  @state() private name = "";
  @state() private enabled = true;
  // editable config values keyed by field key
  @state() private values: Record<string, FieldValue> = {};
  // secret fields the edit-mode user explicitly emptied (send "" to clear)
  @state() private secretCleared: Record<string, boolean> = {};
  // which secrets were configured when the channel loaded (drives the hint)
  @state() private configuredSecrets: Record<string, boolean> = {};
  @state() private errors: Record<string, string> = {};
  @state() private submitting = false;
  @state() private loading = false;
  @state() private loadError: string | null = null;

  private loadedKey: string | null = null;

  private get orgId(): string | null {
    return this.ctx?.activeOrg?.org_id ?? null;
  }
  private get base(): string {
    return `/orgs/${this.orgId ?? ""}`;
  }
  private get isEdit(): boolean {
    return this.channelId !== "";
  }
  private get entry(): ChannelTypeCatalogEntry | null {
    return this.catalog.find((e) => e.type === this.selectedType) ?? null;
  }

  override updated(): void {
    const orgId = this.orgId;
    const key = orgId ? `${orgId}:${this.channelId}` : null;
    if (key && key !== this.loadedKey) void this.load();
  }

  private async load(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId) return;
    this.loadedKey = `${orgId}:${this.channelId}`;
    this.loading = true;
    this.loadError = null;
    try {
      // There is no GET /channels/{id} in the spec, so edit mode loads the org's
      // channels and finds this one by id (the catalog is needed regardless).
      const [catalog, channels, members] = await Promise.all([
        client.getChannelTypes(orgId),
        this.isEdit ? client.listChannels(orgId) : Promise.resolve(null),
        client.listMembers(orgId),
      ]);
      this.catalog = catalog.channel_types;
      this.members = members;
      if (channels) {
        const channel = channels.find((c) => c.id === this.channelId);
        if (!channel) throw new ApiError(404, { code: "not_found", message: t("state.notFound") });
        this.applyChannel(channel);
      }
    } catch (err) {
      this.loadError = err instanceof ApiError ? err.message : t("state.error");
    } finally {
      this.loading = false;
    }
  }

  // Seed the form from a loaded channel: non-secret values fill their inputs,
  // secret values stay blank and are flagged configured so the UI shows the
  // "configured / replace" hint.
  private applyChannel(channel: Channel): void {
    this.name = channel.name;
    this.enabled = channel.enabled;
    this.selectedType = channel.type;
    const entry = this.catalog.find((e) => e.type === channel.type);
    const config = channel.config ?? {};
    const values: Record<string, FieldValue> = {};
    const configured: Record<string, boolean> = {};
    for (const field of entry?.config_fields ?? []) {
      if (field.secret) {
        configured[field.key] = secretIsConfigured(config, field.key);
        values[field.key] = (field.type === "stringlist" || field.type === "memberlist") ? [] : "";
        continue;
      }
      values[field.key] = this.coerceLoaded(field, config[field.key]);
    }
    this.values = values;
    this.configuredSecrets = configured;
  }

  // Turn a loaded config value into the field's editable form value.
  private coerceLoaded(field: CatalogField, raw: unknown): FieldValue {
    if ((field.type === "stringlist" || field.type === "memberlist")) {
      return Array.isArray(raw) ? raw.map((v) => String(v)) : [];
    }
    if (field.type === "bool") return raw === true ? "true" : "false";
    if (raw === undefined || raw === null) return field.default ?? "";
    return String(raw);
  }

  // Initial editable value for a field in create mode (or when a type is picked).
  private initialValue(field: CatalogField): FieldValue {
    if ((field.type === "stringlist" || field.type === "memberlist")) return [];
    if (field.type === "bool") return field.default ?? "false";
    return field.default ?? "";
  }

  private selectType(type: ChannelType): void {
    const entry = this.catalog.find((e) => e.type === type);
    if (!entry || !entry.available) return;
    this.selectedType = type;
    this.errors = {};
    this.secretCleared = {};
    this.configuredSecrets = {};
    const values: Record<string, FieldValue> = {};
    for (const field of entry.config_fields) values[field.key] = this.initialValue(field);
    this.values = values;
  }

  private setValue(key: string, value: FieldValue): void {
    this.values = { ...this.values, [key]: value };
  }

  // Build the config payload from the editable values. Secret fields the user
  // left blank are omitted so the stored value is kept; a secret explicitly
  // cleared sends "" to clear it. bool fields go out as real booleans; ints as
  // numbers; everything else as its string.
  private buildConfig(fields: CatalogField[]): Record<string, unknown> {
    const config: Record<string, unknown> = {};
    for (const field of fields) {
      const value = this.values[field.key];
      if (field.secret) {
        const typed = typeof value === "string" ? value : "";
        if (typed !== "") {
          config[field.key] = typed;
        } else if (this.secretCleared[field.key]) {
          config[field.key] = "";
        }
        // blank and not cleared: omit, so the API keeps the stored secret
        continue;
      }
      if ((field.type === "stringlist" || field.type === "memberlist")) {
        config[field.key] = Array.isArray(value) ? value.filter((v) => v !== "") : [];
      } else if (field.type === "bool") {
        config[field.key] = value === "true";
      } else if (field.type === "int") {
        config[field.key] = value === "" ? null : Number(value);
      } else {
        config[field.key] = value;
      }
    }
    return config;
  }

  private validate(fields: CatalogField[]): Record<string, string> {
    const errs: Record<string, string> = {};
    if (!this.name.trim()) errs.name = t("channelForm.errName");
    if (!this.selectedType) errs.type = t("channelForm.errType");
    for (const field of fields) {
      if (!field.required) continue;
      // a configured secret left blank is fine: the stored value satisfies required
      if (field.secret && this.configuredSecrets[field.key] && !this.secretCleared[field.key])
        continue;
      const value = this.values[field.key];
      const empty =
        (field.type === "stringlist" || field.type === "memberlist")
          ? !Array.isArray(value) || value.filter((v) => v !== "").length === 0
          : (typeof value === "string" ? value.trim() : "") === "";
      if (empty) errs[field.key] = t("channelForm.errRequired");
    }
    return errs;
  }

  private async onSubmit(e: Event): Promise<void> {
    e.preventDefault();
    const orgId = this.orgId;
    const entry = this.entry;
    if (!orgId || !this.selectedType || !entry) {
      this.errors = { type: t("channelForm.errType") };
      return;
    }
    const errs = this.validate(entry.config_fields);
    if (Object.keys(errs).length) {
      this.errors = errs;
      return;
    }
    this.errors = {};
    this.submitting = true;
    const input: ChannelInput = {
      name: this.name.trim(),
      type: this.selectedType,
      enabled: this.enabled,
      config: this.buildConfig(entry.config_fields),
    };
    try {
      this.isEdit
        ? await client.updateChannel(orgId, this.channelId, input)
        : await client.createChannel(orgId, input);
      toast(t(this.isEdit ? "channelForm.saved" : "channelForm.created"), "success");
      navigate(`${this.base}/channels`);
    } catch (err) {
      if (err instanceof ApiError && err.code === "validation_failed" && err.fields) {
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

  override render() {
    if (this.loading && this.isEdit && !this.loadError) {
      return html`<div class="flex flex-col gap-4" aria-busy="true">
        <div class="h-9 w-64 bg-paper animate-pulse"></div>
        <div class="h-96 w-full bg-paper animate-pulse"></div>
      </div>`;
    }
    if (this.loadError) {
      return errorBox(this.loadError, () => this.load(), t("state.retry"));
    }

    // basics + type always show; the config group only once a type with fields is
    // picked, so it is appended (and numbered) only when present.
    const groups: FormSection[] = [this.basicsCard(), this.typeCard()];
    const config = this.configCard();
    if (config) groups.push(config);
    return pageShell(
      t(this.isEdit ? "channelForm.editHeading" : "channelForm.createHeading"),
      nothing,
      html`
        <form class="flex flex-col gap-9" @submit=${this.onSubmit} novalidate>
          <div class="flex flex-col gap-8 lg:gap-10">
            ${groups.map((g, i) => this.group(String(i + 1).padStart(2, "0"), g))}
          </div>
          <div class="flex items-center gap-3 border-t border-line pt-6">
            <button class="pulse-btn" type="submit" ?disabled=${this.submitting}>
              ${this.submitting ? spinner() : ""}
              ${t(this.isEdit ? "channelForm.saveChanges" : "channelForm.create")}
            </button>
            <a class="pulse-btn pulse-btn-ghost" href=${`${this.base}/channels`}
              >${t("dialog.cancel")}</a
            >
          </div>
        </form>
      `,
    );
  }

  // section() packages a group's parts; group() renders one with its running index:
  // a numbered, ruled header (mono index + title, lead beneath) over a hairline panel
  // of controls. The config group drops out until a type is picked, so numbering it at
  // render keeps the indices right.
  private section(title: string, lead: string, body: TemplateResult): FormSection {
    return { title, lead, body };
  }

  private group(index: string, s: FormSection) {
    return html`<section class="flex flex-col gap-4">
      <div class="border-b border-line pb-2.5">
        <div class="flex items-baseline gap-3">
          <span class="font-mono text-[12px] text-ink3 tabular-nums">${index}</span>
          <h2 class="pulse-section-title">${s.title}</h2>
        </div>
        ${s.lead ? html`<p class="text-ink3 text-sm mt-1.5">${s.lead}</p>` : nothing}
      </div>
      <div class="pulse-panel p-5 lg:p-6 flex flex-col gap-5">${s.body}</div>
    </section>`;
  }

  private basicsCard() {
    return this.section(
      tDynamic("channelForm.sectionBasics", "Details", {}),
      tDynamic("channelForm.leadBasics", "Name this channel and turn it on.", {}),
      html`
        <form-field
          label=${t("channelForm.name")}
          fieldName="name"
          .error=${this.errors.name ?? null}
          .help=${t("channelForm.helpName")}
          .control=${html`<input
            id="name"
            class="pulse-input w-full"
            maxlength="200"
            .value=${this.name}
            @input=${(e: Event) => (this.name = (e.target as HTMLInputElement).value)}
          />`}
        ></form-field>
        <label class="inline-flex items-center justify-start gap-3 cursor-pointer">
          <input
            type="checkbox"
            class="size-4 accent-brand"
            .checked=${this.enabled}
            @change=${(e: Event) =>
              (this.enabled = (e.target as HTMLInputElement).checked)}
          />
          <span>${t("channelForm.enabled")}</span>
        </label>
      `,
    );
  }

  // The channel-type picker. Available types render as selectable cards; a
  // plan-gated (unavailable) type is dimmed and not selectable, with its
  // localized unavailable_reason shown via an upsell banner.
  private typeCard() {
    // In edit mode the type is fixed; show it as a read-only label.
    if (this.isEdit) {
      return this.section(
        t("channelForm.type"),
        tDynamic("channelForm.leadType", "How notifications reach you.", {}),
        html`<p class="text-ink2">
          ${this.entry?.display_name ?? this.selectedType}
        </p>`,
      );
    }
    return this.section(
      t("channelForm.type"),
      tDynamic("channelForm.leadType", "How notifications reach you.", {}),
      html`
        ${this.errors.type
          ? html`<p class="text-down text-sm" role="alert">${this.errors.type}</p>`
          : ""}
        <div class="grid grid-cols-2 sm:grid-cols-3 gap-2">
          ${this.catalog.map((e) => this.typeOption(e))}
        </div>
        ${this.unavailableNotice()}
      `,
    );
  }

  private typeOption(e: ChannelTypeCatalogEntry) {
    const selected = e.type === this.selectedType;
    if (!e.available) {
      return html`<div
        class="border border-hair p-3 flex items-center justify-between gap-2 opacity-50 cursor-not-allowed"
        aria-disabled="true"
      >
        <span>${e.display_name}</span>
        ${icon("lock", "size-4")}
      </div>`;
    }
    return html`<button
      type="button"
      class="border p-3 flex items-center justify-between gap-2 text-left ${selected
        ? "border-brand bg-brand text-cream"
        : "border-hair hover:border-brand"}"
      @click=${() => this.selectType(e.type)}
    >
      <span>${e.display_name}</span>
      ${selected ? icon("check", "size-4") : ""}
    </button>`;
  }

  // One upsell covering every plan-gated type, not a banner per type (a long list
  // of identical "available on a higher plan" cards reads as noise, especially on
  // mobile). It names the blocked types and links to billing once.
  private unavailableNotice() {
    const blocked = this.catalog.filter((e) => !e.available && e.unavailable_reason);
    if (blocked.length === 0) return "";
    const types = blocked.map((e) => e.display_name).join(", ");
    return html`<upsell-banner
      .message=${tDynamic("channelForm.upsellTypes", `${types} are available on a higher plan.`, { types })}
      .upgradeHref=${`${this.base}/billing`}
    ></upsell-banner>`;
  }

  private configCard(): FormSection | null {
    const entry = this.entry;
    if (!entry) return null;
    if (entry.config_fields.length === 0) return null;
    return this.section(
      t("channelForm.sectionConfig"),
      tDynamic(
        "channelForm.leadConfig",
        "Where this channel sends, and how it signs in.",
        {},
      ),
      html`${entry.config_fields.map((f) => this.configField(f))}`,
    );
  }

  // Render one catalog field with form-field, mapping its type to a control.
  private configField(field: CatalogField) {
    const label = tDynamic(field.label.code, field.label.message, field.label.params)
      + (field.required ? " *" : "");
    const help = field.help
      ? tDynamic(field.help.code, field.help.message, field.help.params)
      : "";
    return html`<form-field
      label=${label}
      fieldName=${field.key}
      .error=${this.errors[field.key] ?? null}
      .help=${help}
      .hint=${this.fieldHint(field)}
      .control=${this.control(field)}
    ></form-field>`;
  }

  // The "configured / leave blank to keep" hint for a stored secret.
  private fieldHint(field: CatalogField): string {
    if (field.secret && this.configuredSecrets[field.key]) {
      return `${t("channelForm.secretConfigured")}. ${t("channelForm.secretReplaceHint")}`;
    }
    if (field.secret) return t("channelForm.secretHint");
    return "";
  }

  private control(field: CatalogField): TemplateResult {
    switch (field.type) {
      case "bool":
        return this.boolControl(field);
      case "enum":
        return this.enumControl(field);
      case "int":
        return this.intControl(field);
      case "stringlist":
        return this.stringListControl(field);
      case "memberlist":
        return this.memberListControl(field);
      default:
        return this.stringControl(field);
    }
  }

  // memberListControl renders a checklist of the org's active members. The stored
  // value is the list of selected user ids; the labels show name + email. Only
  // listed members can be picked, so the recipient set can never include an address
  // outside the org (the server validates the ids again on save and at send).
  private memberListControl(field: CatalogField): TemplateResult {
    const selected = Array.isArray(this.values[field.key])
      ? (this.values[field.key] as string[])
      : [];
    if (this.members.length === 0) {
      return html`<p class="text-ink3 text-sm" id=${field.key}>
        ${t("channelForm.memberListEmpty")}
      </p>`;
    }
    const toggle = (id: string, on: boolean) =>
      this.setValue(
        field.key,
        on ? [...selected, id] : selected.filter((v) => v !== id),
      );
    return html`<div class="flex flex-col gap-1.5" id=${field.key}>
      ${this.members.map(
        (m) => html`<label class="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            class="size-4 accent-brand"
            .checked=${selected.includes(m.user_id)}
            @change=${(e: Event) =>
              toggle(m.user_id, (e.target as HTMLInputElement).checked)}
          />
          <span>${m.name || m.email}</span>
          ${m.name ? html`<span class="text-ink3">${m.email}</span>` : nothing}
        </label>`,
      )}
    </div>`;
  }

  private stringControl(field: CatalogField): TemplateResult {
    const value = typeof this.values[field.key] === "string" ? (this.values[field.key] as string) : "";
    return html`<input
      id=${field.key}
      class="pulse-input w-full"
      type=${field.secret ? "password" : "text"}
      autocomplete=${field.secret ? "new-password" : "off"}
      placeholder=${field.secret && this.configuredSecrets[field.key]
        ? "••••••••"
        : ""}
      .value=${value}
      @input=${(e: Event) => this.onSecretAwareInput(field, (e.target as HTMLInputElement).value)}
    />`;
  }

  private intControl(field: CatalogField): TemplateResult {
    const value = typeof this.values[field.key] === "string" ? (this.values[field.key] as string) : "";
    return html`<input
      id=${field.key}
      class="pulse-input w-full"
      type="number"
      .value=${value}
      @input=${(e: Event) => this.setValue(field.key, (e.target as HTMLInputElement).value)}
    />`;
  }

  private boolControl(field: CatalogField): TemplateResult {
    const on = this.values[field.key] === "true";
    return html`<input
      id=${field.key}
      type="checkbox"
      class="size-4 accent-brand"
      .checked=${on}
      @change=${(e: Event) =>
        this.setValue(field.key, (e.target as HTMLInputElement).checked ? "true" : "false")}
    />`;
  }

  private enumControl(field: CatalogField): TemplateResult {
    const value = typeof this.values[field.key] === "string" ? (this.values[field.key] as string) : "";
    const options = field.enum ?? [];
    return html`<select
      id=${field.key}
      class="pulse-input w-full"
      .value=${value}
      @change=${(e: Event) => this.setValue(field.key, (e.target as HTMLSelectElement).value)}
    >
      ${field.required ? "" : html`<option value=""></option>`}
      ${options.map((o) => html`<option value=${o} ?selected=${o === value}>${o}</option>`)}
    </select>`;
  }

  private stringListControl(field: CatalogField): TemplateResult {
    const list = Array.isArray(this.values[field.key])
      ? (this.values[field.key] as string[])
      : [];
    return html`<div class="flex flex-col gap-2" id=${field.key}>
      ${list.map(
        (item, i) => html`<div class="flex items-center gap-2">
          <input
            class="pulse-input flex-1"
            .value=${item}
            @input=${(e: Event) =>
              this.setValue(
                field.key,
                list.map((v, idx) => (idx === i ? (e.target as HTMLInputElement).value : v)),
              )}
          />
          <button
            type="button"
            class="pulse-iconbtn hover:text-down"
            aria-label=${t("channelForm.removeItem")}
            @click=${() => this.setValue(field.key, list.filter((_, idx) => idx !== i))}
          >
            ${icon("trash", "size-4")}
          </button>
        </div>`,
      )}
      <button
        type="button"
        class="pulse-btn pulse-btn-ghost pulse-btn-sm self-start"
        @click=${() => this.setValue(field.key, [...list, ""])}
      >
        ${icon("plus", "size-4")}${t("channelForm.addItem")}
      </button>
    </div>`;
  }

  // A secret input: typing flags the secret as replaced; clearing it (back to
  // blank) when it was configured flags it as explicitly cleared.
  private onSecretAwareInput(field: CatalogField, value: string): void {
    this.setValue(field.key, value);
    if (!field.secret) return;
    if (value === "" && this.configuredSecrets[field.key]) {
      this.secretCleared = { ...this.secretCleared, [field.key]: true };
    } else if (this.secretCleared[field.key]) {
      const next = { ...this.secretCleared };
      delete next[field.key];
      this.secretCleared = next;
    }
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "channel-form-view": ChannelFormView;
  }
}
