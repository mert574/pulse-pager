import { html } from "lit";
import { customElement, property } from "lit/decorators.js";
import { AppElement } from "./base.js";
import { t, type MessageKey } from "../i18n.js";
import { formatLatency, formatStatusCode } from "../format.js";
import type { RegionState } from "../api/types.js";

// The label key per live check state. running is shown as "pinging" so the
// amber pulse reads as in-progress; done splits on healthy (ok / down). Status
// is never shown by color alone: the chip always carries the region name and a
// text label (RFC-013 section 9.1, same rule as status-badge).
const STATE_LABEL: Record<RegionState["state"], MessageKey> = {
  scheduled: "region.stateScheduled",
  running: "region.stateRunning",
  done: "region.stateOk",
  failed: "region.stateDown",
};

// daisyUI badge variant per state. done flips to error when the check came back
// unhealthy, so a passing/failing done are visually distinct.
const STATE_CLASS: Record<RegionState["state"], string> = {
  scheduled: "badge-ghost",
  running: "badge-warning badge-soft",
  done: "badge-success badge-soft",
  failed: "badge-error badge-soft",
};

// A small row of chips, one per region, showing each region's live check state
// (RFC-013 multi-region). Purely presentational: it takes the states and renders
// them, never fetches. Shared by the monitors list and the monitor detail view.
@customElement("region-chips")
export class RegionChips extends AppElement {
  @property({ attribute: false }) states: RegionState[] = [];

  override render() {
    if (this.states.length === 0) return html``;
    return html`<div class="flex flex-wrap items-center gap-1">
      ${this.states.map((s) => this.chip(s))}
    </div>`;
  }

  private chip(s: RegionState) {
    // done can be either ok or down; resolve the effective look from healthy.
    const failed = s.state === "failed" || (s.state === "done" && s.healthy === false);
    const cls = failed ? STATE_CLASS.failed : STATE_CLASS[s.state];
    const labelKey = failed ? STATE_LABEL.failed : STATE_LABEL[s.state];
    const pulsing = s.state === "running";
    return html`<span
      class="badge badge-sm gap-1 ${cls}"
      title=${this.tooltip(s)}
    >
      <span
        class="inline-block size-2 rounded-full bg-current ${pulsing
          ? "animate-pulse"
          : ""}"
        aria-hidden="true"
      ></span>
      <span class="font-medium">${s.region}</span>
      <span class="opacity-70">${t(labelKey)}</span>
    </span>`;
  }

  // Latency / status code in the hover tooltip when the state carries them, so
  // the chip stays compact but the detail is one hover away.
  private tooltip(s: RegionState): string {
    const parts = [s.region];
    if (s.latency_ms != null) {
      const lat = formatLatency(s.latency_ms);
      if (lat) parts.push(lat);
    }
    if (s.status_code != null) parts.push(formatStatusCode(s.status_code));
    return parts.join(" · ");
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "region-chips": RegionChips;
  }
}
