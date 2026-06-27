import { html } from "lit";
import { customElement, property, state } from "lit/decorators.js";
import { AppElement } from "./base.js";
import { t } from "../i18n.js";

// A reusable confirm dialog (daisyUI modal). It never uses window.confirm/alert
// because those block the main thread. Open it by setting properties and calling
// open(). It emits a "confirm" event when the user confirms and a "cancel" event
// otherwise.
//
// Accessibility (RFC-013 section 9.1): role=dialog aria-modal, labelled by the
// heading, Escape closes, focus moves into the dialog on open and is trapped
// within it, and is restored to the trigger on close. In light DOM the focus
// queries use `this` (there is no shadow root).
@customElement("confirm-dialog")
export class ConfirmDialog extends AppElement {
  @property({ type: String }) heading = t("dialog.heading");
  @property({ type: String }) message = "";
  @property({ type: String }) confirmLabel = t("dialog.confirm");
  @property({ type: String }) cancelLabel = t("dialog.cancel");
  // danger styles the confirm button as destructive
  @property({ type: Boolean }) danger = false;

  @state() private isOpen = false;

  private previouslyFocused: HTMLElement | null = null;
  private wasOpen = false;

  open(): void {
    this.isOpen = true;
  }

  close(): void {
    this.isOpen = false;
  }

  private onConfirm(): void {
    this.isOpen = false;
    this.dispatchEvent(
      new CustomEvent("confirm", { bubbles: true, composed: true }),
    );
  }

  private onCancel(): void {
    this.isOpen = false;
    this.dispatchEvent(
      new CustomEvent("cancel", { bubbles: true, composed: true }),
    );
  }

  // the action buttons inside the dialog panel (excludes the backdrop element)
  private buttons(): HTMLElement[] {
    return Array.from(this.querySelectorAll<HTMLElement>(".pulse-dialog button"));
  }

  // Escape closes; Tab is trapped so focus cannot leave the open dialog.
  private onKeydown = (e: KeyboardEvent): void => {
    if (!this.isOpen) return;
    if (e.key === "Escape") {
      this.onCancel();
      return;
    }
    if (e.key === "Tab") this.trapTab(e);
  };

  private trapTab(e: KeyboardEvent): void {
    const focusables = this.buttons();
    if (focusables.length === 0) return;
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    const active = document.activeElement;

    if (!active || !focusables.includes(active as HTMLElement)) {
      e.preventDefault();
      first.focus();
    } else if (e.shiftKey && active === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && active === last) {
      e.preventDefault();
      first.focus();
    }
  }

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener("keydown", this.onKeydown);
  }

  override disconnectedCallback(): void {
    window.removeEventListener("keydown", this.onKeydown);
    super.disconnectedCallback();
  }

  // Move focus into the dialog on open, restore it on close. Runs after render,
  // so the buttons exist in the light DOM.
  override updated(): void {
    if (this.isOpen && !this.wasOpen) {
      this.previouslyFocused = deepActiveElement();
      // focus the cancel (first) button: the safe default for destructive prompts
      this.buttons()[0]?.focus();
    } else if (!this.isOpen && this.wasOpen) {
      this.previouslyFocused?.focus?.();
      this.previouslyFocused = null;
    }
    this.wasOpen = this.isOpen;
  }

  override render() {
    if (!this.isOpen) return html``;
    return html`
      <div
        class="fixed inset-0 z-50 flex items-center justify-center p-4"
        role="dialog"
        aria-modal="true"
        aria-labelledby="cd-heading"
      >
        <div class="absolute inset-0 bg-black/40" @click=${this.onCancel}></div>
        <div
          class="pulse-dialog relative w-full max-w-md border border-line bg-bg p-6 flex flex-col gap-4"
        >
          <h3
            id="cd-heading"
            class="font-disp font-extrabold text-lg uppercase tracking-[-0.01em]"
          >
            ${this.heading}
          </h3>
          ${this.message ? html`<p class="text-ink2">${this.message}</p>` : ""}
          <div class="flex justify-end gap-2">
            <button class="pulse-btn pulse-btn-ghost" @click=${this.onCancel}>
              ${this.cancelLabel}
            </button>
            <button
              data-confirm
              class=${this.danger
                ? "pulse-btn pulse-btn-ghost border-down text-down"
                : "pulse-btn"}
              @click=${this.onConfirm}
            >
              ${this.confirmLabel}
            </button>
          </div>
        </div>
      </div>
    `;
  }
}

// The actually-focused element, walking into shadow roots in case a trigger lives
// inside a nested web component. Used to restore focus when the dialog closes.
function deepActiveElement(): HTMLElement | null {
  let el = document.activeElement as HTMLElement | null;
  while (el?.shadowRoot?.activeElement) {
    el = el.shadowRoot.activeElement as HTMLElement;
  }
  return el;
}

declare global {
  interface HTMLElementTagNameMap {
    "confirm-dialog": ConfirmDialog;
  }
}
