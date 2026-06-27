import { html } from "lit";
import { customElement, property, state, query } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { can } from "../state/can.js";
import { session } from "../state/session.js";
import { t, type MessageKey } from "../i18n.js";
import { toast, toastError } from "../toast.js";
import { formatDuration } from "../format.js";
import type {
  FailureReason,
  IncidentAnnotation,
  IncidentDetail,
} from "../api/types.js";
import { icon } from "../icons.js";
import { errorBox, spinner } from "./ui.js";
import type { ConfirmDialog } from "./confirm-dialog.js";

import "./status-badge.js";
import "./confirm-dialog.js";
import "./form-field.js";
import "./relative-time.js";

const FAILURE_LABEL: Record<FailureReason, MessageKey> = {
  connection_error: "failure.connection_error",
  timeout: "failure.timeout",
  status_mismatch: "failure.status_mismatch",
  latency_exceeded: "failure.latency_exceeded",
  body_assertion_failed: "failure.body_assertion_failed",
  blocked_target: "failure.blocked_target",
  cert_expired: "failure.cert_expired",
  cert_expiring_soon: "failure.cert_expiring_soon",
  cert_invalid: "failure.cert_invalid",
};

const CLOSE_REASON_LABEL: Record<
  NonNullable<IncidentDetail["close_reason"]>,
  MessageKey
> = {
  recovered: "incident.closeRecovered",
  disabled: "incident.closeDisabled",
  manual: "incident.closeManual",
};

// Incident detail (PRD-002 4). Shows the incident header (monitor, started/ended,
// duration, cause, close reason), the annotation timeline, an "Add note" form
// (member+) that posts and appends, and a "Close incident" action (owner/admin
// only via can(), confirmed, with the already-closed 409 surfaced). The annotation
// wire shape carries author_user_id only, so the current user's notes are tagged
// "You" and others show the id.
@customElement("incident-detail-view")
export class IncidentDetailView extends AppElement {
  // set from the route :id param
  @property({ type: String }) incidentId = "";

  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private incident: IncidentDetail | null = null;
  @state() private monitorName: string | null = null;
  @state() private loading = false;
  @state() private error: string | null = null;

  @state() private note = "";
  @state() private adding = false;
  @state() private closing = false;

  @query("confirm-dialog") private closeDialog!: ConfirmDialog;

  private loadedKey: string | null = null;

  private get orgId(): string | null {
    return this.ctx?.activeOrg?.org_id ?? null;
  }

  private get base(): string {
    return `/orgs/${this.orgId ?? ""}`;
  }

  override updated(): void {
    const orgId = this.orgId;
    const key = orgId && this.incidentId ? `${orgId}:${this.incidentId}` : null;
    if (key && key !== this.loadedKey) void this.load();
  }

