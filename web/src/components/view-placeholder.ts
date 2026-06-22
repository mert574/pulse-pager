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
        class="rounded-box border border-dashed border-base-300 bg-base-100 p-12 text-center text-base-content/60"
      >
        <h2 class="text-base-content text-lg font-semibold capitalize">
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
