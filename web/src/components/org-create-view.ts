import { html, nothing } from "lit";
import { customElement, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, rememberLastOrg, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { navigate } from "../router.js";
import { t, tDynamic } from "../i18n.js";
import { toast } from "../toast.js";
import type { OrgInput } from "../api/types.js";

import "./form-field.js";

// Create-organization view, reached from the org switcher's "Create organization"
// action (/orgs/new, RFC-013 section 4.2). It collects a name, POSTs /orgs, then
// re-pulls /me so the new org appears in the session and switches to it by
// navigating to its path. The server is authoritative on the slug; we only send a
// name and let it derive the slug.
@customElement("org-create-view")
export class OrgCreateView extends AppElement {
  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private name = "";
  @state() private saving = false;
  @state() private error: ApiError | null = null;

  private async submit(e: Event): Promise<void> {
    e.preventDefault();
    if (this.saving) return;
    const name = this.name.trim();
    if (!name) {
      this.error = new ApiError(422, {
        code: "validation_failed",
        message: t("orgForm.errName"),
        fields: { name: t("orgForm.errName") },
      });
      return;
    }
    this.saving = true;
    this.error = null;
    try {
      const input: OrgInput = { name };
      const org = await client.createOrg(input);
      // make the new org the active one and pull a fresh /me so the switcher and
      // nav list it, then land on its home
      rememberLastOrg(org.org_id);
      await this.ctx.refreshMe();
      toast(t("orgForm.created"), "success");
      navigate(`/orgs/${org.org_id}`);
    } catch (err) {
      this.error = err instanceof ApiError ? err : null;
      if (!this.error) toast(t("state.error"), "error");
    } finally {
      this.saving = false;
    }
  }

  // An onboarding brand moment, not a list page: a mono kicker over a heavy Archivo
  // heading and the lead, then the form in a pulse-panel. Left in the content column
  // where the switcher lands it (no full-screen takeover, the user is already in).
  override render() {
    return html`
      <div class="max-w-md flex flex-col gap-7">
        <div class="flex flex-col gap-2.5">
          <span class="pulse-label">${tDynamic("orgForm.kicker", "New workspace")}</span>
          <h1
            class="m-0 font-disp font-black uppercase tracking-[-0.04em] leading-[0.85] text-[36px] lg:text-[42px]"
          >
            ${t("orgForm.heading")}
          </h1>
          <p class="font-mono text-[12px] leading-relaxed text-ink2">
            ${tDynamic(
              "orgForm.lead",
              "A workspace for your monitors, channels, and teammates. You can rename it later.",
            )}
          </p>
        </div>
        ${this.error && !this.error.fields
          ? html`<div role="alert" class="border border-down px-4 py-3 text-down">
              <span>${this.error.message}</span>
            </div>`
          : nothing}
        <form class="pulse-panel p-6 flex flex-col gap-5" @submit=${this.submit}>
          <form-field
            label=${t("orgForm.name")}
            fieldName="name"
            help=${t("orgForm.helpName")}
            .error=${this.error?.fields?.name ?? null}
            .control=${html`<input
              id="name"
              class="pulse-input w-full"
              .value=${this.name}
              @input=${(e: Event) =>
                (this.name = (e.target as HTMLInputElement).value)}
              autocomplete="organization"
            />`}
          ></form-field>
          <div class="flex gap-2">
            <button type="submit" class="pulse-btn" ?disabled=${this.saving}>
              ${this.saving ? t("orgForm.creating") : t("orgForm.create")}
            </button>
          </div>
        </form>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "org-create-view": OrgCreateView;
  }
}
