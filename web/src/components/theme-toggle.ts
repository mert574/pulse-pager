import { html } from "lit";
import { customElement, state } from "lit/decorators.js";
import { AppElement } from "./base.js";
import { currentTheme, toggleTheme } from "../theme.js";
import { t } from "../i18n.js";

// Light/dark theme toggle for the sidebar (RFC-013 section 9.3). A daisyUI swap
// showing a sun (light) or moon (dark); flipping it switches data-theme on <html>
// and persists the choice. Theme is plain CSS, so no app-wide re-render is needed,
// only this control reflects the new state.
@customElement("theme-toggle")
export class ThemeToggle extends AppElement {
  @state() private dark = currentTheme() === "coffee";

  private onToggle(): void {
    this.dark = toggleTheme() === "coffee";
  }

  override render() {
    const label = t("theme.toggle");
    return html`
      <label
        class="swap swap-rotate btn btn-ghost btn-sm btn-circle"
        title=${label}
      >
        <input
          type="checkbox"
          aria-label=${label}
          .checked=${this.dark}
          @change=${this.onToggle}
        />
        <svg
          class="swap-off size-5 fill-current"
          xmlns="http://www.w3.org/2000/svg"
          viewBox="0 0 24 24"
          aria-hidden="true"
        >
          <path
            d="M12 17a5 5 0 100-10 5 5 0 000 10zm0 2a1 1 0 011 1v1a1 1 0 11-2 0v-1a1 1 0 011-1zm0-18a1 1 0 011 1v1a1 1 0 11-2 0V2a1 1 0 011-1zm10 10a1 1 0 010 2h-1a1 1 0 110-2h1zM3 11a1 1 0 010 2H2a1 1 0 110-2h1zm15.07-7.07a1 1 0 011.41 1.41l-.7.71a1 1 0 11-1.42-1.42l.71-.7zM5.64 17.66a1 1 0 011.41 1.41l-.71.7a1 1 0 11-1.41-1.41l.71-.7zM18.36 17.66l.71.7a1 1 0 11-1.41 1.41l-.71-.7a1 1 0 011.41-1.41zM6.34 4.34l.7.71A1 1 0 015.64 6.46l-.71-.7a1 1 0 011.41-1.42z"
          />
        </svg>
        <svg
          class="swap-on size-5 fill-current"
          xmlns="http://www.w3.org/2000/svg"
          viewBox="0 0 24 24"
          aria-hidden="true"
        >
          <path
            d="M21.64 13a1 1 0 00-1.05-.14 8 8 0 01-3.37.73 8.15 8.15 0 01-8.14-8.1 8 8 0 01.74-3.36 1 1 0 00-1.2-1.36A10.14 10.14 0 1022 14.05a1 1 0 00-.36-1.05z"
          />
        </svg>
      </label>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "theme-toggle": ThemeToggle;
  }
}
