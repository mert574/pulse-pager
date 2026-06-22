import { html } from "lit";
import { customElement, property } from "lit/decorators.js";
import { AppElement } from "./base.js";

export interface UptimeBar {
  healthy: boolean;
  title?: string; // hover text (e.g. time + result)
}

// The signature uptime strip (RFC-013 section 7.1): one bar per recent check,
// green for healthy and red for failed, chronological (oldest left). Each bar has
// a hover title with its time and result. Pure CSS, theme-colored.
@customElement("uptime-bar")
export class UptimeBarStrip extends AppElement {
  @property({ attribute: false }) bars: UptimeBar[] = [];

  override render() {
    if (this.bars.length === 0) return html``;
    return html`
      <div class="flex items-stretch gap-0.5 h-9 w-full" role="img">
        ${this.bars.map(
          (b) => html`<div
            title=${b.title ?? ""}
            class="flex-1 min-w-0.5 rounded-sm transition-opacity hover:opacity-70 ${b.healthy
              ? "bg-success"
              : "bg-error"}"
          ></div>`,
        )}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "uptime-bar": UptimeBarStrip;
  }
}
