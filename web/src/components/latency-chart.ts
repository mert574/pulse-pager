import { html } from "lit";
import { customElement, property } from "lit/decorators.js";
import uPlot from "uplot";
import { AppElement } from "./base.js";
import { t } from "../i18n.js";

const CHART_HEIGHT = 200;

export interface LatencyPoint {
  latency: number | null;
  healthy: boolean;
  // shown in the hover tooltip
  label?: string; // formatted check time
  result?: string; // result text (Healthy / failure reason, optionally + code)
}

// Latency chart (RFC-013 section 7.2, decision D12) on uPlot: a line + soft area
// of latency over recent checks, failed checks marked in the theme error color,
// and a hover tooltip with the check time, latency, and result. uPlot draws on
// canvas, so colors are read from the daisyUI theme variables (getComputedStyle)
// and the chart is rebuilt when the theme changes; a ResizeObserver keeps it the
// container width. uPlot's stylesheet is imported once in the app entry (main.ts).
@customElement("latency-chart")
export class LatencyChart extends AppElement {
  // chronological order (oldest first)
  @property({ attribute: false }) points: LatencyPoint[] = [];

  private chart?: uPlot;
  private tip?: HTMLDivElement;
  private resizeObserver?: ResizeObserver;
  private themeObserver?: MutationObserver;

  private get latencyLabel(): string {
    return t("monitors.colLatency");
  }

  override render() {
    return html`<div class="uplot-host relative w-full"></div>`;
  }

  override disconnectedCallback(): void {
    this.teardown();
    super.disconnectedCallback();
  }

  override updated(): void {
    const host = this.querySelector<HTMLElement>(".uplot-host");
    if (!host) return;
    const data = this.toData();
    if (data[0].length < 2) {
      this.teardown();
      return;
    }
    if (!this.chart) {
      this.chart = new uPlot(this.opts(host.clientWidth || 600), data, host);
      this.createTooltip();
      this.observe(host);
    } else {
      this.chart.setData(data);
    }
  }

  private observe(host: HTMLElement): void {
    this.resizeObserver = new ResizeObserver(() => {
      this.chart?.setSize({ width: host.clientWidth || 600, height: CHART_HEIGHT });
    });
    this.resizeObserver.observe(host);
    this.themeObserver = new MutationObserver(() => this.rebuild());
    this.themeObserver.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ["data-theme"],
    });
  }

  private rebuild(): void {
    this.teardown();
    this.requestUpdate();
  }

  private teardown(): void {
    this.chart?.destroy(); // also removes the tooltip (a child of u.over)
    this.chart = undefined;
    this.tip = undefined;
    this.resizeObserver?.disconnect();
    this.themeObserver?.disconnect();
  }

  // A floating tooltip element, appended to uPlot's overlay so cursor pixel
  // coords position it directly.
  private createTooltip(): void {
    if (!this.chart) return;
    const tip = document.createElement("div");
    tip.className =
      "absolute z-10 hidden pointer-events-none flex flex-col gap-1 rounded-lg " +
      "border border-base-300 bg-base-100 px-3.5 py-2.5 text-xs leading-relaxed " +
      "shadow-lg min-w-40 whitespace-nowrap";
    tip.style.transform = "translate(-50%, calc(-100% - 12px))";
    this.chart.over.appendChild(tip);
    this.tip = tip;
  }

  private updateTooltip(u: uPlot): void {
    const tip = this.tip;
    if (!tip) return;
    const idx = u.cursor.idx;
    if (idx == null) {
      tip.classList.add("hidden");
      return;
    }
    const p = this.points[idx];
    if (!p) {
      tip.classList.add("hidden");
      return;
    }
    const left = u.cursor.left ?? 0;
    const top = p.latency != null ? u.valToPos(p.latency, "y") : 0;
    tip.style.left = `${left}px`;
    tip.style.top = `${top}px`;
    tip.classList.remove("hidden");

    const resultClass = p.healthy ? "text-success" : "text-error";
    const rows: string[] = [];
    if (p.label) {
      rows.push(
        `<div class="font-semibold pb-1.5 mb-0.5 border-b border-base-200">${p.label}</div>`,
      );
    }
    if (p.latency != null) {
      rows.push(
        `<div class="flex items-center justify-between gap-6">` +
          `<span class="text-base-content/60">${this.latencyLabel}</span>` +
          `<span class="font-semibold tabular-nums">${p.latency} ms</span></div>`,
      );
    }
    if (p.result) {
      rows.push(
        `<div class="flex items-center gap-1.5"><span class="size-2 rounded-full ${p.healthy ? "bg-success" : "bg-error"}"></span><span class="${resultClass}">${p.result}</span></div>`,
      );
    }
    tip.innerHTML = rows.join("");
  }

  private toData(): uPlot.AlignedData {
    const xs: number[] = [];
    const ys: (number | null)[] = [];
    const failed: (number | null)[] = [];
    this.points.forEach((p, i) => {
      xs.push(i);
      ys.push(p.latency);
      failed.push(!p.healthy ? p.latency : null);
    });
    return [xs, ys, failed];
  }

  private cssVar(name: string, fallback: string): string {
    const v = getComputedStyle(this).getPropertyValue(name).trim();
    return v || fallback;
  }

  private withAlpha(color: string, alpha: number): string {
    return color.startsWith("oklch(")
      ? color.replace(/\)\s*$/, ` / ${alpha})`)
      : color;
  }

  private opts(width: number): uPlot.Options {
    const primary = this.cssVar("--color-primary", "#2563eb");
    const error = this.cssVar("--color-error", "#dc2626");
    const text = this.cssVar("--color-base-content", "#1c2128");
    const grid = this.cssVar("--color-base-300", "#e2e5ea");
    return {
      width,
      height: CHART_HEIGHT,
      legend: { show: false },
      cursor: { y: false, points: { size: 6 } },
      scales: { x: { time: false } },
      hooks: { setCursor: [(u) => this.updateTooltip(u)] },
      axes: [
        { show: false },
        {
          stroke: text,
          grid: { stroke: grid, width: 1 },
          ticks: { show: false },
          size: 44,
          font: "11px system-ui, sans-serif",
        },
      ],
      series: [
        {},
        {
          label: "ms",
          stroke: primary,
          width: 2,
          fill: this.withAlpha(primary, 0.12),
          points: { show: false },
        },
        {
          label: "fail",
          stroke: error,
          fill: error,
          paths: () => null,
          points: { show: true, size: 7 },
        },
      ],
    };
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "latency-chart": LatencyChart;
  }
}