  private async load(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId || !this.incidentId) return;
    this.loadedKey = `${orgId}:${this.incidentId}`;
    this.loading = true;
    this.error = null;
    try {
      const incident = await client.getIncident(orgId, this.incidentId);
      this.incident = incident;
      // resolve the monitor name for the header; a failed lookup just leaves the
      // id-only fallback, so it does not block the rest of the page.
      try {
        const monitor = await client.getMonitor(orgId, incident.monitor_id);
        this.monitorName = monitor.name;
      } catch {
        this.monitorName = null;
      }
    } catch (err) {
      this.error = err instanceof ApiError ? err.message : t("state.error");
    } finally {
      this.loading = false;
    }
  }

  private get headerName(): string {
    return this.monitorName ?? this.incident?.monitor_id ?? "";
  }

  // --- add note (member+) ---

  private async onAddNote(e: Event): Promise<void> {
    e.preventDefault();
    const orgId = this.orgId;
    if (!orgId || !this.incident || this.adding) return;
    const note = this.note.trim();
    if (!note) return;
    this.adding = true;
    try {
      const created = await client.addIncidentAnnotation(
        orgId,
        this.incident.id,
        note,
      );
      this.incident = {
        ...this.incident,
        annotations: [...this.incident.annotations, created],
      };
      this.note = "";
      toast(t("incident.noteAdded"), "success");
    } catch (err) {
      toastError(err, t("state.error"));
    } finally {
      this.adding = false;
    }
  }

  // --- close (owner/admin) ---

  private async onCloseConfirmed(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId || !this.incident || this.closing) return;
    this.closing = true;
    try {
      const updated = await client.closeIncident(orgId, this.incident.id);
      this.incident = updated;
      toast(t("incident.closed"), "success");
    } catch (err) {
      // 409 means it was already closed (recovered or closed elsewhere); refresh
      // so the header reflects the now-closed state and tell the user.
      if (err instanceof ApiError && err.status === 409) {
        toast(t("incident.alreadyClosed"), "error");
        this.loadedKey = null;
        await this.load();
      } else {
        toastError(err, t("state.error"));
      }
    } finally {
      this.closing = false;
    }
  }

  override render() {
    if (this.loading && !this.incident) {
      return html`<div class="flex flex-col gap-6" aria-busy="true">
        <div class="h-9 w-64 bg-paper animate-pulse"></div>
        <div class="h-24 w-full bg-paper animate-pulse"></div>
        <div class="h-48 w-full bg-paper animate-pulse"></div>
      </div>`;
    }
    if (this.error || !this.incident) {
      return errorBox(
        this.error ?? t("state.error"),
        () => this.load(),
        t("state.retry"),
      );
    }
    return html`
      <div class="-mx-6 lg:-mx-10 -my-7">
        ${this.header()}
        <div class="px-6 lg:px-10 py-7 flex flex-col gap-6">
          ${this.timelineCard()} ${this.addNoteCard()}
        </div>
      </div>
      <confirm-dialog
        .heading=${t("incident.closeHeading")}
        .message=${t("incident.closeMessage")}
        .confirmLabel=${t("incident.close")}
        @confirm=${this.onCloseConfirmed}
      ></confirm-dialog>
    `;
  }

  private header() {
    const i = this.incident!;
    const open = i.ended_at === null;
    const canClose = can(this.ctx?.role ?? null, "incident.close");
    return html`
      <div
        class="flex flex-wrap items-start justify-between gap-4 px-6 lg:px-10 pt-8 pb-6 border-b border-line"
      >
        <div class="min-w-0 flex flex-col gap-3">
          <div class="flex flex-wrap items-center gap-3">
            <a
              class="hover:no-underline"
              href=${`${this.base}/monitors/${i.monitor_id}`}
            >
              <h1
                class="m-0 font-disp font-black uppercase tracking-[-0.04em] leading-[0.85] text-3xl lg:text-4xl text-ink hover:text-brand truncate"
              >
                ${this.headerName}
              </h1>
            </a>
            ${open
              ? html`<span class="pulse-state text-down"
                  ><span class="pulse-state-sq bg-down"></span
                  >${t("incidents.statusOpen")}</span
                >`
              : html`<span class="pulse-state text-ink3"
                  ><span class="pulse-state-sq bg-ink3"></span
                  >${t("incidents.statusClosed")}</span
                >`}
          </div>
          <dl
            class="font-mono text-[12px] text-ink2 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1.5"
          >
            <dt class="text-ink3 uppercase tracking-[0.08em]">
              ${t("incident.started")}
            </dt>
            <dd><relative-time .datetime=${i.started_at}></relative-time></dd>
            <dt class="text-ink3 uppercase tracking-[0.08em]">
              ${t("incident.ended")}
            </dt>
            <dd>
              ${open
                ? t("incidents.ongoing")
                : html`<relative-time .datetime=${i.ended_at ?? ""}></relative-time>`}
            </dd>
            <dt class="text-ink3 uppercase tracking-[0.08em]">
              ${t("incident.duration")}
            </dt>
            <dd>${open ? t("incidents.ongoing") : formatDuration(i.duration_seconds)}</dd>
            <dt class="text-ink3 uppercase tracking-[0.08em]">
              ${t("incident.cause")}
            </dt>
            <dd class="uppercase">${t(FAILURE_LABEL[i.cause_reason])}</dd>
            ${i.close_reason
              ? html`<dt class="text-ink3 uppercase tracking-[0.08em]">
                    ${t("incident.closeReason")}
                  </dt>
                  <dd>${t(CLOSE_REASON_LABEL[i.close_reason])}</dd>`
              : ""}
          </dl>
        </div>
        ${open && canClose
          ? html`<button
              class="pulse-btn pulse-btn-ghost border-down text-down"
              ?disabled=${this.closing}
              @click=${() => this.closeDialog.open()}
            >
              ${this.closing ? spinner() : icon("incident", "size-4")}${t(
                "incident.close",
              )}
            </button>`
          : ""}
      </div>
    `;
  }

  private authorLabel(a: IncidentAnnotation): string {
    return a.author_user_id === session.me?.user_id
      ? t("incident.you")
      : a.author_user_id;
  }

  private timelineCard() {
    const i = this.incident!;
    return html`
      <div class="border border-hair">
        <div class="flex flex-col gap-4 p-5">
          <h2
            class="m-0 pulse-section-title"
          >
            ${t("incident.timelineTitle")}
          </h2>
          ${i.annotations.length === 0
            ? html`<p class="text-ink3">${t("incident.noNotes")}</p>`
            : html`<ul class="flex flex-col gap-3 m-0 p-0 list-none">
                ${i.annotations.map(
                  (a) => html`<li
                    class="flex flex-col gap-1 border-l-2 border-hair pl-3"
                  >
                    <div class="flex flex-wrap items-center gap-2 text-sm">
                      <span class="font-medium text-ink">${this.authorLabel(a)}</span>
                      <span class="font-mono text-[11px] text-ink3">
                        <relative-time .datetime=${a.created_at}></relative-time>
                      </span>
                    </div>
                    <p class="whitespace-pre-wrap break-words text-ink2 m-0">
                      ${a.note}
                    </p>
                  </li>`,
                )}
              </ul>`}
        </div>
      </div>
    `;
  }

  private addNoteCard() {
    if (!can(this.ctx?.role ?? null, "incident.annotate")) return "";
    return html`
      <div class="border border-hair">
        <div class="flex flex-col gap-3 p-5">
          <h2
            class="m-0 pulse-section-title"
          >
            ${t("incident.addNoteTitle")}
          </h2>
          <form class="flex flex-col gap-3" @submit=${this.onAddNote}>
            <form-field
              label=${t("incident.note")}
              fieldName="incident-note"
              .control=${html`<textarea
                id="incident-note"
                class="pulse-input w-full"
                rows="3"
                .value=${this.note}
                @input=${(e: Event) =>
                  (this.note = (e.target as HTMLTextAreaElement).value)}
              ></textarea>`}
            ></form-field>
            <div class="flex justify-end">
              <button
                type="submit"
                class="pulse-btn pulse-btn-sm"
                ?disabled=${this.adding || this.note.trim() === ""}
              >
                ${this.adding ? t("incident.addingNote") : t("incident.addNote")}
              </button>
            </div>
          </form>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "incident-detail-view": IncidentDetailView;
  }
}
