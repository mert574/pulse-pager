package authz

import (
	"testing"

	"pulse/internal/domain"
)

// TestMatrix asserts the PRD-001 section 7.2 permission matrix exactly, for all
// four roles across every action, including the owner-only existential rows and
// the admin-not-owner limits. The expected map below is the matrix transcribed.
func TestMatrix(t *testing.T) {
	const org = int64(1)

	// expected[action][role] = allowed. Transcribed from PRD-001 7.2 / RFC-003 7.2.
	expected := map[Action]map[domain.Role]bool{
		ActionViewMonitoring:     {domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: true, domain.RoleViewer: true},
		ActionViewMembers:        {domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: true, domain.RoleViewer: true},
		ActionCreateOrganization: {domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: true, domain.RoleViewer: true},

		ActionManageMonitors:    {domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: true, domain.RoleViewer: false},
		ActionManageChannels:    {domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: true, domain.RoleViewer: false},
		ActionAckIncident:       {domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: true, domain.RoleViewer: false},
		ActionManageStatusPages: {domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: true, domain.RoleViewer: false},

		ActionCloseIncident:         {domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: false, domain.RoleViewer: false},
		ActionConfigureCustomDomain: {domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: false, domain.RoleViewer: false},
		ActionInviteMember:          {domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: false, domain.RoleViewer: false},
		ActionManageInvitation:      {domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: false, domain.RoleViewer: false},
		ActionViewAuditLog:          {domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: false, domain.RoleViewer: false},
		ActionEditOrgSettings:       {domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: false, domain.RoleViewer: false},
		ActionManageAPIKeys:         {domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: false, domain.RoleViewer: false},
		ActionViewBilling:           {domain.RoleOwner: true, domain.RoleAdmin: true, domain.RoleMember: false, domain.RoleViewer: false},

		// owner-only existential actions
		ActionManageBilling:      {domain.RoleOwner: true, domain.RoleAdmin: false, domain.RoleMember: false, domain.RoleViewer: false},
		ActionTransferOwnership:  {domain.RoleOwner: true, domain.RoleAdmin: false, domain.RoleMember: false, domain.RoleViewer: false},
		ActionDeleteOrganization: {domain.RoleOwner: true, domain.RoleAdmin: false, domain.RoleMember: false, domain.RoleViewer: false},
	}

	roles := []domain.Role{domain.RoleOwner, domain.RoleAdmin, domain.RoleMember, domain.RoleViewer}

	for action, perRole := range expected {
		for _, role := range roles {
			want := perRole[role]
			actor := Actor{Kind: ActorHuman, UserID: 7, OrgID: org, Role: role}
			// For change-role/remove the bare allow is tested with a non-owner
			// target and >1 owners so the guards do not interfere; those guards
			// have their own focused tests below.
			res := Resource{OrgID: org, TargetRole: domain.RoleMember, NewRole: domain.RoleMember, OwnerCount: 2}
			got := Can(actor, action, res).Allowed
			if got != want {
				t.Errorf("Can(role=%s, action=%s) = %v, want %v", role, action, got, want)
			}
		}
	}

	// change-role and remove are admin+ at the bare level (member/viewer denied).
	for _, action := range []Action{ActionChangeMemberRole, ActionRemoveMember} {
		cases := map[domain.Role]bool{
			domain.RoleOwner: true, domain.RoleAdmin: true,
			domain.RoleMember: false, domain.RoleViewer: false,
		}
		for role, want := range cases {
			actor := Actor{Kind: ActorHuman, OrgID: org, Role: role}
			res := Resource{OrgID: org, TargetRole: domain.RoleMember, NewRole: domain.RoleAdmin, OwnerCount: 2}
			if got := Can(actor, action, res).Allowed; got != want {
				t.Errorf("Can(role=%s, action=%s) = %v, want %v", role, action, got, want)
			}
		}
	}
}

