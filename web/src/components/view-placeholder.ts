import { html } from "lit";
import { customElement, property } from "lit/decorators.js";
import { AppElement } from "./base.js";

// Temporary stand-in for the feature views (RFC-013 section 7). The route table in
// main.ts points its feature routes at this element so the app compiles and mounts
// before those views are built; each route target is swapped for the real component
// as it lands. Removed once all feature views exist (RFC-013 section 1.4).
@customElement("view-placeholder")
export class ViewPlaceholder extends AppElement {
  @property({ type: String }) name = "view";

  override render() {
    return html`
      <div
        class="border border-dashed border-hair bg-bg p-12 text-center text-ink3"
      >
        <h2 class="text-ink text-lg font-semibold capitalize">
          ${this.name}
        </h2>
        <p>${this.name} view coming soon.</p>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "view-placeholder": ViewPlaceholder;
  }
}
