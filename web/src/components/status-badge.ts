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

// daisyUI badge variant per status. Status is never shown by color alone: the
// text label is always present (PRD-004 / RFC-013 section 9.1). disabled uses
// badge-ghost (a base-toned badge that stays legible in both themes); soft-neutral
// would be dark-on-dark in the coffee theme.
const STATUS_CLASS: Record<CoverageStatus, string> = {
  up: "badge-success badge-soft",
  down: "badge-error badge-soft",
  disabled: "badge-ghost",
  pending: "badge-warning badge-soft",
  "coverage-degraded": "badge-secondary badge-soft",
};

// A small pill showing a monitor's status.
@customElement("status-badge")
export class StatusBadge extends AppElement {
  @property({ type: String }) status: CoverageStatus = "pending";

  override render() {
    return html`<span class="badge ${STATUS_CLASS[this.status]} gap-1">
      <span
        class="inline-block size-2 rounded-full bg-current"
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
