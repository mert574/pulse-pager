// Package authz is the role gate: the RBAC permission matrix from PRD-001 section
// 7.2 (and the master section 4 matrix) expressed as a permission set per role,
// plus the pure Can decision (RFC-003 section 7). It owns the decision and nothing
// about identity: it imports nothing from authn and does no I/O, so the middleware
// resolves (org, role) first (RFC-003 section 6), reads whatever resource fields a
// guard needs, then calls Can. Purity makes the matrix exhaustively unit-testable.
//
// This is only the role gate. The entitlement gate (does the org's plan allow it?)
// is a separate gate in internal/entitlements; a write must pass both (RFC-003 7.4).
package authz

import "pulse/internal/domain"

// Action is a named capability, the rows of the PRD-001 section 7.2 matrix.
type Action string

const (
	// Read across the monitoring surface (every role, any key).
	ActionViewMonitoring Action = "view_monitoring"
	// Monitors: create/edit/delete, run check-now (member+).
	ActionManageMonitors Action = "manage_monitors"
	// Channels: create/edit/delete, send test (member+).
	ActionManageChannels Action = "manage_channels"
	// Acknowledge or annotate an incident (member+).
	ActionAckIncident Action = "ack_incident"
	// Manually close an incident (admin+; member cannot, PRD-001 7.2).
	ActionCloseIncident Action = "close_incident"
	// Status pages: create/edit/publish (member+).
	ActionManageStatusPages Action = "manage_status_pages"
	// Configure a custom domain for a status page (admin+).
	ActionConfigureCustomDomain Action = "configure_custom_domain"

	// View the member list and roles (every role).
	ActionViewMembers Action = "view_members"
	// Invite a member and set the invited role (admin+).
	ActionInviteMember Action = "invite_member"
	// Resend or revoke a pending invitation (admin+).
	ActionManageInvitation Action = "manage_invitation"
	// Change a member's role (admin+; admin not to/from owner, guarded).
	ActionChangeMemberRole Action = "change_member_role"
	// Remove a member (admin+; admin not an owner, guarded).
	ActionRemoveMember Action = "remove_member"

	// View the audit log (admin+).
	ActionViewAuditLog Action = "view_audit_log"
	// Edit org settings: name, slug, defaults (admin+).
	ActionEditOrgSettings Action = "edit_org_settings"
	// Create or revoke an API key (admin+).
	ActionManageAPIKeys Action = "manage_api_keys"
	// Register/rotate/disable/delete an org-level outbound webhook (admin+,
	// PRD-005 4.6 / 7.4). Like API keys, never reachable by a member key.
	ActionManageWebhooks Action = "manage_webhooks"
	// View billing and usage (admin+, read only).
	ActionViewBilling Action = "view_billing"

	// Owner-only existential actions (never reachable by an API key, PRD-001 App A).
	ActionManageBilling      Action = "manage_billing"      // plan, payment, invoices
	ActionTransferOwnership  Action = "transfer_ownership"  // hand the org to another member
	ActionDeleteOrganization Action = "delete_organization" // end the org

	// Per-user, not org-scoped: any role can create a new org (PRD-001 7.2).
	ActionCreateOrganization Action = "create_organization"
)

// Actor is the already-resolved request principal the middleware hands to Can
// (RFC-003 7.3). Kind separates a human from an API key so guards can apply the
// key ceiling (no owner-equivalent key).
type Actor struct {
	Kind   ActorKind
	UserID int64 // set for a human actor
	KeyID  int64 // set for an API-key actor
	OrgID  int64 // the active org the request resolved to
	Role   domain.Role
}

// ActorKind is how the principal authenticated.
type ActorKind string

const (
	ActorHuman  ActorKind = "human"
	ActorAPIKey ActorKind = "api_key"
)

// Resource is the target an action touches. Most actions only need OrgID (matched
// against the actor's org). Member/role actions also carry the target's role and
// the org's owner count so the owner-only and at-least-one-owner guards can run
// without any I/O inside Can (RFC-003 7.5).
type Resource struct {
	OrgID      int64       // the resource's org; must equal the actor's org
	TargetRole domain.Role // for change-role/remove: the member being acted on
	// NewRole is the role a change-role action would set. Empty for non-role actions.
	NewRole domain.Role
	// OwnerCount is the org's current owner count, read in the same transaction as
	// the action. The last-owner guard uses it (RFC-003 7.5).
	OwnerCount int
}

// Decision is the result of Can. A deny carries a short machine reason that maps
// to the forbidden (403) envelope (RFC-003 7.3).
type Decision struct {
	Allowed bool
	Reason  string // empty when allowed
}

func allow() Decision             { return Decision{Allowed: true} }
func deny(reason string) Decision { return Decision{Allowed: false, Reason: reason} }

