import { html } from "lit";
import { customElement, query, state } from "lit/decorators.js";
import { consume } from "@lit/context";
import { AppElement } from "./base.js";
import { appContext, type AppContext } from "../state/context.js";
import { client, ApiError } from "../api/client.js";
import { can } from "../state/can.js";
import { t, tDynamic } from "../i18n.js";
import { toast } from "../toast.js";
import { navigate } from "../router.js";
import { icon } from "../icons.js";
import { session } from "../state/session.js";
import type {
  Invitation,
  InvitationInput,
  Member,
  Role,
} from "../api/types.js";

import "./data-table.js";
import "./confirm-dialog.js";
import "./form-field.js";
import "./relative-time.js";
import type { DataColumn } from "./data-table.js";
import type { ConfirmDialog } from "./confirm-dialog.js";

// Members + invitations (PRD-001, RFC-003). Two blocks under the active org:
// the member list (name/email/role/joined) and pending invitations (list + invite
// form). Owner/admin can change a member's role (admin cannot pick owner; the
// server is authoritative), remove a member, or transfer ownership (owner only).
// Everyone can leave the org; the last-owner 409 is surfaced as a clear message.
// All mutations are mirrored with can("member.manage") and re-checked server-side
// (RFC-013 section 4.3); a viewer/member sees a read-only list.
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

  // invite form
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
      toast(err instanceof ApiError ? err.message : t("state.error"), "error");
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
      toast(err instanceof ApiError ? err.message : t("state.error"), "error");
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
      toast(err instanceof ApiError ? err.message : t("state.error"), "error");
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
      toast(err instanceof ApiError ? err.message : t("state.error"), "error");
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
      toast(err instanceof ApiError ? err.message : t("state.error"), "error");
    } finally {
      this.busyInvite = null;
    }
  }

  // --- render ---

  override render() {
    return html`
      <div class="flex flex-col gap-8">
        <div class="flex items-center justify-between">
          <h1 class="text-2xl font-bold">${t("members.heading")}</h1>
          <button class="btn btn-sm btn-ghost gap-1.5" @click=${this.askLeave}>
            ${icon("logout", "size-4")}${t("members.leave")}
          </button>
        </div>
        ${this.membersBody()} ${this.canManage ? this.invitationsSection() : ""}
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
      </div>
    `;
  }

  private membersBody() {
    if (this.loading && this.members === null) {
      return html`<div class="flex flex-col gap-2" aria-busy="true">
        ${Array.from({ length: 4 }).map(
          () => html`<div class="skeleton h-12 w-full"></div>`,
        )}
      </div>`;
    }

    if (this.error) {
      return html`<div role="alert" class="alert alert-error">
        <span>${this.error}</span>
        <button class="btn btn-sm" @click=${this.retry}>
          ${t("state.retry")}
        </button>
      </div>`;
    }

    if (!this.members || this.members.length === 0) {
      return html`<p class="text-base-content/60">${t("members.empty")}</p>`;
    }

    return html`<data-table
      .columns=${this.memberColumns()}
      .data=${this.members}
      .pageSize=${15}
    ></data-table>`;
  }

  private memberColumns(): DataColumn[] {
    const cols: DataColumn[] = [
      {
        id: "name",
        header: t("members.colName"),
        accessor: (r) => (r as Member).name,
        sortable: true,
        cell: (r) => {
          const m = r as Member;
          return html`<span class="font-medium">${m.name}</span>${this.isSelf(m)
              ? html` <span class="badge badge-ghost badge-sm"
                  >${t("members.you")}</span
                >`
              : ""}`;
        },
      },
      {
        id: "email",
        header: t("members.colEmail"),
        accessor: (r) => (r as Member).email,
        sortable: true,
      },
      {
        id: "role",
        header: t("members.colRole"),
        accessor: (r) => (r as Member).role,
        sortable: true,
        cell: (r) => this.roleCell(r as Member),
      },
      {
        id: "joined",
        header: t("members.colJoined"),
        accessor: (r) => (r as Member).joined_at,
        sortable: true,
        cell: (r) =>
          html`<relative-time .datetime=${(r as Member).joined_at}></relative-time>`,
      },
    ];

    if (this.canManage) {
      cols.push({
        id: "actions",
        header: "",
        class: "text-right",
        cell: (r) => this.memberActions(r as Member),
      });
    }
    return cols;
  }

  // owner/admin edit a member's role in place via a select; the owner role is not
  // offered (use transfer ownership). The acting user's own row and any owner row
  // are read-only here. Server stays authoritative.
  private roleCell(m: Member) {
    const editable = this.canManage && !this.isSelf(m) && m.role !== "owner";
    if (!editable) {
      return html`<span class="badge badge-ghost badge-sm"
        >${this.roleLabel(m.role)}</span
      >`;
    }
    return html`<select
      class="select select-sm select-bordered"
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
            class="btn btn-sm btn-ghost"
            ?disabled=${busy}
            @click=${() => this.askTransfer(m)}
          >
            ${t("members.transfer")}
          </button>`
        : ""}
      ${m.role !== "owner"
        ? html`<button
            class="btn btn-sm btn-ghost btn-square"
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
        <h2 class="text-lg font-semibold">${t("invites.heading")}</h2>
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
            class="input w-full"
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
          class="select w-full"
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
      <button type="submit" class="btn btn-primary" ?disabled=${this.inviting}>
        ${this.inviting ? t("invites.sending") : t("invites.send")}
      </button>
    </form>`;
  }

  private invitationsBody() {
    if (this.invitations === null) {
      return html`<div class="skeleton h-12 w-full"></div>`;
    }
    if (this.invitations.length === 0) {
      return html`<p class="text-base-content/60">${t("invites.empty")}</p>`;
    }
    return html`<data-table
      .columns=${this.inviteColumns()}
      .data=${this.invitations}
      .pageSize=${10}
    ></data-table>`;
  }

  private inviteColumns(): DataColumn[] {
    return [
      {
        id: "email",
        header: t("invites.colEmail"),
        accessor: (r) => (r as Invitation).email,
        sortable: true,
      },
      {
        id: "role",
        header: t("invites.colRole"),
        accessor: (r) => (r as Invitation).role,
        sortable: true,
        cell: (r) =>
          html`<span class="badge badge-ghost badge-sm"
            >${this.roleLabel((r as Invitation).role)}</span
          >`,
      },
      {
        id: "state",
        header: t("invites.colState"),
        accessor: (r) => (r as Invitation).state,
        sortable: true,
        cell: (r) =>
          html`<span class="badge badge-sm"
            >${t(`inviteState.${(r as Invitation).state}` as const)}</span
          >`,
      },
      {
        id: "expires",
        header: t("invites.colExpires"),
        accessor: (r) => (r as Invitation).expires_at,
        sortable: true,
        cell: (r) =>
          html`<relative-time
            .datetime=${(r as Invitation).expires_at}
          ></relative-time>`,
      },
      {
        id: "actions",
        header: "",
        class: "text-right",
        cell: (r) => this.inviteActions(r as Invitation),
      },
    ];
  }

  private inviteActions(inv: Invitation) {
    if (inv.state !== "pending") return html``;
    const busy = this.busyInvite === inv.id;
    return html`<div class="flex items-center justify-end gap-1">
      <button
        class="btn btn-sm btn-ghost"
        ?disabled=${busy}
        @click=${() => this.onResend(inv)}
      >
        ${busy ? t("invites.resending") : t("invites.resend")}
      </button>
      <button
        class="btn btn-sm btn-ghost"
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
