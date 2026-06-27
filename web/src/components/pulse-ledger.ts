import { html, type TemplateResult } from "lit";
import { customElement, property, query } from "lit/decorators.js";
import { AppElement } from "./base.js";

// Editorial "ledger": full-bleed indexed rows with one textured highlighter that
// slides to the hovered row (the signature interaction from the monitors/incidents
// screens). The caller supplies the items and a row renderer; this owns the layout
// and the highlighter so every list screen reads the same without re-implementing
// the hover plumbing.
//
//   <pulse-ledger .items=${rows} .renderRow=${(r, i) => html`...`}></pulse-ledger>
//
// renderRow returns the row body (typically an <a> grid). The ledger wraps each in
// a positioned cell that drives the highlighter, so the row body does not need its
// own hover handling.
@customElement("pulse-ledger")
export class PulseLedger extends AppElement {
  @property({ attribute: false }) items: unknown[] = [];
  @property({ attribute: false }) renderRow!: (
    item: unknown,
    index: number,
  ) => TemplateResult;

  @query(".pl-list") private listEl?: HTMLElement;
  @query(".pl-hl") private hlEl?: HTMLElement;

  private onRowEnter = (e: MouseEvent): void => {
    const wrap = this.listEl;
    const hl = this.hlEl;
    const row = e.currentTarget as HTMLElement;
    if (!wrap || !hl) return;
    const wr = wrap.getBoundingClientRect();
    const rr = row.getBoundingClientRect();
    hl.style.height = `${rr.height}px`;
    hl.style.transform = `translateY(${rr.top - wr.top}px)`;
    hl.style.opacity = "1";
  };

  private hide = (): void => {
    if (this.hlEl) this.hlEl.style.opacity = "0";
  };

  override render() {
    return html`<div
      class="pl-list relative -mx-6 lg:-mx-10 border-t border-line"
      @mouseleave=${this.hide}
    >
      <div class="pl-hl pulse-highlighter"></div>
      ${this.items.map(
        (item, i) =>
          html`<div class="relative z-[1]" @mouseenter=${this.onRowEnter}>
            ${this.renderRow(item, i)}
          </div>`,
      )}
    </div>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "pulse-ledger": PulseLedger;
  }
}