func TestAdminCannotTouchOwner(t *testing.T) {
	const org = int64(1)
	admin := Actor{Kind: ActorHuman, OrgID: org, Role: domain.RoleAdmin}

	// admin changing a role TO owner is denied
	if Can(admin, ActionChangeMemberRole, Resource{OrgID: org, TargetRole: domain.RoleMember, NewRole: domain.RoleOwner, OwnerCount: 1}).Allowed {
		t.Error("admin must not promote a member to owner")
	}
	// admin changing a role FROM owner is denied
	if Can(admin, ActionChangeMemberRole, Resource{OrgID: org, TargetRole: domain.RoleOwner, NewRole: domain.RoleAdmin, OwnerCount: 2}).Allowed {
		t.Error("admin must not demote an owner")
	}
	// admin removing an owner is denied
	if Can(admin, ActionRemoveMember, Resource{OrgID: org, TargetRole: domain.RoleOwner, OwnerCount: 2}).Allowed {
		t.Error("admin must not remove an owner")
	}
	// admin removing a member is allowed
	if !Can(admin, ActionRemoveMember, Resource{OrgID: org, TargetRole: domain.RoleMember, OwnerCount: 1}).Allowed {
		t.Error("admin should be able to remove a member")
	}
}

func TestLastOwnerInvariant(t *testing.T) {
	const org = int64(1)
	owner := Actor{Kind: ActorHuman, OrgID: org, Role: domain.RoleOwner}

	// owner demoting the last owner (themselves) is denied
	if Can(owner, ActionChangeMemberRole, Resource{OrgID: org, TargetRole: domain.RoleOwner, NewRole: domain.RoleAdmin, OwnerCount: 1}).Allowed {
		t.Error("demoting the last owner must be denied")
	}
	// with two owners, demoting one is allowed
	if !Can(owner, ActionChangeMemberRole, Resource{OrgID: org, TargetRole: domain.RoleOwner, NewRole: domain.RoleAdmin, OwnerCount: 2}).Allowed {
		t.Error("demoting one of two owners should be allowed")
	}
	// owner removing the last owner is denied
	if Can(owner, ActionRemoveMember, Resource{OrgID: org, TargetRole: domain.RoleOwner, OwnerCount: 1}).Allowed {
		t.Error("removing the last owner must be denied")
	}

	// the standalone helper
	if EnsureNotLastOwner(domain.RoleOwner, 1).Allowed {
		t.Error("EnsureNotLastOwner should deny removing the last owner")
	}
	if !EnsureNotLastOwner(domain.RoleOwner, 2).Allowed {
		t.Error("EnsureNotLastOwner should allow when other owners exist")
	}
	if !EnsureNotLastOwner(domain.RoleMember, 1).Allowed {
		t.Error("EnsureNotLastOwner should allow removing a non-owner")
	}
}

func TestAPIKeyCeiling(t *testing.T) {
	const org = int64(1)
	// even an admin-roled key cannot reach owner-only actions
	key := Actor{Kind: ActorAPIKey, KeyID: 9, OrgID: org, Role: domain.RoleAdmin}
	for _, action := range []Action{ActionManageBilling, ActionTransferOwnership, ActionDeleteOrganization} {
		if Can(key, action, Resource{OrgID: org}).Allowed {
			t.Errorf("api key must not be allowed owner-only action %s", action)
		}
	}
	// but an admin key can do admin actions
	if !Can(key, ActionManageAPIKeys, Resource{OrgID: org}).Allowed {
		t.Error("admin api key should manage api keys")
	}
}

func TestCrossOrgDenied(t *testing.T) {
	owner := Actor{Kind: ActorHuman, OrgID: 1, Role: domain.RoleOwner}
	// a resource in another org is denied even for an owner
	if Can(owner, ActionManageMonitors, Resource{OrgID: 2}).Allowed {
		t.Error("acting on another org's resource must be denied")
	}
	// create-org is per-user, not org-scoped, so it is not blocked by org match
	if !Can(owner, ActionCreateOrganization, Resource{}).Allowed {
		t.Error("create org should be allowed regardless of org match")
	}
}
