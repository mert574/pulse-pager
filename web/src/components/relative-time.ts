import { html } from "lit";
import { customElement, property } from "lit/decorators.js";
import { AppElement } from "./base.js";
import { formatDateTime, formatRelative } from "../format.js";

// All mounted <relative-time> elements refresh on one shared timer, so a table full
// of them costs a single interval, not one per cell. 30s is fine: the smallest unit
// shown is a minute.
const live = new Set<RelativeTime>();
let timer: number | null = null;

function startTimer(): void {
  if (timer !== null) return;
  timer = window.setInterval(() => {
    for (const el of live) el.requestUpdate();
  }, 30_000);
}

function stopTimerIfIdle(): void {
  if (timer !== null && live.size === 0) {
    window.clearInterval(timer);
    timer = null;
  }
}

// A timestamp shown as a short relative time ("5m ago", "in 3d") that ticks on its
// own, with the full localized date+time on hover (native title). datetime is an
// RFC3339 string; empty/invalid renders nothing so callers can pass a value straight
// through. For a null-with-fallback case (e.g. "Never"), the caller guards and skips
// this element.
@customElement("relative-time")
export class RelativeTime extends AppElement {
  @property({ type: String }) datetime = "";

  override connectedCallback(): void {
    super.connectedCallback();
    live.add(this);
    startTimer();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    live.delete(this);
    stopTimerIfIdle();
  }

  override render() {
    const rel = formatRelative(this.datetime);
    if (!rel) return html``;
    return html`<span title=${formatDateTime(this.datetime) ?? ""}>${rel}</span>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "relative-time": RelativeTime;
  }
}
