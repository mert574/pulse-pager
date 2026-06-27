import { html } from "lit";
import { customElement, state } from "lit/decorators.js";
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
// It's our own little menu rather than a native <select> so each item can be rich
// and multiline: org name, the role + plan tags, and the org id. The active org's
// role/plan also shows under the trigger, so there's no stray role line in the
// sidebar. The menu opens upward since the switcher sits at the bottom of the nav.
// We track open ourselves and close on a pick or an outside click.
@customElement("org-switcher")
export class OrgSwitcher extends AppElement {
  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private open = false;

  override connectedCallback(): void {
    super.connectedCallback();
    document.addEventListener("click", this.onDocClick);
  }

  override disconnectedCallback(): void {
    document.removeEventListener("click", this.onDocClick);
    super.disconnectedCallback();
  }

  // Close when a click lands outside the switcher. The trigger's own @click runs
  // first (it sits inside the wrapper), so opening is not undone here.
  private onDocClick = (e: Event): void => {
    if (!this.open) return;
    const root = this.querySelector("[data-switcher]");
    if (root && !e.composedPath().includes(root)) this.open = false;
  };

  private toggle(): void {
    this.open = !this.open;
  }

  private go(orgId: string): void {
    this.open = false;
    navigate(`/orgs/${orgId}`);
  }

  private createOrg(): void {
    this.open = false;
    navigate("/orgs/new");
  }

  private item(o: OrgMembership, active: boolean) {
    return html`
      <li>
        <a
          class="block px-2 py-1.5 cursor-pointer hover:bg-paper ${active
            ? "bg-paper"
            : ""}"
          @click=${() => this.go(o.org_id)}
        >
          <span class="flex flex-col items-start gap-1 py-0.5 min-w-0">
            <span class="flex w-full items-center gap-2">
              <span class="font-medium text-ink truncate">${o.name}</span>
              ${active ? icon("check", "size-4 text-ink3 shrink-0 ml-auto") : ""}
            </span>
            <span class="flex flex-wrap gap-1">
              <span
                class="pulse-tag"
                >${t(`role.${o.role}` as const)}</span
              >
              <span
                class="font-mono text-[11px] uppercase tracking-[0.04em] text-brand"
                >${t(`plan.${o.plan}` as const)}</span
              >
            </span>
            <span
              class="flex items-baseline gap-1 text-[0.65rem] text-ink3 max-w-full"
            >
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
      <div class="relative w-full" data-switcher>
        <button
          type="button"
          role="button"
          class="w-full flex items-center justify-between gap-2 border border-line px-3 py-1.5 text-[13px] text-ink hover:border-brand"
          aria-label=${t("org.switch")}
          aria-expanded=${this.open}
          @click=${() => this.toggle()}
        >
          <span class="flex flex-col items-start leading-tight min-w-0">
            <span class="truncate max-w-full">${selected?.name}</span>
            ${selected
              ? html`<span class="text-[0.65rem] text-ink3"
                  >${t(`role.${selected.role}` as const)} ·
                  ${t(`plan.${selected.plan}` as const)}</span
                >`
              : ""}
          </span>
          ${icon("chevronDown", "size-4 text-ink3 shrink-0")}
        </button>
        <ul
          role="menu"
          class="absolute top-full left-0 z-10 mt-1 w-full flex flex-col gap-0.5 border border-line bg-bg p-2 ${this
            .open
            ? ""
            : "hidden"}"
        >
          ${me.orgs.map((o) => this.item(o, o.org_id === activeId))}
          <li>
            <a
              class="block px-2 py-1.5 cursor-pointer text-ink2 hover:bg-paper hover:text-ink"
              @click=${() => this.createOrg()}
              >${t("org.create")}</a
            >
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
