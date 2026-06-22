import { LitElement } from "lit";
import { onLocaleChange } from "../i18n.js";

// Base for all app components (RFC-013 section 2.5). Renders into light DOM so the
// global Tailwind + daisyUI classes reach component markup; there is no shadow
// root and no scoped `static styles`. The host element is made layout-transparent
// (display: contents) so the single wrapper each component renders carries the
// layout/daisyUI classes.
//
// It also subscribes to locale changes (RFC-013 section 9.2): when the language
// switches, every mounted component re-renders, so all t(key) copy updates without
// a reload.
export class AppElement extends LitElement {
  private _i18nUnsubscribe?: () => void;

  protected override createRenderRoot(): HTMLElement {
    return this;
  }

  override connectedCallback(): void {
    super.connectedCallback();
    this.style.display = "contents";
    this._i18nUnsubscribe = onLocaleChange(() => this.requestUpdate());
  }

  override disconnectedCallback(): void {
    this._i18nUnsubscribe?.();
    super.disconnectedCallback();
  }
}
