import { html } from "lit";
import { customElement, property } from "lit/decorators.js";
import { AppElement } from "./base.js";
import { t } from "../i18n.js";

// Shown when an action is blocked by a plan limit (RFC-013 section 6.3). Pairs a
// plain explanation with an upgrade link to billing, so the user is not dead-ended.
// Used inline next to at-cap actions and as the render for entitlement_exceeded.
@customElement("upsell-banner")
export class UpsellBanner extends AppElement {
  @property({ type: String }) message = "";
  @property({ type: String }) upgradeHref = "";

  override render() {
    return html`
      <div role="status" class="alert alert-warning">
        <span>${this.message || t("upsell.limit")}</span>
        ${this.upgradeHref
          ? html`<a class="btn btn-sm" href=${this.upgradeHref}
              >${t("upsell.upgrade")}</a
            >`
          : ""}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "upsell-banner": UpsellBanner;
  }
}
