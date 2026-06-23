import { html, type TemplateResult } from "lit";
import { customElement, property, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { navigate } from "../router.js";
import { toast } from "../toast.js";
import { t, tDynamic, type MessageKey } from "../i18n.js";
import { icon, fieldHelp } from "../icons.js";
import type {
  Channel,
  DownPolicy,
  Method,
  MonitorHeader,
  MonitorInput,
  MonitorType,
} from "../api/types.js";

import "./form-field.js";
import "./upsell-banner.js";

const METHODS: Method[] = ["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"];
const TYPES: MonitorType[] = ["http", "ssl"];
const TYPE_LABEL: Record<MonitorType, MessageKey> = {
  http: "monitorForm.typeHttp",
  ssl: "monitorForm.typeSsl",
};
const BODY_METHODS: Method[] = ["POST", "PUT", "PATCH"];
const HARD_MIN_INTERVAL = 30;

// Sane preset check intervals. Values below the plan's floor render as locked
// options with an upsell tooltip rather than being hidden. Labels are localized
// full words ("5 minutes", "1 day"), not abbreviations.
const INTERVAL_OPTIONS: { seconds: number; label: MessageKey }[] = [
  { seconds: 30, label: "monitorForm.int30s" },
  { seconds: 60, label: "monitorForm.int1m" },
  { seconds: 120, label: "monitorForm.int2m" },
  { seconds: 300, label: "monitorForm.int5m" },
  { seconds: 600, label: "monitorForm.int10m" },
  { seconds: 900, label: "monitorForm.int15m" },
  { seconds: 1800, label: "monitorForm.int30m" },
  { seconds: 3600, label: "monitorForm.int1h" },
  { seconds: 7200, label: "monitorForm.int2h" },
  { seconds: 86400, label: "monitorForm.int1d" },
];

function intervalLabel(seconds: number): string {
  const opt = INTERVAL_OPTIONS.find((o) => o.seconds === seconds);
  return opt ? t(opt.label) : `${seconds}s`;
}

const POLICY_LABEL: Record<DownPolicy, MessageKey> = {
  any: "monitorForm.policyAny",
  quorum: "monitorForm.policyQuorum",
  all: "monitorForm.policyAll",
};

// Per-field explanatory text shown via the info icon next to each label.
const FIELD_HELP: Partial<Record<keyof MonitorInput, MessageKey>> = {
  name: "monitorForm.helpName",
  url: "monitorForm.helpUrl",
  method: "monitorForm.helpMethod",
  body: "monitorForm.helpBody",
  expected_status_codes: "monitorForm.helpExpected",
  max_latency_ms: "monitorForm.helpMaxLatency",
  body_contains: "monitorForm.helpBodyContains",
  timeout_seconds: "monitorForm.helpTimeout",
  failure_threshold: "monitorForm.helpFailureThreshold",
  down_policy: "monitorForm.helpDownPolicy",
};

function defaultForm(): MonitorInput {
  return {
    type: "http",
    name: "",
    url: "",
    method: "GET",
    headers: [],
    body: "",
    expected_status_codes: "200",
    timeout_seconds: 10,
    interval_seconds: 60,
    enabled: true,
    max_latency_ms: null,
    body_contains: null,
    failure_threshold: 1,
    notification_channel_ids: [],
    regions: [],
    down_policy: "quorum",
  };
}

// Monitor create/edit form (RFC-013 section 7.1). One component for both modes:
// no monitorId = create, monitorId set (from /monitors/:id/edit) = edit (loads the
// monitor first). Fields and validation mirror PRD-002; the interval floor and
// region set are clamped to the plan's entitlements (RFC-013 section 6.3). Per-
// field server errors (validation_failed envelope) render under each field.
@customElement("monitor-form-view")
export class MonitorFormView extends AppElement {
  // set from the route :id param in edit mode
  @property({ type: String }) monitorId = "";

  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private form: MonitorInput = defaultForm();
  @state() private errors: Record<string, string> = {};
  @state() private channels: Channel[] = [];
  @state() private submitting = false;
  @state() private loading = false;
  @state() private loadError: string | null = null;
  // The over-cap message from a 402 monitor_limit_reached, localized via tDynamic.
  // Set on submit and rendered as an inline upsell with an upgrade link.
  @state() private capMessage: string | null = null;

  private loadedKey: string | null = null;
  // create-mode only: whether we have applied the default first region yet.
  private regionSeeded = false;

  private get orgId(): string | null {
    return this.ctx?.activeOrg?.org_id ?? null;
  }
  private get base(): string {
    return `/orgs/${this.orgId ?? ""}`;
  }
  private get isEdit(): boolean {
    return this.monitorId !== "";
  }
  private get minInterval(): number {
    return Math.max(
      HARD_MIN_INTERVAL,
      this.ctx?.entitlements?.min_interval_seconds ?? HARD_MIN_INTERVAL,
    );
  }
  private get allowedRegions(): string[] {
    // Comes from the plan entitlements. Empty until those load, never a guessed
    // region: the FE renders whatever the backend allows and nothing more.
    return this.ctx?.entitlements?.regions_allowed ?? [];
  }
  private get regionCap(): number {
    return this.ctx?.entitlements?.regions_per_monitor_cap ?? this.allowedRegions.length;
  }

  override updated(): void {
    const orgId = this.orgId;
    const key = orgId ? `${orgId}:${this.monitorId}` : null;
    if (key && key !== this.loadedKey) void this.load();

    // On create, default to the first allowed region once the plan entitlements
    // load (the backend requires at least one region). Seed once so deselecting
    // every region does not silently re-add one.
    if (!this.isEdit && !this.regionSeeded && this.allowedRegions.length > 0) {
      this.regionSeeded = true;
      if (this.form.regions.length === 0) {
        this.patch("regions", [this.allowedRegions[0]]);
      }
    }
  }

  private async load(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId) return;
    this.loadedKey = `${orgId}:${this.monitorId}`;
    this.loading = true;
    this.loadError = null;
    try {
      const [channels, monitor] = await Promise.all([
        client.listChannels(orgId),
        this.isEdit ? client.getMonitor(orgId, this.monitorId) : Promise.resolve(null),
      ]);
      this.channels = channels;
      if (monitor) {
        // copy only the input fields from the loaded monitor
        this.form = {
          type: monitor.type,
          name: monitor.name,
          url: monitor.url,
          method: monitor.method,
          headers: monitor.headers,
          body: monitor.body,
          expected_status_codes: monitor.expected_status_codes,
          timeout_seconds: monitor.timeout_seconds,
          interval_seconds: monitor.interval_seconds,
          enabled: monitor.enabled,
          max_latency_ms: monitor.max_latency_ms,
          body_contains: monitor.body_contains,
          failure_threshold: monitor.failure_threshold,
          notification_channel_ids: monitor.notification_channel_ids,
          regions: monitor.regions,
          down_policy: monitor.down_policy,
        };
      }
    } catch (err) {
      this.loadError = err instanceof ApiError ? err.message : t("state.error");
    } finally {
      this.loading = false;
    }
  }

  private patch<K extends keyof MonitorInput>(key: K, value: MonitorInput[K]): void {
    this.form = { ...this.form, [key]: value };
  }

  private toggleRegion(region: string): void {
    const has = this.form.regions.includes(region);
    if (has) {
      this.patch(
        "regions",
        this.form.regions.filter((r) => r !== region),
      );
    } else if (this.form.regions.length < this.regionCap) {
      this.patch("regions", [...this.form.regions, region]);
    }
  }

  private selectInterval(seconds: number): void {
    this.patch("interval_seconds", seconds);
    // close the daisyUI focus-dropdown
    (document.activeElement as HTMLElement | null)?.blur();
  }

  // Interval as a preset dropdown. Options below the plan floor are shown locked
  // (dimmed, not selectable) with an upsell tooltip on hover (RFC-013 section 6.3).
  private intervalField(f: MonitorInput) {
    const min = this.minInterval;
    return html`
      <fieldset class="fieldset">
        <label class="fieldset-legend inline-flex w-fit items-center gap-1.5"
          >${t("monitorForm.interval")}${fieldHelp(t("monitorForm.helpInterval"))}</label
        >
        <div class="dropdown w-full">
          <div
            tabindex="0"
            role="button"
            class="select w-full flex items-center justify-between"
          >
            <span>${intervalLabel(f.interval_seconds)}</span>
            ${icon("chevronDown", "size-4 opacity-60")}
          </div>
          <ul
            tabindex="0"
            class="dropdown-content menu z-20 w-full mt-1 rounded-box border border-base-300 bg-base-100 shadow-md"
          >
            ${INTERVAL_OPTIONS.map(({ seconds: s }) =>
              s < min
                ? html`<li>
                    <span
                      class="tooltip tooltip-right before:max-w-52 before:whitespace-normal flex items-center justify-between opacity-50 cursor-not-allowed"
                      data-tip=${t("monitorForm.intervalLocked")}
                    >
                      <span>${intervalLabel(s)}</span>
                      ${icon("lock", "size-3.5")}
                    </span>
                  </li>`
                : html`<li>
                    <a
                      class=${s === f.interval_seconds ? "menu-active" : ""}
                      @click=${() => this.selectInterval(s)}
                      >${intervalLabel(s)}</a
                    >
                  </li>`,
            )}
          </ul>
        </div>
        ${this.errors.interval_seconds
          ? html`<p class="fieldset-label text-error">
              ${this.errors.interval_seconds}
            </p>`
          : html`<p class="fieldset-label text-base-content/60">
              ${t("monitorForm.planMin")}: ${intervalLabel(min)}
            </p>`}
      </fieldset>
    `;
  }

  private toggleChannel(id: string): void {
    const has = this.form.notification_channel_ids.includes(id);
    this.patch(
      "notification_channel_ids",
      has
        ? this.form.notification_channel_ids.filter((c) => c !== id)
        : [...this.form.notification_channel_ids, id],
    );
  }

  // --- headers editor ---
  private addHeader(): void {
    this.patch("headers", [...this.form.headers, { key: "", value: "", secret: false }]);
  }
  private removeHeader(i: number): void {
    this.patch("headers", this.form.headers.filter((_, idx) => idx !== i));
  }
  private updateHeader(i: number, patch: Partial<MonitorHeader>): void {
    this.patch(
      "headers",
      this.form.headers.map((h, idx) => (idx === i ? { ...h, ...patch } : h)),
    );
  }

  private validate(): Record<string, string> {
    const errs: Record<string, string> = {};
    if (!this.form.name.trim()) errs.name = t("monitorForm.errName");
    const url = this.form.url.trim();
    if (this.form.type === "ssl") {
      // a host (with or without scheme/port); reject empty or whitespace
      if (!url || /\s/.test(url)) errs.url = t("monitorForm.errHost");
    } else if (!/^https?:\/\/.+/i.test(url)) {
      errs.url = t("monitorForm.errUrl");
    }
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
        ? await client.updateMonitor(orgId, this.monitorId, this.form)
        : await client.createMonitor(orgId, this.form);
      toast(t(this.isEdit ? "monitorForm.saved" : "monitorForm.created"), "success");
      navigate(`${this.base}/monitors/${result.id}`);
    } catch (err) {
      if (err instanceof ApiError && err.status === 402) {
        // over the plan's monitor cap: show the upsell inline (with the cap from
        // params/fields) rather than a transient toast, so the upgrade link stays.
        this.capMessage = tDynamic(
          err.code,
          err.message || t("monitorForm.capReached"),
          err.params,
        );
      } else if (err instanceof ApiError && err.code === "validation_failed" && err.fields) {
        // per-field server errors (e.g. a sub-floor interval_seconds) render under
        // their field; the interval shows it below the preset dropdown.
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

  // a labelled control with its per-field error, via <form-field>
  private field(
    name: keyof MonitorInput,
    labelKey: MessageKey,
    control: TemplateResult,
    hint = "",
  ) {
    const helpKey = FIELD_HELP[name];
    return html`<form-field
      label=${t(labelKey)}
      fieldName=${name}
      .error=${this.errors[name] ?? null}
      .hint=${hint}
      .help=${helpKey ? t(helpKey) : ""}
      .control=${control}
    ></form-field>`;
  }

  override render() {
    // Only block on the skeleton while loading an existing monitor (edit). In
    // create mode the form shows immediately and channels populate in the
    // background, so there is no empty-form -> skeleton -> form flash.
    if (this.loading && this.isEdit && !this.loadError) {
      return html`<div class="flex flex-col gap-4" aria-busy="true">
        <div class="skeleton h-9 w-64"></div>
        <div class="skeleton h-96 w-full"></div>
      </div>`;
    }
    if (this.loadError) {
      return html`<div role="alert" class="alert alert-error">
        <span>${this.loadError}</span>
        <button class="btn btn-sm" @click=${() => this.load()}>
          ${t("state.retry")}
        </button>
      </div>`;
    }

    const f = this.form;
    return html`
      <form class="flex flex-col gap-6 max-w-3xl" @submit=${this.onSubmit} novalidate>
        <h1 class="text-2xl font-bold">
          ${t(this.isEdit ? "monitorForm.editHeading" : "monitorForm.createHeading")}
        </h1>

        ${this.capMessage
          ? html`<upsell-banner
              .message=${this.capMessage}
              .upgradeHref=${`${this.base}/billing`}
            ></upsell-banner>`
          : ""}
        ${this.requestCard(f)}
        ${f.type === "ssl"
          ? ""
          : html`${this.assertionsCard(f)} ${this.schedulingCard(f)}`}
        ${this.notifyCard(f)}

        <div class="flex items-center gap-2">
          <button class="btn btn-primary" type="submit" ?disabled=${this.submitting}>
            ${this.submitting
              ? html`<span class="loading loading-spinner loading-sm"></span>`
              : ""}
            ${t(this.isEdit ? "monitorForm.saveChanges" : "monitorForm.create")}
          </button>
          <a class="btn btn-ghost" href=${this.isEdit
            ? `${this.base}/monitors/${this.monitorId}`
            : this.base}
            >${t("dialog.cancel")}</a
          >
        </div>
      </form>
    `;
  }

  private card(titleKey: MessageKey, body: TemplateResult) {
    return html`<div class="card bg-base-100 border border-base-300 shadow-sm">
      <div class="card-body gap-4 p-5">
        <h2 class="font-semibold">${t(titleKey)}</h2>
        ${body}
      </div>
    </div>`;
  }

  // The check-type picker. Switching to ssl hides the http-only request and
  // assertion fields; the cert thresholds are fixed, so the form just explains them.
  private typeField(f: MonitorInput) {
    return this.field(
      "type",
      "monitorForm.type",
      html`<select
        id="type"
        class="select w-full"
        .value=${f.type}
        @change=${(e: Event) =>
          this.patch("type", (e.target as HTMLSelectElement).value as MonitorType)}
      >
        ${TYPES.map((tpe) => html`<option value=${tpe}>${t(TYPE_LABEL[tpe])}</option>`)}
      </select>`,
    );
  }

  private requestCard(f: MonitorInput) {
    const isSSL = f.type === "ssl";
    return this.card(
      "monitorForm.sectionRequest",
      html`
        ${this.typeField(f)}
        ${this.field(
          "name",
          "monitorForm.name",
          html`<input
            id="name"
            class="input w-full"
            maxlength="200"
            .value=${f.name}
            @input=${(e: Event) => this.patch("name", (e.target as HTMLInputElement).value)}
          />`,
        )}
        ${this.field(
          "url",
          isSSL ? "monitorForm.host" : "monitorForm.url",
          html`<input
            id="url"
            type=${isSSL ? "text" : "url"}
            class="input w-full"
            placeholder=${isSSL ? "example.com" : "https://example.com"}
            .value=${f.url}
            @input=${(e: Event) => this.patch("url", (e.target as HTMLInputElement).value)}
          />`,
          isSSL ? t("monitorForm.hostHint") : "",
        )}
        ${isSSL
          ? html`<div role="note" class="alert alert-info alert-soft text-sm">
              <span>${t("monitorForm.sslNotifyInfo")}</span>
            </div>`
          : html`
              <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
                ${this.field(
                  "method",
                  "monitorForm.method",
                  html`<select
                    id="method"
                    class="select w-full"
                    .value=${f.method}
                    @change=${(e: Event) =>
                      this.patch("method", (e.target as HTMLSelectElement).value as Method)}
                  >
                    ${METHODS.map((m) => html`<option value=${m}>${m}</option>`)}
                  </select>`,
                )}
              </div>
              ${BODY_METHODS.includes(f.method)
                ? this.field(
                    "body",
                    "monitorForm.body",
                    html`<textarea
                      id="body"
                      class="textarea w-full font-mono"
                      rows="4"
                      .value=${f.body}
                      @input=${(e: Event) =>
                        this.patch("body", (e.target as HTMLTextAreaElement).value)}
                    ></textarea>`,
                  )
                : ""}
              ${this.headersEditor(f)}
            `}
      `,
    );
  }

  private headersEditor(f: MonitorInput) {
    return html`
      <fieldset class="fieldset">
        <label class="fieldset-legend inline-flex w-fit items-center gap-1.5"
          >${t("monitorForm.headers")}${fieldHelp(t("monitorForm.helpHeaders"))}</label
        >
        <div class="flex flex-col gap-2">
          ${f.headers.map(
            (h, i) => html`<div class="flex items-center gap-2">
              <input
                class="input input-sm flex-1"
                placeholder=${t("monitorForm.headerKey")}
                .value=${h.key}
                @input=${(e: Event) =>
                  this.updateHeader(i, { key: (e.target as HTMLInputElement).value })}
              />
              <input
                class="input input-sm flex-1"
                placeholder=${t("monitorForm.headerValue")}
                .value=${h.value ?? ""}
                @input=${(e: Event) =>
                  this.updateHeader(i, { value: (e.target as HTMLInputElement).value })}
              />
              <label class="label gap-1 text-xs cursor-pointer">
                <input
                  type="checkbox"
                  class="checkbox checkbox-sm"
                  .checked=${h.secret}
                  @change=${(e: Event) =>
                    this.updateHeader(i, {
                      secret: (e.target as HTMLInputElement).checked,
                    })}
                />${t("monitorForm.headerSecret")}
              </label>
              <button
                type="button"
                class="btn btn-sm btn-ghost btn-square"
                @click=${() => this.removeHeader(i)}
              >
                ${icon("trash", "size-4")}
              </button>
            </div>`,
          )}
          <button type="button" class="btn btn-sm btn-ghost self-start gap-1.5" @click=${this.addHeader}>
            ${icon("plus", "size-4")}${t("monitorForm.addHeader")}
          </button>
        </div>
      </fieldset>
    `;
  }

  private assertionsCard(f: MonitorInput) {
    return this.card(
      "monitorForm.sectionAssertions",
      html`
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
          ${this.field(
            "expected_status_codes",
            "monitorForm.expectedCodes",
            html`<input
              id="expected_status_codes"
              class="input w-full"
              .value=${f.expected_status_codes}
              @input=${(e: Event) =>
                this.patch("expected_status_codes", (e.target as HTMLInputElement).value)}
            />`,
            t("monitorForm.expectedCodesHint"),
          )}
          ${this.field(
            "max_latency_ms",
            "monitorForm.maxLatency",
            html`<input
              id="max_latency_ms"
              type="number"
              min="1"
              class="input w-full"
              .value=${f.max_latency_ms === null ? "" : String(f.max_latency_ms)}
              @input=${(e: Event) => {
                const v = (e.target as HTMLInputElement).value;
                this.patch("max_latency_ms", v === "" ? null : Number(v));
              }}
            />`,
            t("monitorForm.optional"),
          )}
        </div>
        ${this.field(
          "body_contains",
          "monitorForm.bodyContains",
          html`<input
            id="body_contains"
            class="input w-full"
            .value=${f.body_contains ?? ""}
            @input=${(e: Event) => {
              const v = (e.target as HTMLInputElement).value;
              this.patch("body_contains", v === "" ? null : v);
            }}
          />`,
          t("monitorForm.optional"),
        )}
      `,
    );
  }

  private schedulingCard(f: MonitorInput) {
    return this.card(
      "monitorForm.sectionScheduling",
      html`
        <div class="grid grid-cols-1 sm:grid-cols-3 gap-4">
          ${this.field(
            "timeout_seconds",
            "monitorForm.timeout",
            html`<input
              id="timeout_seconds"
              type="number"
              min="1"
              max="60"
              class="input w-full"
              .value=${String(f.timeout_seconds)}
              @input=${(e: Event) =>
                this.patch("timeout_seconds", Number((e.target as HTMLInputElement).value))}
            />`,
          )}
          ${this.intervalField(f)}
          ${this.field(
            "failure_threshold",
            "monitorForm.failureThreshold",
            html`<input
              id="failure_threshold"
              type="number"
              min="1"
              class="input w-full"
              .value=${String(f.failure_threshold)}
              @input=${(e: Event) =>
                this.patch("failure_threshold", Number((e.target as HTMLInputElement).value))}
            />`,
          )}
        </div>
        <fieldset class="fieldset">
          <label class="fieldset-legend inline-flex w-fit items-center gap-1.5"
            >${t("monitorForm.regions")} (${f.regions.length}/${this.regionCap})${fieldHelp(
              t("monitorForm.helpRegions"),
            )}</label
          >
          <div class="flex flex-wrap gap-2">
            ${this.allowedRegions.map((region) => {
              const on = f.regions.includes(region);
              const disabled = !on && f.regions.length >= this.regionCap;
              return html`<label
                class="btn btn-sm ${on ? "btn-primary" : "btn-outline"} ${disabled
                  ? "btn-disabled"
                  : ""}"
              >
                <input
                  type="checkbox"
                  class="hidden"
                  .checked=${on}
                  ?disabled=${disabled}
                  @change=${() => this.toggleRegion(region)}
                />${region}
              </label>`;
            })}
          </div>
          ${this.errors.regions
            ? html`<p class="fieldset-label text-error">${this.errors.regions}</p>`
            : ""}
        </fieldset>
        ${this.field(
          "down_policy",
          "monitorForm.downPolicy",
          html`<select
            id="down_policy"
            class="select w-full"
            .value=${f.down_policy}
            @change=${(e: Event) =>
              this.patch("down_policy", (e.target as HTMLSelectElement).value as DownPolicy)}
          >
            ${(["any", "quorum", "all"] as DownPolicy[]).map(
              (p) => html`<option value=${p}>${t(POLICY_LABEL[p])}</option>`,
            )}
          </select>`,
        )}
      `,
    );
  }

  private notifyCard(f: MonitorInput) {
    return this.card(
      "monitorForm.sectionNotify",
      html`
        <fieldset class="fieldset">
          <label class="fieldset-legend inline-flex w-fit items-center gap-1.5"
            >${t("monitorForm.channels")}${fieldHelp(t("monitorForm.helpChannels"))}</label
          >
          ${this.channels.length === 0
            ? html`<p class="text-base-content/60 text-sm">${t("monitorForm.noChannels")}</p>`
            : html`<div class="flex flex-col gap-2">
                ${this.channels.map(
                  (c) => html`<label class="label cursor-pointer justify-start gap-2">
                    <input
                      type="checkbox"
                      class="checkbox checkbox-sm"
                      .checked=${f.notification_channel_ids.includes(c.id)}
                      @change=${() => this.toggleChannel(c.id)}
                    />
                    <span>${c.name}</span>
                    <span class="badge badge-ghost badge-sm">${c.type}</span>
                  </label>`,
                )}
              </div>`}
        </fieldset>
        <label class="label cursor-pointer justify-start gap-3">
          <input
            type="checkbox"
            class="toggle toggle-sm"
            .checked=${f.enabled}
            @change=${(e: Event) =>
              this.patch("enabled", (e.target as HTMLInputElement).checked)}
          />
          <span class="inline-flex items-center gap-1.5"
            >${t("monitorForm.enabled")}${fieldHelp(t("monitorForm.helpEnabled"))}</span
          >
        </label>
      `,
    );
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "monitor-form-view": MonitorFormView;
  }
}
