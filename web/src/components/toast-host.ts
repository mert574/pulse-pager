import { html } from "lit";
import { customElement, state } from "lit/decorators.js";
import { AppElement } from "./base.js";
import { subscribeToasts, dismissToast, type ToastItem } from "../toast.js";
import { icon, type IconName } from "../icons.js";
import { t } from "../i18n.js";

// Border + icon color per toast type (Swiss): bordered panels, color carried by the
// border and the icon, never by background alone.
const TOAST_BORDER: Record<ToastItem["type"], string> = {
  success: "border-up",
  error: "border-down",
  info: "border-line",
};

const TOAST_ICON_COLOR: Record<ToastItem["type"], string> = {
  success: "text-up",
  error: "text-down",
  info: "text-ink2",
};

const ALERT_ICON: Record<ToastItem["type"], IconName> = {
  success: "check",
  error: "x",
  info: "info",
};

// Renders the active toasts in a fixed bottom-right stack. Mounted once in app-root;
// driven by the toast() pub/sub.
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
      class="font-mono text-xs text-ink3 hover:text-ink underline"
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
      <div
        class="fixed bottom-4 right-4 z-50 flex flex-col items-end gap-2"
        role="region"
        aria-label="Notifications"
      >
        ${this.items.map(
          (item) => html`<div
            role="status"
            class="flex items-center gap-2 border-2 ${TOAST_BORDER[
              item.type
            ]} bg-bg px-4 py-3 text-ink cursor-pointer max-w-sm"
            @click=${() => dismissToast(item.id)}
          >
            <span class=${TOAST_ICON_COLOR[item.type]}
              >${icon(ALERT_ICON[item.type], "size-4")}</span
            >
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
