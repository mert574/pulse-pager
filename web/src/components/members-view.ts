import { html, nothing } from "lit";
import { customElement, query, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { can } from "../state/can.js";
import { t, tDynamic } from "../i18n.js";
import { toast, toastError } from "../toast.js";
import { navigate } from "../router.js";
import { icon } from "../icons.js";
import { session } from "../state/session.js";
import { pageHeader, errorBox, skeletonRows } from "./ui.js";
import type {
  Invitation,
  InvitationInput,
  Member,
  Role,
} from "../api/types.js";

import "./pulse-ledger.js";
import "./confirm-dialog.js";
import "./form-field.js";
import "./relative-time.js";
import type { ConfirmDialog } from "./confirm-dialog.js";

// Members + invitations (PRD-001, RFC-003). A roster grouped by role: a masthead,
// a thin seats-used line, then a section per role that has members (Owners, Admins,
// Members, Viewers), each member shown as an avatar initial, name and email. For
// owner/admin a pending-invitations section with the invite form follows. Owner/admin
// can change a member's role (admin cannot pick owner; the server is authoritative),
// remove a member, or transfer ownership (owner only). Everyone can leave the org;
// the last-owner 409 is surfaced as a clear message. All mutations are mirrored with
// can("member.manage") and re-checked server-side (RFC-013 section 4.3); a
// viewer/member sees a read-only list.
//
// The seat-limit error from POST /invitations is localized via tDynamic on its
// code + params, falling back to the server message.
@customElement("members-view")
export class MembersView extends AppElement {
  @consume({ context: appContext, subscribe: true })
  private ctx!: AppContext;

  @state() private members: Member[] | null = null;
  @state() private invitations: Invitation[] | null = null;
  @state() private loading = false;
  @state() private error: string | null = null;

  // queued targets for the three confirm dialogs
  @state() private pendingRemove: Member | null = null;
  @state() private pendingTransfer: Member | null = null;
  // per-row in-flight state so the right button shows a spinner / disables
  @state() private busyMember: string | null = null;
  @state() private busyInvite: string | null = null;

  @state() private inviteEmail = "";
  @state() private inviteRole: Role = "member";
  @state() private inviting = false;
  @state() private inviteError: ApiError | null = null;

  private loadedOrgId: string | null = null;

  @query("#remove-dialog") private removeDialog!: ConfirmDialog;
  @query("#transfer-dialog") private transferDialog!: ConfirmDialog;
  @query("#leave-dialog") private leaveDialog!: ConfirmDialog;

  private get orgId(): string | null {
    return this.ctx?.activeOrg?.org_id ?? null;
  }

  private get canManage(): boolean {
    return can(this.ctx?.role ?? null, "member.manage");
  }
  private get canTransfer(): boolean {
    return can(this.ctx?.role ?? null, "org.transfer");
  }
  // Roles offered in the role pickers. Owner is never set here (neither admins nor
  // owners grant it via a role change); ownership moves only through transfer.
  // The server enforces this too, this just keeps it out of the picker.
  private readonly assignableRoles: Role[] = ["admin", "member", "viewer"];

  // Roster grouping: the order the role sections appear in, plus the plural section
  // titles (localized via tDynamic with an English fallback, like the rest of the
  // dynamic labels here).
  private readonly roleOrder: Role[] = ["owner", "admin", "member", "viewer"];
  private readonly roleGroupTitle: Record<Role, [string, string]> = {
    owner: ["members.groupOwners", "Owners"],
    admin: ["members.groupAdmins", "Admins"],
    member: ["members.groupMembers", "Members"],
    viewer: ["members.groupViewers", "Viewers"],
  };

  override updated(): void {
    const orgId = this.orgId;
    if (orgId && orgId !== this.loadedOrgId) void this.load(orgId);
  }

  private async load(orgId: string): Promise<void> {
    this.loadedOrgId = orgId;
    this.loading = true;
    this.error = null;
    try {
      const [members, invitations] = await Promise.all([
        client.listMembers(orgId),
        this.canManage ? client.listInvitations(orgId) : Promise.resolve([]),
      ]);
      this.members = members;
      this.invitations = invitations;
    } catch (err) {
      this.error = err instanceof ApiError ? err.message : t("state.error");
      this.members = null;
    } finally {
      this.loading = false;
    }
  }

  private retry(): void {
    if (this.orgId) void this.load(this.orgId);
  }

  private roleLabel(role: Role): string {
    return t(`role.${role}` as const);
  }

  private isSelf(m: Member): boolean {
    return m.user_id === session.me?.user_id;
  }

  // --- role change ---

  private async onRoleChange(m: Member, role: Role): Promise<void> {
    const orgId = this.orgId;
    if (!orgId || role === m.role) return;
    this.busyMember = m.user_id;
    try {
      const updated = await client.changeMemberRole(orgId, m.user_id, role);
      this.members = (this.members ?? []).map((x) =>
        x.user_id === updated.user_id ? updated : x,
      );
      toast(t("members.roleChanged"), "success");
    } catch (err) {
      toastError(err, t("state.error"));
      // re-pull so the select snaps back to the server's truth
      void this.load(orgId);
    } finally {
      this.busyMember = null;
    }
  }

  // --- remove ---

  private askRemove(m: Member): void {
    this.pendingRemove = m;
    this.removeDialog.open();
  }

  private async onRemoveConfirmed(): Promise<void> {
    const orgId = this.orgId;
    const member = this.pendingRemove;
    this.pendingRemove = null;
    if (!orgId || !member) return;
    this.busyMember = member.user_id;
    try {
      await client.removeMember(orgId, member.user_id);
      this.members = (this.members ?? []).filter(
        (x) => x.user_id !== member.user_id,
      );
      toast(t("members.removed"), "success");
    } catch (err) {
      toastError(err, t("state.error"));
    } finally {
      this.busyMember = null;
    }
  }

  // --- transfer ownership ---

  private askTransfer(m: Member): void {
    this.pendingTransfer = m;
    this.transferDialog.open();
  }

  private async onTransferConfirmed(): Promise<void> {
    const orgId = this.orgId;
    const member = this.pendingTransfer;
    this.pendingTransfer = null;
    if (!orgId || !member) return;
    this.busyMember = member.user_id;
    try {
      await client.transferOwnership(orgId, {
        user_id: member.user_id,
        step_down: true,
      });
      toast(t("members.transferred"), "success");
      // ownership changed for the caller too; pull a fresh /me, then reload
      await this.ctx.refreshMe();
      void this.load(orgId);
    } catch (err) {
      toastError(err, t("state.error"));
    } finally {
      this.busyMember = null;
    }
  }

  // --- leave ---

  private askLeave(): void {
    this.leaveDialog.open();
  }

  private async onLeaveConfirmed(): Promise<void> {
    const orgId = this.orgId;
    if (!orgId) return;
    try {
      await client.leaveOrg(orgId);
      toast(t("members.left"), "success");
      await this.ctx.refreshMe();
      const me = session.me;
      navigate(me && me.orgs.length ? `/orgs/${me.orgs[0].org_id}` : "/account");
    } catch (err) {
      // last owner cannot leave (409): surface the clear localized hint
      const msg =
        err instanceof ApiError && err.status === 409
          ? t("members.leaveLastOwner")
          : err instanceof ApiError
            ? err.message
            : t("state.error");
      toast(msg, "error");
    }
  }

  // --- invite ---

  private async onInvite(e: Event): Promise<void> {
    e.preventDefault();
    const orgId = this.orgId;
    if (!orgId || this.inviting) return;
    const email = this.inviteEmail.trim();
    if (!email || !email.includes("@")) {
      this.inviteError = new ApiError(422, {
        code: "validation_failed",
        message: t("invites.errEmail"),
        fields: { email: t("invites.errEmail") },
      });
      return;
    }
    this.inviting = true;
    this.inviteError = null;
    try {
      const input: InvitationInput = { email, role: this.inviteRole };
      const created = await client.createInvitation(orgId, input);
      this.invitations = [created, ...(this.invitations ?? [])];
      this.inviteEmail = "";
      this.inviteRole = "member";
      toast(t("invites.sent"), "success");
    } catch (err) {
      if (err instanceof ApiError) {
        this.inviteError = err;
        // seat-limit codes carry "seat" and params; localize via the code with
        // a fallback to the server message. Other errors show their own message.
        const msg = err.code.includes("seat")
          ? tDynamic(err.code, err.message || t("invites.seatLimit"), err.params)
          : err.message;
        toast(msg, "error");
      } else {
        toast(t("state.error"), "error");
      }
    } finally {
      this.inviting = false;
    }
  }

  private async onRevoke(inv: Invitation): Promise<void> {
    const orgId = this.orgId;
    if (!orgId) return;
    this.busyInvite = inv.id;
    try {
      await client.revokeInvitation(orgId, inv.id);
      this.invitations = (this.invitations ?? []).filter((x) => x.id !== inv.id);
      toast(t("invites.revoked"), "success");
    } catch (err) {
      toastError(err, t("state.error"));
    } finally {
      this.busyInvite = null;
    }
  }

  private async onResend(inv: Invitation): Promise<void> {
    const orgId = this.orgId;
    if (!orgId) return;
    this.busyInvite = inv.id;
    try {
      const updated = await client.resendInvitation(orgId, inv.id);
      this.invitations = (this.invitations ?? []).map((x) =>
        x.id === updated.id ? updated : x,
      );
      toast(t("invites.resent"), "success");
    } catch (err) {
      toastError(err, t("state.error"));
    } finally {
      this.busyInvite = null;
    }
  }

  // --- render ---

  override render() {
    return html`
      <div class="-mx-6 lg:-mx-10 -my-7">
        ${pageHeader(t("members.heading"), this.leaveButton())}
        <div class="px-6 lg:px-10 py-7">${this.viewBody()}</div>
      </div>
      <confirm-dialog
        id="remove-dialog"
        heading=${t("members.removeHeading")}
        message=${tDynamic("members.removeMessage", "", {
          name: this.pendingRemove?.name ?? this.pendingRemove?.email ?? "",
        })}
        confirmLabel=${t("members.remove")}
        ?danger=${true}
        @confirm=${this.onRemoveConfirmed}
        @cancel=${() => (this.pendingRemove = null)}
      ></confirm-dialog>
      <confirm-dialog
        id="transfer-dialog"
        heading=${t("members.transferHeading")}
        message=${tDynamic("members.transferMessage", "", {
          name: this.pendingTransfer?.name ?? this.pendingTransfer?.email ?? "",
        })}
        confirmLabel=${t("members.transfer")}
        @confirm=${this.onTransferConfirmed}
        @cancel=${() => (this.pendingTransfer = null)}
      ></confirm-dialog>
      <confirm-dialog
        id="leave-dialog"
        heading=${t("members.leaveHeading")}
        message=${t("members.leaveMessage")}
        confirmLabel=${t("members.leave")}
        ?danger=${true}
        @confirm=${this.onLeaveConfirmed}
      ></confirm-dialog>
    `;
  }

  private leaveButton() {
    return html`<button
      class="pulse-btn pulse-btn-ghost pulse-btn-sm"
      @click=${this.askLeave}
    >
      ${icon("logout", "size-4")}${t("members.leave")}
    </button>`;
  }

  private viewBody() {
    return html`<div class="flex flex-col gap-12">
      ${this.membersBody()} ${this.canManage ? this.invitationsSection() : ""}
    </div>`;
  }

  // A thin mono line under the masthead: seats used of the plan cap (the cap is
  // dropped when entitlements have not resolved yet), plus the pending-invite count
  // for owner/admin.
  private seatLine(members: Member[]) {
    const ent = this.ctx?.entitlements;
    const used = ent?.seats_used ?? members.length;
    const cap = ent?.seats_cap ?? null;
    const pending = (this.invitations ?? []).filter(
      (i) => i.state === "pending",
    ).length;
    return html`<div
      class="flex flex-wrap items-center gap-x-5 gap-y-1 font-mono text-[11px] uppercase tracking-[0.12em] text-ink3 mb-7"
    >
      <span class="text-ink2"
        >${cap != null
          ? tDynamic("members.seatsUsage", "{used} of {cap} seats", {
              used,
              cap,
            })
          : tDynamic("members.seatsUsageNoCap", "{used} seats", { used })}</span
      >
      ${this.canManage && pending
        ? html`<span class="text-deg"
            >${tDynamic("members.pendingCount", "{n} pending", {
              n: pending,
            })}</span
          >`
        : ""}
    </div>`;
  }

  private membersBody() {
    if (this.loading && this.members === null) {
      return skeletonRows(5);
    }

    if (this.error) {
      return errorBox(this.error, () => this.retry(), t("state.retry"));
    }

    if (!this.members || this.members.length === 0) {
      return html`<p class="text-ink3">${t("members.empty")}</p>`;
    }

    // Group the roster by role, in the fixed order, skipping a role with nobody in
    // it. A role change moves the member to its new group on the next reload.
    const members = this.members;
    const groups = this.roleOrder
      .map((role) => ({ role, rows: members.filter((m) => m.role === role) }))
      .filter((g) => g.rows.length > 0);

    return html`<div>
      ${this.seatLine(members)}
      <div class="flex flex-col gap-9">
        ${groups.map((g) => this.roleGroup(g.role, g.rows))}
      </div>
    </div>`;
  }

  // One role section: a section title with the headcount, then the member rows.
  private roleGroup(role: Role, rows: Member[]) {
    const [code, fallback] = this.roleGroupTitle[role];
    return html`<section>
      <div
        class="flex items-baseline justify-between border-b border-line pb-2.5 mb-1"
      >
        <h2 class="pulse-section-title">${tDynamic(code, fallback, {})}</h2>
        <span class="font-mono text-[11px] text-ink3"
          >${String(rows.length).padStart(2, "0")}</span
        >
      </div>
      ${rows.map((m) => this.memberRow(m))}
    </section>`;
  }

  // One roster entry: an avatar initial square, the name and email, then the role
  // control (a select only on an editable row; the group heading already states the
  // role otherwise) and the owner/admin actions.
  private memberRow(m: Member) {
    const initial = (m.name?.trim()[0] ?? m.email.trim()[0] ?? "?").toUpperCase();
    return html`<div
      data-member-row
      class="flex items-center gap-4 py-4 border-b border-hair"
    >
      <span
        class="grid place-items-center size-9 shrink-0 bg-brand text-cream font-disp font-bold text-[14px]"
        aria-hidden="true"
        >${initial}</span
      >
      <div class="min-w-0 flex-1">
        <div class="flex items-center gap-2 flex-wrap">
          <span
            class="font-disp font-extrabold text-[17px] tracking-[-0.025em] text-ink truncate"
            >${m.name}</span
          >
          ${this.isSelf(m)
            ? html`<span class="pulse-tag">${t("members.you")}</span>`
            : ""}
        </div>
        <div class="font-mono text-[11.5px] text-ink3 mt-0.5 truncate">
          ${m.email}
        </div>
      </div>
      <div class="flex flex-wrap items-center justify-end gap-3">
        ${this.roleCell(m)} ${this.canManage ? this.memberActions(m) : ""}
      </div>
    </div>`;
  }

  // owner/admin edit a member's role in place via a select; the owner role is not
  // offered (use transfer ownership). The acting user's own row and any owner row
  // are read-only, and the group heading already names their role, so nothing is
  // shown there. Server stays authoritative.
  private roleCell(m: Member) {
    const editable = this.canManage && !this.isSelf(m) && m.role !== "owner";
    if (!editable) {
      return nothing;
    }
    return html`<select
      class="pulse-input min-w-28"
      aria-label=${t("members.colRole")}
      ?disabled=${this.busyMember === m.user_id}
      @change=${(e: Event) =>
        this.onRoleChange(m, (e.target as HTMLSelectElement).value as Role)}
    >
      ${this.assignableRoles.map(
        (role) =>
          html`<option value=${role} ?selected=${role === m.role}>
            ${this.roleLabel(role)}
          </option>`,
      )}
    </select>`;
  }

  private memberActions(m: Member) {
    if (this.isSelf(m)) return html``;
    const busy = this.busyMember === m.user_id;
    return html`<div class="flex items-center justify-end gap-1">
      ${this.canTransfer && m.role !== "owner"
        ? html`<button
            class="pulse-btn pulse-btn-ghost pulse-btn-sm"
            ?disabled=${busy}
            @click=${() => this.askTransfer(m)}
          >
            ${t("members.transfer")}
          </button>`
        : ""}
      ${m.role !== "owner"
        ? html`<button
            class="pulse-iconbtn hover:text-down"
            aria-label=${t("members.remove")}
            ?disabled=${busy}
            @click=${() => this.askRemove(m)}
          >
            ${icon("trash", "size-4")}
          </button>`
        : ""}
    </div>`;
  }

  // --- invitations ---

  private invitationsSection() {
    return html`
      <section class="flex flex-col gap-4">
        <h2 class="pulse-section-title">${t("invites.heading")}</h2>
        ${this.inviteForm()} ${this.invitationsBody()}
      </section>
    `;
  }

  private inviteForm() {
    return html`<form
      class="flex flex-col gap-3 sm:flex-row sm:items-end"
      @submit=${this.onInvite}
    >
      <div class="flex-1">
        <form-field
          label=${t("invites.email")}
          fieldName="invite-email"
          .error=${this.inviteError?.fields?.email ?? null}
          .control=${html`<input
            id="invite-email"
            type="email"
            class="pulse-input w-full"
            .value=${this.inviteEmail}
            @input=${(e: Event) =>
              (this.inviteEmail = (e.target as HTMLInputElement).value)}
            autocomplete="off"
          />`}
        ></form-field>
      </div>
      <form-field
        label=${t("invites.role")}
        fieldName="invite-role"
        .control=${html`<select
          id="invite-role"
          class="pulse-input w-full"
          .value=${this.inviteRole}
          @change=${(e: Event) =>
            (this.inviteRole = (e.target as HTMLSelectElement).value as Role)}
        >
          ${this.assignableRoles.map(
            (role) =>
              html`<option value=${role} ?selected=${role === this.inviteRole}>
                ${this.roleLabel(role)}
              </option>`,
          )}
        </select>`}
      ></form-field>
      <button type="submit" class="pulse-btn" ?disabled=${this.inviting}>
        ${this.inviting ? t("invites.sending") : t("invites.send")}
      </button>
    </form>`;
  }

  private invitationsBody() {
    if (this.invitations === null) {
      return html`<div class="h-12 w-full bg-paper animate-pulse"></div>`;
    }
    if (this.invitations.length === 0) {
      return html`<p class="text-ink3">${t("invites.empty")}</p>`;
    }
    return html`<pulse-ledger
      .items=${this.invitations}
      .renderRow=${(item: unknown, i: number) =>
        this.inviteRow(item as Invitation, i)}
    ></pulse-ledger>`;
  }

  // One indexed invitation row: the index, the invited email with its role tag,
  // the expiry underneath, the state tag, and the resend/revoke actions (pending
  // only). A non-pending invite is muted (ink3 index and a left edge).
  private inviteRow(inv: Invitation, i: number) {
    const n = String(i + 1).padStart(2, "0");
    const pending = inv.state === "pending";
    return html`<div
      data-invite-row
      class="grid grid-cols-[44px_1fr_auto] items-center gap-5 px-6 lg:px-10 py-5 border-b border-hair ${pending
        ? ""
        : "border-l-2 border-l-ink3"}"
    >
      <span
        class="font-mono text-[12px] font-medium ${pending
          ? "text-brand"
          : "text-ink3"}"
        >${n}</span
      >
      <div class="min-w-0">
        <div class="flex items-center gap-2.5 flex-wrap">
          <span
            class="font-disp font-extrabold text-[17px] tracking-[-0.025em] text-ink truncate"
            >${inv.email}</span
          >
          <span class="pulse-tag">${this.roleLabel(inv.role)}</span>
        </div>
        <div class="font-mono text-[11.5px] text-ink3 mt-1">
          ${t("invites.colExpires")}
          <relative-time .datetime=${inv.expires_at}></relative-time>
        </div>
      </div>
      <div class="flex items-center justify-end gap-3">
        <span class="pulse-tag"
          >${t(`inviteState.${inv.state}` as const)}</span
        >
        ${this.inviteActions(inv)}
      </div>
    </div>`;
  }

  private inviteActions(inv: Invitation) {
    if (inv.state !== "pending") return html``;
    const busy = this.busyInvite === inv.id;
    return html`<div class="flex items-center justify-end gap-1.5">
      <button
        class="pulse-btn pulse-btn-ghost pulse-btn-sm"
        ?disabled=${busy}
        @click=${() => this.onResend(inv)}
      >
        ${busy ? t("invites.resending") : t("invites.resend")}
      </button>
      <button
        class="pulse-btn pulse-btn-ghost pulse-btn-sm"
        ?disabled=${busy}
        @click=${() => this.onRevoke(inv)}
      >
        ${t("invites.revoke")}
      </button>
    </div>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    "members-view": MembersView;
  }
}