// roleActions is the permission-set-per-role table (RFC-003 7.2). A role's set is
// every action it may take. A higher role is a superset of the lower where the
// matrix says so, except the deliberately owner-only existential actions, which
// are listed only under owner. The four self-scoped account actions (profile,
// sessions, log-out-all, delete-own-account) are role-independent ("actor ==
// subject", PRD-001 7.3) and are not in this table.
var roleActions = map[domain.Role]map[Action]bool{
	domain.RoleViewer: setOf(
		ActionViewMonitoring,
		ActionViewMembers,
		ActionCreateOrganization,
	),
	domain.RoleMember: setOf(
		ActionViewMonitoring,
		ActionViewMembers,
		ActionCreateOrganization,
		ActionManageMonitors,
		ActionManageChannels,
		ActionAckIncident,
		ActionManageStatusPages,
	),
	domain.RoleAdmin: setOf(
		ActionViewMonitoring,
		ActionViewMembers,
		ActionCreateOrganization,
		ActionManageMonitors,
		ActionManageChannels,
		ActionAckIncident,
		ActionManageStatusPages,
		ActionCloseIncident,
		ActionConfigureCustomDomain,
		ActionInviteMember,
		ActionManageInvitation,
		ActionChangeMemberRole,
		ActionRemoveMember,
		ActionViewAuditLog,
		ActionEditOrgSettings,
		ActionManageAPIKeys,
		ActionManageWebhooks,
		ActionViewBilling,
	),
	domain.RoleOwner: setOf(
		ActionViewMonitoring,
		ActionViewMembers,
		ActionCreateOrganization,
		ActionManageMonitors,
		ActionManageChannels,
		ActionAckIncident,
		ActionManageStatusPages,
		ActionCloseIncident,
		ActionConfigureCustomDomain,
		ActionInviteMember,
		ActionManageInvitation,
		ActionChangeMemberRole,
		ActionRemoveMember,
		ActionViewAuditLog,
		ActionEditOrgSettings,
		ActionManageAPIKeys,
		ActionManageWebhooks,
		ActionViewBilling,
		// owner-only existential actions:
		ActionManageBilling,
		ActionTransferOwnership,
		ActionDeleteOrganization,
	),
}

func setOf(actions ...Action) map[Action]bool {
	s := make(map[Action]bool, len(actions))
	for _, a := range actions {
		s[a] = true
	}
	return s
}

// Can decides whether the actor may take the action on the resource. It is the one
// decision seam (RFC-003 7.3): look up whether the actor's role set contains the
// action, then apply action-specific guards that need the resource (target-org ==
// actor-org, admin-cannot-touch-owner, at-least-one-owner). Pure, no I/O.
func Can(actor Actor, action Action, resource Resource) Decision {
	// Cross-org guard: a request can never act on another org's resource. The org
	// is checked against membership in the middleware; this is the in-handler
	// backstop (RFC-003 section 10 cross-org escalation). CreateOrganization is
	// per-user, not org-scoped, so it skips the match.
	if action != ActionCreateOrganization && resource.OrgID != 0 && resource.OrgID != actor.OrgID {
		return deny("resource belongs to another org")
	}

	// An API key never reaches the owner-only existential actions, regardless of
	// the role stamped on it: keys max out at admin (PRD-001 App A, RFC-003 5.4).
	if actor.Kind == ActorAPIKey && ownerOnly(action) {
		return deny("api keys cannot perform owner-only actions")
	}

	if !roleActions[actor.Role][action] {
		return deny("role not permitted")
	}

	// Action-specific guards that need the resource.
	switch action {
	case ActionChangeMemberRole:
		return guardChangeRole(actor, resource)
	case ActionRemoveMember:
		return guardRemoveMember(actor, resource)
	}
	return allow()
}

func ownerOnly(action Action) bool {
	switch action {
	case ActionManageBilling, ActionTransferOwnership, ActionDeleteOrganization:
		return true
	}
	return false
}

// guardChangeRole encodes the admin restriction (an admin may change roles but
// never to or from owner, PRD-001 7.2) and the last-owner invariant (an owner
// cannot be demoted away from owner if they are the only one, I1 / RFC-003 7.5).
func guardChangeRole(actor Actor, r Resource) Decision {
	if actor.Role == domain.RoleAdmin {
		if r.TargetRole == domain.RoleOwner || r.NewRole == domain.RoleOwner {
			return deny("admin cannot change a role to or from owner")
		}
	}
	// Demoting the last owner away from owner breaks I1.
	if r.TargetRole == domain.RoleOwner && r.NewRole != domain.RoleOwner && isLastOwner(r) {
		return deny("cannot demote the last owner; transfer ownership first")
	}
	return allow()
}

// guardRemoveMember encodes that an admin may remove a member but never an owner
// (PRD-001 7.2), and that the last owner cannot be removed (I1 / RFC-003 7.5).
func guardRemoveMember(actor Actor, r Resource) Decision {
	if actor.Role == domain.RoleAdmin && r.TargetRole == domain.RoleOwner {
		return deny("admin cannot remove an owner")
	}
	if r.TargetRole == domain.RoleOwner && isLastOwner(r) {
		return deny("cannot remove the last owner; transfer ownership first")
	}
	return allow()
}

// isLastOwner reports whether the target is the only owner of the org. The handler
// supplies OwnerCount read in the same transaction as the action (RFC-003 7.5).
func isLastOwner(r Resource) bool {
	return r.OwnerCount <= 1
}

// EnsureNotLastOwner is the at-least-one-owner invariant guard helper exposed for
// handlers that end a membership outside the change-role/remove paths (a last owner
// trying to leave, PRD-001 4.6). It returns an allow only when removing the target
// owner would still leave the org with an owner. The DB trigger is the backstop
// (schema.sql trg_memberships_last_owner); this is the friendly-message guard.
func EnsureNotLastOwner(targetRole domain.Role, ownerCount int) Decision {
	if targetRole == domain.RoleOwner && ownerCount <= 1 {
		return deny("cannot remove the last owner; transfer ownership or delete the org")
	}
	return allow()
}
