package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/apigen"
	"pulse/internal/authn"
	"pulse/internal/authz"
	"pulse/internal/crypto"
	"pulse/internal/domain"
	"pulse/internal/entitlements"
	"pulse/internal/events"
	"pulse/internal/store"
)

// This file implements the members + invitations slice (PRD-001 5/6/7). The role
// gate is authz.Can (never reimplemented here); the last-owner invariant runs
// through CountOwners + authz guards before the store call, with the DB trigger as
// the backstop; the seat gate runs through internal/entitlements. Every error is
// the localizable {code, message} envelope (RFC-012 / RFC-014).

const inviteTTL = 7 * 24 * time.Hour // PRD-001 6.1: invitations expire after 7 days.

// --- members ---

// ListMembers returns the org's members with role and user info (PRD-001 7.2: every
// role can view the member list). RequireOrg already confirmed membership.
func (s *Server) ListMembers(ctx context.Context, req apigen.ListMembersRequestObject) (apigen.ListMembersResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.ListMembers401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionViewMembers, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.ListMembers403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	members, err := s.store.ListMembers(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	out := make([]apigen.Member, 0, len(members))
	for _, m := range members {
		u, err := s.store.GetUser(ctx, m.UserID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return nil, err
		}
		out = append(out, memberDTO(m, u))
	}
	return apigen.ListMembers200JSONResponse(out), nil
}

// ChangeMemberRole changes a member's role (PRD-001 7.2: owner or admin; admin not
// to/from owner). The last-owner guard reads the owner count in the same flow as
// the change so demoting the only owner is blocked (the DB trigger is the backstop).
func (s *Server) ChangeMemberRole(ctx context.Context, req apigen.ChangeMemberRoleRequestObject) (apigen.ChangeMemberRoleResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.ChangeMemberRole401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if req.Body == nil {
		return apigen.ChangeMemberRole422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	newRole := domain.Role(req.Body.Role)
	if !validRole(newRole) {
		return apigen.ChangeMemberRole422JSONResponse{ValidationFailedJSONResponse: validationFailed("invalid role")}, nil
	}
	targetID, err := strconv.ParseInt(req.UserId, 10, 64)
	if err != nil {
		return apigen.ChangeMemberRole404JSONResponse{NotFoundJSONResponse: notFound("member not found")}, nil
	}

	target, err := s.store.GetMembership(ctx, targetID, p.OrgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.ChangeMemberRole404JSONResponse{NotFoundJSONResponse: notFound("member not found")}, nil
		}
		return nil, err
	}
	owners, err := s.store.CountOwners(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	d := authz.Can(p.Actor(), authz.ActionChangeMemberRole, authz.Resource{
		OrgID:      p.OrgID,
		TargetRole: target.Role,
		NewRole:    newRole,
		OwnerCount: owners,
	})
	if !d.Allowed {
		// The last-owner reasons are a 409 conflict (a state the org cannot be in),
		// the role/admin denials are a 403.
		if isLastOwnerReason(d.Reason) {
			return apigen.ChangeMemberRole409JSONResponse{ConflictJSONResponse: conflict(d.Reason)}, nil
		}
		return apigen.ChangeMemberRole403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	if newRole == target.Role {
		// No-op change: return the current member without touching the DB.
		u, err := s.store.GetUser(ctx, targetID)
		if err != nil {
			return nil, err
		}
		return apigen.ChangeMemberRole200JSONResponse(memberDTO(target, u)), nil
	}

	affected, err := s.store.UpdateMemberRole(ctx, p.OrgID, targetID, newRole)
	if err != nil {
		// The DB trigger backstops the last-owner invariant; surface it as a 409.
		if isLastOwnerDBError(err) {
			return apigen.ChangeMemberRole409JSONResponse{ConflictJSONResponse: conflict("cannot demote the last owner; transfer ownership first")}, nil
		}
		return nil, err
	}
	if affected == 0 {
		return apigen.ChangeMemberRole404JSONResponse{NotFoundJSONResponse: notFound("member not found")}, nil
	}
	_ = s.auth.InvalidateMemberRole(ctx, targetID, p.OrgID)

	target.Role = newRole
	u, err := s.store.GetUser(ctx, targetID)
	if err != nil {
		return nil, err
	}
	return apigen.ChangeMemberRole200JSONResponse(memberDTO(target, u)), nil
}

// RemoveMember removes a member from the org (PRD-001 7.2: owner or admin; admin not
// an owner; never the last owner). Frees the seat (PRD-001 5.1).
func (s *Server) RemoveMember(ctx context.Context, req apigen.RemoveMemberRequestObject) (apigen.RemoveMemberResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.RemoveMember401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	targetID, err := strconv.ParseInt(req.UserId, 10, 64)
	if err != nil {
		return apigen.RemoveMember404JSONResponse{NotFoundJSONResponse: notFound("member not found")}, nil
	}
	target, err := s.store.GetMembership(ctx, targetID, p.OrgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.RemoveMember404JSONResponse{NotFoundJSONResponse: notFound("member not found")}, nil
		}
		return nil, err
	}
	owners, err := s.store.CountOwners(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	d := authz.Can(p.Actor(), authz.ActionRemoveMember, authz.Resource{
		OrgID:      p.OrgID,
		TargetRole: target.Role,
		OwnerCount: owners,
	})
	if !d.Allowed {
		if isLastOwnerReason(d.Reason) {
			return apigen.RemoveMember409JSONResponse{ConflictJSONResponse: conflict(d.Reason)}, nil
		}
		return apigen.RemoveMember403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	affected, err := s.store.RemoveMember(ctx, p.OrgID, targetID)
	if err != nil {
		if isLastOwnerDBError(err) {
			return apigen.RemoveMember409JSONResponse{ConflictJSONResponse: conflict("cannot remove the last owner; transfer ownership first")}, nil
		}
		return nil, err
	}
	if affected == 0 {
		return apigen.RemoveMember404JSONResponse{NotFoundJSONResponse: notFound("member not found")}, nil
	}
	_ = s.auth.InvalidateMemberRole(ctx, targetID, p.OrgID)
	return apigen.RemoveMember204Response{}, nil
}

// LeaveOrg removes the caller's own membership (PRD-001 5.3: any role can leave,
// except the last owner who must transfer ownership or delete the org first).
func (s *Server) LeaveOrg(ctx context.Context, req apigen.LeaveOrgRequestObject) (apigen.LeaveOrgResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.LeaveOrg401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if p.Role == domain.RoleOwner {
		owners, err := s.store.CountOwners(ctx, p.OrgID)
		if err != nil {
			return nil, err
		}
		if d := authz.EnsureNotLastOwner(domain.RoleOwner, owners); !d.Allowed {
			return apigen.LeaveOrg409JSONResponse{ConflictJSONResponse: conflict(d.Reason)}, nil
		}
	}
	affected, err := s.store.RemoveMember(ctx, p.OrgID, p.UserID)
	if err != nil {
		if isLastOwnerDBError(err) {
			return apigen.LeaveOrg409JSONResponse{ConflictJSONResponse: conflict("cannot leave as the last owner; transfer ownership or delete the org")}, nil
		}
		return nil, err
	}
	if affected == 0 {
		// Not a member (should not happen behind RequireOrg) reads as 403.
		return apigen.LeaveOrg403JSONResponse{ForbiddenJSONResponse: forbidden("not a member of this org")}, nil
	}
	_ = s.auth.InvalidateMemberRole(ctx, p.UserID, p.OrgID)
	return apigen.LeaveOrg204Response{}, nil
}

// TransferOwnership hands ownership to another member (PRD-001 4.7, owner only). The
// new owner must already be a member; the acting owner may step down to admin or
// stay a co-owner. Both writes run, then the role cache is busted for both.
func (s *Server) TransferOwnership(ctx context.Context, req apigen.TransferOwnershipRequestObject) (apigen.TransferOwnershipResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.TransferOwnership401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionTransferOwnership, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.TransferOwnership403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	if req.Body == nil || req.Body.UserId == "" {
		return apigen.TransferOwnership422JSONResponse{ValidationFailedJSONResponse: validationFailed("user_id required")}, nil
	}
	targetID, err := strconv.ParseInt(req.Body.UserId, 10, 64)
	if err != nil {
		return apigen.TransferOwnership422JSONResponse{ValidationFailedJSONResponse: validationFailed("invalid user_id")}, nil
	}
	if targetID == p.UserID {
		return apigen.TransferOwnership422JSONResponse{ValidationFailedJSONResponse: validationFailed("cannot transfer ownership to yourself")}, nil
	}
	// The new owner must already be a member (PRD-001 4.7 recommended default).
	if _, err := s.store.GetMembership(ctx, targetID, p.OrgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.TransferOwnership404JSONResponse{NotFoundJSONResponse: notFound("the new owner must already be a member")}, nil
		}
		return nil, err
	}

	// Promote the target to owner first, so the org always has at least one owner at
	// every step (the last-owner trigger never fires on the acting owner's demote).
	if _, err := s.store.UpdateMemberRole(ctx, p.OrgID, targetID, domain.RoleOwner); err != nil {
		return nil, err
	}
	_ = s.auth.InvalidateMemberRole(ctx, targetID, p.OrgID)

	// The acting owner steps down to admin when asked; otherwise they stay a
	// co-owner (PRD-001 4.7: multiple owners are allowed).
	if req.Body.StepDown != nil && *req.Body.StepDown {
		if _, err := s.store.UpdateMemberRole(ctx, p.OrgID, p.UserID, domain.RoleAdmin); err != nil {
			return nil, err
		}
		_ = s.auth.InvalidateMemberRole(ctx, p.UserID, p.OrgID)
	}
	return apigen.TransferOwnership204Response{}, nil
}

// --- invitations ---

// ListInvitations returns the org's pending invitations (PRD-001 7.2: owner/admin
// manage invitations; viewing them is part of managing people, gated by invite role).
func (s *Server) ListInvitations(ctx context.Context, req apigen.ListInvitationsRequestObject) (apigen.ListInvitationsResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.ListInvitations401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageInvitation, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.ListInvitations403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	all, err := s.store.ListInvitations(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	out := make([]apigen.Invitation, 0, len(all))
	for _, inv := range all {
		if inv.State != domain.InvitePending {
			continue
		}
		out = append(out, invitationDTO(inv))
	}
	return apigen.ListInvitations200JSONResponse(out), nil
}

// CreateInvitation invites a teammate by email + role (PRD-001 6.1). It gates on the
// invite-member role, checks a seat is free (a pending invite reserves a seat), and
// emails the tokenized accept link localized to the invite locale. A duplicate
// pending invite for the same email is a 409 (I7).
func (s *Server) CreateInvitation(ctx context.Context, req apigen.CreateInvitationRequestObject) (apigen.CreateInvitationResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.CreateInvitation401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionInviteMember, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.CreateInvitation403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	if req.Body == nil {
		return apigen.CreateInvitation422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	email := strings.TrimSpace(string(req.Body.Email))
	if email == "" {
		return apigen.CreateInvitation422JSONResponse{ValidationFailedJSONResponse: validationFailed("email required")}, nil
	}
	role := domain.Role(req.Body.Role)
	if !validRole(role) {
		return apigen.CreateInvitation422JSONResponse{ValidationFailedJSONResponse: validationFailed("invalid role")}, nil
	}
	// An admin may set the invited role but never invite an owner (the matrix has no
	// "invite owner" path; ownership only arrives via transfer, PRD-001 4.7).
	if role == domain.RoleOwner {
		return apigen.CreateInvitation422JSONResponse{ValidationFailedJSONResponse: validationFailed("cannot invite as owner; use transfer ownership")}, nil
	}

	// Seat gate: accepted members + reserved pending invites must stay under the cap
	// (PRD-001 5.1/5.2). The cap comes from the entitlements resolver, not a literal.
	usage, err := s.seatUsage(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	if !usage.HasFreeSeat() {
		return apigen.CreateInvitation402JSONResponse(seatLimitReached(usage.Cap)), nil
	}

	org, err := s.store.GetOrganization(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}

	locale := org.DefaultLocale
	if req.Body.Locale != nil && *req.Body.Locale != "" {
		locale = *req.Body.Locale
	}
	if locale == "" {
		locale = "en"
	}
	creator := p.UserID
	// The row is created token-less (token_hash NULL); the notifier mints the token
	// when it sends the invite email (RFC-019). The invitee has no link until the email
	// lands, which is invisible to them.
	inv := &domain.Invitation{
		OrgID:     p.OrgID,
		Email:     email,
		Role:      role,
		Locale:    locale,
		CreatedBy: &creator,
		ExpiresAt: time.Now().Add(inviteTTL),
	}
	if _, err := s.store.CreateInvitation(ctx, inv); err != nil {
		if isUniqueViolation(err) {
			return apigen.CreateInvitation409JSONResponse{ConflictJSONResponse: conflict("a pending invitation for this email already exists")}, nil
		}
		return nil, err
	}
	inv.State = domain.InvitePending
	s.publishInviteEmail(ctx, inv, org.Name)
	return apigen.CreateInvitation201JSONResponse(invitationDTO(inv)), nil
}

// RevokeInvitation revokes a pending invitation, freeing its reserved seat
// (PRD-001 6.2). Owner/admin only.
func (s *Server) RevokeInvitation(ctx context.Context, req apigen.RevokeInvitationRequestObject) (apigen.RevokeInvitationResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.RevokeInvitation401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageInvitation, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.RevokeInvitation403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	inviteID, err := strconv.ParseInt(req.Id, 10, 64)
	if err != nil {
		return apigen.RevokeInvitation404JSONResponse{NotFoundJSONResponse: notFound("invitation not found")}, nil
	}
	if _, err := s.store.GetInvitation(ctx, p.OrgID, inviteID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.RevokeInvitation404JSONResponse{NotFoundJSONResponse: notFound("invitation not found")}, nil
		}
		return nil, err
	}
	affected, err := s.store.RevokeInvitation(ctx, p.OrgID, inviteID)
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		// The row exists but is not pending: a terminal invite cannot be revoked.
		return apigen.RevokeInvitation409JSONResponse{ConflictJSONResponse: conflict("invitation is no longer pending")}, nil
	}
	return apigen.RevokeInvitation204Response{}, nil
}

// ResendInvitation refreshes a pending invitation's token and 7-day expiry and
// re-sends the email (PRD-001 6.2: same row, new token, refreshed clock; the seat
// stays reserved). Owner/admin only.
func (s *Server) ResendInvitation(ctx context.Context, req apigen.ResendInvitationRequestObject) (apigen.ResendInvitationResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.ResendInvitation401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageInvitation, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.ResendInvitation403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	inviteID, err := strconv.ParseInt(req.Id, 10, 64)
	if err != nil {
		return apigen.ResendInvitation404JSONResponse{NotFoundJSONResponse: notFound("invitation not found")}, nil
	}
	inv, err := s.store.GetInvitation(ctx, p.OrgID, inviteID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.ResendInvitation404JSONResponse{NotFoundJSONResponse: notFound("invitation not found")}, nil
		}
		return nil, err
	}
	if inv.State != domain.InvitePending {
		return apigen.ResendInvitation409JSONResponse{ConflictJSONResponse: conflict("invitation is no longer pending")}, nil
	}
	newExpiry := time.Now().Add(inviteTTL)
	affected, err := s.store.ResendInvitation(ctx, p.OrgID, inviteID, newExpiry)
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return apigen.ResendInvitation409JSONResponse{ConflictJSONResponse: conflict("invitation is no longer pending")}, nil
	}
	org, err := s.store.GetOrganization(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	inv.ExpiresAt = newExpiry
	s.publishInviteEmail(ctx, inv, org.Name)
	return apigen.ResendInvitation200JSONResponse(invitationDTO(inv)), nil
}

// --- accept flow (token-based, not org-scoped) ---

// GetInvitationPreview renders the pre-login accept page (PRD-001 6.5): it loads the
// invitation by its token capability (no org scope, no session) and returns the org
// name, role, and inviter. A terminal invite is a 409 so the page can show "no
// longer valid"; an unknown token is a 404.
func (s *Server) GetInvitationPreview(ctx context.Context, req apigen.GetInvitationPreviewRequestObject) (apigen.GetInvitationPreviewResponseObject, error) {
	inv, err := s.store.GetInvitationByToken(ctx, crypto.HashToken(req.Token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.GetInvitationPreview404JSONResponse{NotFoundJSONResponse: notFound("invitation not found")}, nil
		}
		return nil, err
	}
	if inv.State != domain.InvitePending || time.Now().After(inv.ExpiresAt) {
		return apigen.GetInvitationPreview409JSONResponse{ConflictJSONResponse: conflict("this invitation is no longer valid")}, nil
	}
	org, err := s.store.GetOrganization(ctx, inv.OrgID)
	if err != nil {
		return nil, err
	}
	preview := apigen.InvitationPreview{
		OrgName: org.Name,
		Role:    apigen.Role(inv.Role),
		State:   apigen.InvitationState(inv.State),
		Email:   inv.Email,
	}
	if inv.CreatedBy != nil {
		if u, err := s.store.GetUser(ctx, *inv.CreatedBy); err == nil {
			name := u.Name
			preview.InviterName = &name
		}
	}
	return apigen.GetInvitationPreview200JSONResponse(preview), nil
}

// AcceptInvitation completes the join (PRD-001 6.2/6.3). It requires a signed-in
// session whose verified email matches the invited email, then atomically flips the
// invite to accepted and creates the membership (the reserved seat becomes occupied).
func (s *Server) AcceptInvitation(ctx context.Context, req apigen.AcceptInvitationRequestObject) (apigen.AcceptInvitationResponseObject, error) {
	pr, ok := authn.FromContext(ctx)
	if !ok || pr.Kind != authz.ActorHuman {
		return apigen.AcceptInvitation401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	inv, err := s.store.GetInvitationByToken(ctx, crypto.HashToken(req.Token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.AcceptInvitation404JSONResponse{NotFoundJSONResponse: notFound("invitation not found")}, nil
		}
		return nil, err
	}
	if inv.State != domain.InvitePending || time.Now().After(inv.ExpiresAt) {
		return apigen.AcceptInvitation409JSONResponse{ConflictJSONResponse: conflict("this invitation is no longer valid")}, nil
	}

	// Email-match guard (PRD-001 6.3): the signed-in verified email must match the
	// invited email. The match is case-insensitive against a verified account email.
	u, err := s.store.GetUser(ctx, pr.UserID)
	if err != nil {
		return nil, err
	}
	if !u.EmailVerified || !strings.EqualFold(u.Email, inv.Email) {
		return apigen.AcceptInvitation403JSONResponse{ForbiddenJSONResponse: forbidden(fmt.Sprintf("this invite was sent to %s; sign in with that account or ask for a new invite", inv.Email))}, nil
	}

	// Already a member: a no-op success that closes the pending invite and frees its
	// seat (PRD-001 6.3 edge). The org is returned with the existing role.
	if existing, err := s.store.GetMembership(ctx, pr.UserID, inv.OrgID); err == nil {
		_, _ = s.store.RevokeInvitation(ctx, inv.OrgID, inv.ID)
		org, err := s.store.GetOrganization(ctx, inv.OrgID)
		if err != nil {
			return nil, err
		}
		return apigen.AcceptInvitation200JSONResponse(membershipDTO(org, existing.Role)), nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	if _, err := s.store.AcceptInvitation(ctx, inv.OrgID, inv.ID, pr.UserID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Lost the race: another accept/revoke flipped it out of pending.
			return apigen.AcceptInvitation409JSONResponse{ConflictJSONResponse: conflict("this invitation is no longer valid")}, nil
		}
		return nil, err
	}
	_ = s.auth.InvalidateMemberRole(ctx, pr.UserID, inv.OrgID)
	org, err := s.store.GetOrganization(ctx, inv.OrgID)
	if err != nil {
		return nil, err
	}
	return apigen.AcceptInvitation200JSONResponse(membershipDTO(org, inv.Role)), nil
}

// --- helpers ---

// humanInOrg returns the principal when it is a human with an org and role resolved
// (RequireOrg ran). An API-key actor also reaches here with org+role from the key.
func (s *Server) humanInOrg(ctx context.Context) (authn.Principal, bool) {
	p, ok := authn.FromContext(ctx)
	if !ok || p.OrgID == 0 || p.Role == "" {
		return authn.Principal{}, false
	}
	return p, true
}

// seatUsage builds the seat meter for an org: accepted members + pending invites
// against the plan cap (PRD-001 5.1). The plan is read off the org (free until the
// billing catalog lands); the cap comes from the entitlements resolver.
func (s *Server) seatUsage(ctx context.Context, orgID int64) (entitlements.SeatUsage, error) {
	members, err := s.store.CountMembers(ctx, orgID)
	if err != nil {
		return entitlements.SeatUsage{}, err
	}
	pending, err := s.store.CountPendingInvitations(ctx, orgID)
	if err != nil {
		return entitlements.SeatUsage{}, err
	}
	cap := s.seats.SeatCap(orgID, s.orgPlan(ctx, orgID))
	return entitlements.SeatUsage{Used: members + pending, Cap: cap}, nil
}

// publishInviteEmail publishes the invite intent; the notifier mints the accept token
// and sends the localized email (RFC-019). Keyed by org so an org's intents stay in
// order. Best-effort: the row is already created/refreshed, so a publish failure is
// swallowed (the owner can resend). A nil publisher (dev/test without a bus) is a no-op.
func (s *Server) publishInviteEmail(ctx context.Context, inv *domain.Invitation, orgName string) {
	if s.email == nil {
		return
	}
	_ = s.email.PublishEmail(ctx, strconv.FormatInt(inv.OrgID, 10), events.EmailIntent{
		Type:   events.EmailInvitation,
		Locale: inv.Locale,
		Invitation: &events.InvitationRequested{
			InvitationID: inv.ID,
			OrgID:        inv.OrgID,
			OrgName:      orgName,
			Inviter:      s.inviterDisplay(ctx, inv),
			Role:         string(inv.Role),
			Email:        inv.Email,
		},
	})
}

// inviterDisplay resolves who sent the invite into a display string for the email,
// e.g. "Jane Doe (jane@acme.com)", or just the email when no name is set. It returns
// "" when there is no creator on record or the lookup fails, so the invite copy
// falls back to its passive phrasing rather than blocking the send.
func (s *Server) inviterDisplay(ctx context.Context, inv *domain.Invitation) string {
	if inv.CreatedBy == nil {
		return ""
	}
	u, err := s.store.GetUser(ctx, *inv.CreatedBy)
	if err != nil {
		return ""
	}
	if u.Name == "" {
		return u.Email
	}
	return fmt.Sprintf("%s (%s)", u.Name, u.Email)
}

// memberDTO maps a membership + user to the API Member shape.
func memberDTO(m *domain.Membership, u *domain.User) apigen.Member {
	var avatar *string
	if u.AvatarURL != "" {
		a := u.AvatarURL
		avatar = &a
	}
	return apigen.Member{
		UserId:    strconv.FormatInt(u.ID, 10),
		Email:     u.Email,
		Name:      u.Name,
		AvatarUrl: avatar,
		Role:      apigen.Role(m.Role),
		JoinedAt:  m.CreatedAt,
	}
}

// invitationDTO maps an invitation to the API Invitation shape.
func invitationDTO(inv *domain.Invitation) apigen.Invitation {
	var invitedBy *string
	if inv.CreatedBy != nil {
		s := strconv.FormatInt(*inv.CreatedBy, 10)
		invitedBy = &s
	}
	return apigen.Invitation{
		Id:        strconv.FormatInt(inv.ID, 10),
		Email:     inv.Email,
		Role:      apigen.Role(inv.Role),
		State:     apigen.InvitationState(inv.State),
		CreatedAt: inv.CreatedAt,
		ExpiresAt: inv.ExpiresAt,
		InvitedBy: invitedBy,
	}
}

// membershipDTO maps an org + role to the OrgMembership shape (mirrors
// orgMembershipDTO; the accept flow already holds the *domain.Organization).
func membershipDTO(org *domain.Organization, role domain.Role) apigen.OrgMembership {
	return orgMembershipDTO(org, role)
}

// seatLimitReached is the localizable upsell envelope (PRD-001 5.2): a stable code
// the FE maps to upsell copy, with the plan's seat limit in fields so the message
// can be interpolated client-side (RFC-014).
func seatLimitReached(limit int) apigen.ErrorResponse {
	fields := map[string]string{"limit": strconv.Itoa(limit)}
	return apigen.ErrorResponse{Error: apigen.Error{
		Code:    "seat_limit_reached",
		Message: fmt.Sprintf("your plan allows %d seats; remove a member or invite, or upgrade to add more", limit),
		Fields:  &fields,
	}}
}

// validRole reports whether r is one of the four roles.
func validRole(r domain.Role) bool {
	switch r {
	case domain.RoleOwner, domain.RoleAdmin, domain.RoleMember, domain.RoleViewer:
		return true
	}
	return false
}

// isLastOwnerReason reports whether a deny reason is the at-least-one-owner
// invariant (a 409 conflict), as opposed to a plain role/admin denial (a 403).
func isLastOwnerReason(reason string) bool {
	return strings.Contains(reason, "last owner")
}

// isLastOwnerDBError reports whether err is the last-owner trigger backstop firing.
func isLastOwnerDBError(err error) bool {
	return store.IsLastOwnerViolation(err)
}

// isUniqueViolation reports whether err is a unique-constraint clash (e.g. a
// duplicate pending invite, I7).
func isUniqueViolation(err error) bool {
	return store.IsUniqueViolation(err)
}
