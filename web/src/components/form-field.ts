import { html, type TemplateResult } from "lit";
import { customElement, property } from "lit/decorators.js";
import { AppElement } from "./base.js";
import { fieldHelp } from "../icons.js";

// Wraps a labelled form control and renders the per-field error from the API error
// envelope (RFC-013 section 10.1: validation_failed carries fields[name]).
//
// In light DOM there is no <slot>, so the control is passed as a property (a Lit
// template) rather than projected as a child. The caller owns the control markup
// and its daisyUI input classes; this wrapper owns the label, the layout, and the
// error/hint line.
//
// Usage:
//   <form-field
//     label="Name"
//     fieldName="name"
//     .error=${err?.fields?.name}
//     .control=${html`<input id="name" class="input w-full" .value=${this.name} />`}
//   ></form-field>
@customElement("form-field")
export class FormField extends AppElement {
  @property({ type: String }) label = "";
  // the field key in the error envelope; also the id the label points at
  @property({ type: String }) fieldName = "";
  // per-field message from fields[fieldName], if any
  @property({ type: String }) error: string | null = null;
  // optional helper text shown below the control when there is no error
  @property({ type: String }) hint = "";
  // optional explanatory text shown via an info icon + tooltip next to the label
  @property({ type: String }) help = "";
  // the control template (input/select/textarea) the caller supplies
  @property({ attribute: false }) control: TemplateResult | null = null;

  override render() {
    return html`
      <fieldset class="fieldset">
        ${this.label
          ? html`<label
              class="fieldset-legend inline-flex w-fit items-center gap-1.5"
              for=${this.fieldName}
              >${this.label}${this.help ? fieldHelp(this.help) : ""}</label
            >`
          : ""}
        ${this.control}
        ${this.error
          ? html`<p class="text-error text-sm" role="alert">${this.error}</p>`
          : this.hint
            ? html`<p class="text-base-content/60 text-sm">${this.hint}</p>`
            : ""}
      </fieldset>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "form-field": FormField;
  }
}
