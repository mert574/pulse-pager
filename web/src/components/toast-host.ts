import { html } from "lit";
import { customElement, state } from "lit/decorators.js";
import { AppElement } from "./base.js";
import { subscribeToasts, dismissToast, type ToastItem } from "../toast.js";
import { icon, type IconName } from "../icons.js";
import { t } from "../i18n.js";

const ALERT_CLASS: Record<ToastItem["type"], string> = {
  success: "alert-success",
  error: "alert-error",
  info: "alert-info",
};

const ALERT_ICON: Record<ToastItem["type"], IconName> = {
  success: "check",
  error: "x",
  info: "info",
};

// Renders the active toasts in a fixed daisyUI toast stack (bottom-end). Mounted
// once in app-root; driven by the toast() pub/sub.
@customElement("toast-host")
export class ToastHost extends AppElement {
  @state() private items: ToastItem[] = [];
  private unsubscribe?: () => void;

  override connectedCallback(): void {
    super.connectedCallback();
    this.unsubscribe = subscribeToasts((items) => {
      this.items = items;
    });
  }

  override disconnectedCallback(): void {
    this.unsubscribe?.();
    super.disconnectedCallback();
  }

  // Short, clickable trace id on error toasts: clicking copies the full id and does
  // not dismiss the toast (RFC-021 section 8). Shown abbreviated to keep it small.
  private renderTraceId(traceId: string) {
    return html`<button
      class="font-mono text-xs opacity-70 hover:opacity-100 underline"
      title=${t("error.traceId")}
      @click=${(e: Event) => {
        e.stopPropagation();
        void navigator.clipboard?.writeText(traceId);
      }}
    >
      ${traceId.slice(0, 8)}
    </button>`;
  }

  override render() {
    if (this.items.length === 0) return html``;
    return html`
      <div class="toast toast-end toast-bottom z-50">
        ${this.items.map(
          (item) => html`<div
            role="status"
            class="alert ${ALERT_CLASS[item.type]} shadow-lg cursor-pointer"
            @click=${() => dismissToast(item.id)}
          >
            ${icon(ALERT_ICON[item.type], "size-4")}
            <span>${item.message}</span>
            ${item.traceId ? this.renderTraceId(item.traceId) : null}
          </div>`,
        )}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "toast-host": ToastHost;
  }
}
