import { html } from "lit";
import { customElement } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, lastOrgHint, type AppContext } from "../state/context.js";
import { navigate } from "../router.js";
import { t } from "../i18n.js";
import { icon } from "../icons.js";
import type { OrgMembership } from "../api/types.js";

// Org switcher (RFC-013 section 4.2). Lists the orgs the user belongs to (already
// loaded in /me, read from the context), shows the active org, and offers "Create
// organization". Selecting an org navigates to its home; switching is a pure
// client navigation, nothing is written server-side and no token is reissued.
//
// It's a daisyUI dropdown rather than a native <select> so each item can be rich
// and multiline: org name, the role + plan badges, and the org id. The active
// org's role/plan also shows under the trigger, so there's no stray role line in
// the sidebar. dropdown-top opens it upward since the switcher sits at the bottom.
@customElement("org-switcher")
export class OrgSwitcher extends AppElement {
  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  private go(orgId: string): void {
    this.close();
    navigate(`/orgs/${orgId}`);
  }

  private createOrg(): void {
    this.close();
    navigate("/orgs/new");
  }

  // daisyUI dropdowns stay open while something inside has focus; blur to close
  // after a pick so the menu doesn't linger over the new route.
  private close(): void {
    (document.activeElement as HTMLElement | null)?.blur();
  }

  private item(o: OrgMembership, active: boolean) {
    return html`
      <li>
        <a
          class=${active ? "active" : ""}
          @click=${() => this.go(o.org_id)}
        >
          <span class="flex flex-col items-start gap-1 py-0.5 min-w-0">
            <span class="flex w-full items-center gap-2">
              <span class="font-medium truncate">${o.name}</span>
              ${active ? icon("check", "size-4 opacity-70 shrink-0 ml-auto") : ""}
            </span>
            <span class="flex flex-wrap gap-1">
              <span class="badge badge-ghost badge-xs">${t(`role.${o.role}` as const)}</span>
              <span class="badge badge-primary badge-soft badge-xs"
                >${t(`plan.${o.plan}` as const)}</span
              >
            </span>
            <span class="flex items-baseline gap-1 text-[0.65rem] opacity-50 max-w-full">
              <span class="uppercase tracking-wide">${t("org.id")}</span>
              <span class="font-mono truncate">${o.org_id}</span>
            </span>
          </span>
        </a>
      </li>
    `;
  }

  override render() {
    const me = this.ctx?.me;
    if (!me || me.orgs.length === 0) return html``;
    // show the active org, or fall back to the last-used / first org on a non-org
    // route (/account, /orgs/new) so the switcher is never blank
    const hint = lastOrgHint();
    const selected =
      this.ctx.activeOrg ??
      (hint ? me.orgs.find((o) => o.org_id === hint) : undefined) ??
      me.orgs[0];
    const activeId = selected?.org_id ?? "";

    return html`
      <div class="dropdown dropdown-top w-full">
        <div
          tabindex="0"
          role="button"
          class="btn btn-sm btn-block h-auto py-1.5 justify-between font-normal normal-case"
          aria-label=${t("org.switch")}
        >
          <span class="flex flex-col items-start leading-tight min-w-0">
            <span class="truncate max-w-full">${selected?.name}</span>
            ${selected
              ? html`<span class="text-[0.65rem] opacity-60"
                  >${t(`role.${selected.role}` as const)} ·
                  ${t(`plan.${selected.plan}` as const)}</span
                >`
              : ""}
          </span>
          ${icon("chevronDown", "size-4 opacity-60 shrink-0")}
        </div>
        <ul
          tabindex="0"
          class="dropdown-content menu menu-sm z-10 mb-1 w-full gap-0.5 rounded-box border border-base-300 bg-base-100 p-2 shadow-lg"
        >
          ${me.orgs.map((o) => this.item(o, o.org_id === activeId))}
          <li>
            <a @click=${() => this.createOrg()}>${t("org.create")}</a>
          </li>
        </ul>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "org-switcher": OrgSwitcher;
  }
}
