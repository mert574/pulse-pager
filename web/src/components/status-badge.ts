import { html } from "lit";
import { customElement, property } from "lit/decorators.js";
import { AppElement } from "./base.js";
import type { CoverageStatus } from "../api/types.js";
import { t, type MessageKey } from "../i18n.js";

// Maps each status to its (translatable) label key, so the badge never renders a
// raw enum value as user-facing text.
const STATUS_LABEL: Record<CoverageStatus, MessageKey> = {
  up: "status.up",
  down: "status.down",
  disabled: "status.disabled",
  pending: "status.pending",
  "coverage-degraded": "status.coverageDegraded",
};

// Text + square color per status (Swiss). Status is never shown by color alone:
// the text label is always present (PRD-004 / RFC-013 section 9.1). Red is reserved
// for down; degraded/pending share the amber, disabled is muted ink.
const STATUS_TEXT: Record<CoverageStatus, string> = {
  up: "text-up",
  down: "text-down",
  disabled: "text-ink3",
  pending: "text-deg",
  "coverage-degraded": "text-deg",
};

const STATUS_SQUARE: Record<CoverageStatus, string> = {
  up: "bg-up",
  down: "bg-down",
  disabled: "bg-ink3",
  pending: "bg-deg",
  "coverage-degraded": "bg-deg",
};

// A small status marker: a colored square plus its label.
@customElement("status-badge")
export class StatusBadge extends AppElement {
  @property({ type: String }) status: CoverageStatus = "pending";

  override render() {
    return html`<span class="pulse-state ${STATUS_TEXT[this.status]}">
      <span
        class="pulse-state-sq ${STATUS_SQUARE[this.status]}"
        aria-hidden="true"
      ></span>
      ${t(STATUS_LABEL[this.status])}
    </span>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "status-badge": StatusBadge;
  }
}
