# PRD-001 - Identity and Tenancy (Sub-PRD)

Status: Draft for architecture and build
Owner: Product (Principal PM)
Parent: `docs/PRD.md` (Pulse master PRD v2.1)
Scope: the identity, account, organization, membership, seat, invitation, and RBAC domain
Depends on: PRD-006 Billing (seats, plans, entitlements), PRD-005 Public API (key auth), per master sections 9 and 11

This sub-PRD goes deeper than the master on one domain: who a user is, how they sign in, how organizations are formed and governed, and exactly what each role can do. Where it builds on the master it cites the master section number in the form (master 3), (master 4), etc. It does not change any locked decision in the master. Where the master left a decision open (master 16), this document either picks the master's recommended default or states the deeper rule and flags it in section 13.

---

## 1. Overview, goals, non-goals

### 1.1 Overview

Pulse is multi-tenant. The unit of isolation and ownership is the organization (master 3). A user is a global person who signs in only with Google or GitHub (master 5), can belong to many organizations, and carries a role per organization (master 4). Every resource (monitors, channels, incidents, status pages, API keys, billing) belongs to exactly one org and is never reachable across orgs (master 13.1).

This domain owns the front door (sign-in), the account (profile, linked providers, sessions), the org lifecycle (create, switch, settings, transfer, delete), membership and seats, the invitation flow, and the role-based permission model that every other domain reads from.

### 1.2 Goals

| # | Goal | Why |
|---|------|-----|
| G1 | Sign-in to first org in one click, no passwords | Activation is the North Star input (master 14); social-only removes password breach class (master 5) |
| G2 | Every user always has at least one org | No "you have no workspace" dead end (master 3) |
| G3 | One person, one Pulse user, even with two providers | Account linking on verified email (master 5) |
| G4 | Org is a hard isolation boundary | Top security invariant (master 13.1) |
| G5 | Roles map to real separation of duties (pay / configure / operate / watch) | Four roles, no custom-role complexity in v1 (master 4) |
| G6 | Invite a teammate by email and have them productive in minutes | Team persona job (master 2 persona B) |
| G7 | At least one owner always exists on every org | Governance invariant (master 4) |
| G8 | Account and org deletion honor GDPR with no orphaned owned data | master 13 GDPR |

### 1.3 Non-goals (v1)

| # | Non-goal | Where it lives |
|---|----------|----------------|
| N1 | Email + password login, password reset | Out forever in v1; social-only (master 5) |
| N2 | SSO (SAML/OIDC) and SCIM provisioning | Phase 3 enterprise (master 15) |
| N3 | Custom roles / fine-grained permissions | Phase 3 (master 4) |
| N4 | Automated self-serve account merge | Manual support action in v1 (master 16.6) |
| N5 | Nested orgs, teams-within-org, sub-orgs | Not in scope; org is flat |
| N6 | Per-endpoint API key scopes | Role-scoped only in v1 (master 5) |
| N7 | More than one bootstrap superuser, or bootstrap admin in hosted SaaS | Self-host only, single env admin (section 11) |

---

## 2. Entities, relationships, and lifecycle

Conceptual model only. Storage, keys, and indexes are the architecture team's call. This expands the entity list in (master 3).

### 2.1 Entities

| Entity | What it is | Key attributes (conceptual) | Lifecycle |
|--------|-----------|------------------------------|-----------|
| **User** | A person, global to Pulse | id, primary email (verified), display name, avatar URL, created_at, status (active / deletion-pending / deleted) | Created on first sign-in (master 3); ends on account deletion (section 10) |
| **UserIdentity** | A link from a User to one provider account | id, user_id, provider (google / github), provider_user_id, provider_email, email_verified, linked_at | Created at sign-in or manual connect; removed on disconnect (section 8); a user has 1 or 2 |
| **Organization** | The tenant; owns all resources | id, name, slug, kind (personal / team), plan_id, created_at, status (active / deletion-pending / deleted) | Personal org auto-created at signup; team org created by a user; ends on org deletion (section 4.5) |
| **Membership** | Link of a User to an Org with a role | id, user_id, org_id, role (owner / admin / member / viewer), joined_at, source (signup / invitation), seat_id | Created at signup or invite-accept; ends on leave or removal |
| **Seat** | A paid capacity unit on an org's plan | id, org_id, occupancy (accepted-member / reserved-invite / free), occupied_by (membership_id or invitation_id, nullable) | Created with plan capacity (PRD-006); occupied by a membership or a pending invite; freed on leave/remove/revoke/expire |
| **Invitation** | A pending offer to join an org | id, org_id, invited_email, target_role, state, token, invited_by, created_at, expires_at, accepted_at, reserved_seat_id | Created `pending` with a reserved seat; ends `accepted` / `revoked` / `expired` (section 6) |

### 2.2 Relationships

| Relationship | Cardinality | Note |
|--------------|-------------|------|
| User to UserIdentity | 1 to 1..2 | Exactly one or two (Google and/or GitHub); never zero for an active user |
| User to Organization | many to many, via Membership | A user belongs to many orgs (master 3) |
| Membership carries role | 1 membership = 1 role | Role is per org (master 4) |
| Membership occupies Seat | 1 to 1 | An accepted membership occupies exactly one seat |
| Invitation reserves Seat | 1 to 1 while pending | Pending invites reserve a seat (master 16.1) |
| Organization to Membership | 1 to many | Always >=1 (at least one owner, master 4) |
| Organization to Invitation | 1 to many | |
| Organization to Plan | 1 to 1 | PRD-006 owns plan/subscription |
| Organization to all resources | 1 to many | monitors, channels, incidents, status pages, API keys (master 3) |

### 2.3 Invariants (must always hold)

| # | Invariant | Enforced where |
|---|-----------|----------------|
| I1 | Every active org has at least one owner | Membership write path; the last owner cannot be removed or demoted (master 4) |
| I2 | Every active user has at least one membership | Account flow; deleting the last personal org of a user is part of account deletion only |
| I3 | A user has at most one membership per org | Membership uniqueness on (user_id, org_id) |
| I4 | A user has at most one identity per provider | UserIdentity uniqueness on (user_id, provider) |
| I5 | A provider account maps to at most one user | UserIdentity uniqueness on (provider, provider_user_id) |
| I6 | Accepted members + reserved pending invites <= seat capacity | Seat allocation on invite and accept (PRD-006) |
| I7 | An invited email can have at most one `pending` invitation per org | Invitation uniqueness on (org_id, invited_email, state=pending) |
| I8 | Every resource carries an org_id and is scoped to it on every read and write | Data-access layer, not handlers (master 13.1) |

### 2.4 Lifecycle summary (state per entity)

- User: `active` -> `deletion-pending` (grace) -> `deleted`. Sign-in is blocked once `deletion-pending`.
- Organization: `active` -> `deletion-pending` (grace) -> `deleted` (section 4.5).
- Membership: exists while active; hard-removed on leave/remove (with audit, section 9).
- Invitation: see the state machine in section 6.2.
- Seat: occupancy flips between free / reserved-invite / accepted-member; capacity itself is set by the plan (PRD-006).

---

## 3. Authentication product behavior

This is the user-facing contract. Token crypto and durations are the architecture team's call (master 5). This expands (master 5).

### 3.1 Sign-in (Google + GitHub OAuth/OIDC)

| Rule | Behavior |
|------|----------|
| Providers | Sign in with Google and Sign in with GitHub only. No password field anywhere (master 5) |
| Identity anchor | The provider's verified email. We never act on an unverified provider email (master 5) |
| First sign-in | Creates the User from the provider profile (email, name, avatar) and a personal org, owner membership, seat 1 (section 4.1) |
| Returning sign-in | Matches the provider account to an existing UserIdentity and resumes the user; no new org |
| Unverified provider email | Sign-in is refused with a clear message ("your {provider} email is not verified"); no user is created |

### 3.2 First-sign-in user + personal-org creation (atomic)

On first sign-in, in one transaction (master 3):

1. Create User from verified provider profile.
2. Create UserIdentity linking the provider.
3. Create a personal Organization (kind=personal) on the Free plan, named from the user's name or email (for example "Dev's workspace").
4. Create a Membership making the user owner, occupying seat 1.
5. Route into onboarding inside that org (master 10 screen 2).

If any step fails the whole thing rolls back; the user is never left half-created with no org (I2).

### 3.3 Account linking rules

Goal: one person is one Pulse user even when they use both providers (master 5). Three paths:

| Path | Trigger | Rule |
|------|---------|------|
| **Auto-link on verified-email match** | A user signs in with provider B whose verified email equals an existing user's verified email | The provider B identity is linked to that existing user. No new user. We only auto-link when both emails are verified (master 5) |
| **Manual link** | Signed-in user clicks "Connect GitHub"/"Connect Google" in Account settings, completes that provider's OAuth | The new provider identity attaches to the current user, even if the provider email differs from the user's primary, because the user proved control of both sessions. Blocked if that provider account is already linked to another user (I5) |
| **Divergent-email edge case** | Provider B returns a verified email that matches no existing user (for example the same person changed their GitHub email) | A new separate User is created (master 5). The two accounts are not auto-merged. Merging is a manual support action in v1 (master 16.6). Recommended UX: detect "you may already have an account" heuristically and point the user to manual link or support |

Account linking never happens on an unverified email (master 5). Linking via the manual path requires an active authenticated session, so a forwarded OAuth callback cannot attach a stranger's provider to your account.

### 3.4 Sign-out and sessions

| Behavior | Contract |
|----------|----------|
| Session persistence | Sessions survive browser restarts; you are not logged out every visit (master 5) |
| Multi-device | Each device has an independent session; signing in on a phone does not end the laptop session (master 5) |
| Sign out (this device) | Ends the current device session promptly (master 5) |
| Log out of all devices | Available in Account settings; ends every active session for the user across all devices (master 5, master 10 screen 14) |
| Revocation timeliness | Logout, role change, or removal from an org takes effect quickly, within the access-token refresh window at worst (master 5) |
| Authorization timing | Always evaluated against the active org membership at request time; a demoted or removed user sees the change on their next refreshed request (master 5) |

"Log out of all devices" is the lever a user pulls after a lost laptop or a suspected token leak. It also runs automatically on account-deletion request (section 10) so a deletion-pending account cannot keep acting.

### 3.5 Active org and the session

The active org is part of session/UI state (master 3). Every API call is scoped to one org via the path or an org context (master 9). Permissions are always evaluated against the active org's membership role (master 3). Switching org changes the active context; it does not start a new session (section 4.3).

---

## 4. Organizations

Expands (master 3) on org creation, kinds, switching, settings, deletion, and ownership.

### 4.1 Creation

| Trigger | Result |
|---------|--------|
| First sign-in | Personal org auto-created (kind=personal), creator is owner, Free plan (section 3.2) |
| User clicks "Create organization" | Team org created (kind=team), creator is owner, seat 1, Free plan to start; user is dropped into onboarding inside the new org |

Org name and slug are set at creation. Slug is unique across Pulse and shapes the status-page URL `{org-slug}.pulsepager.com` (master 16.3). Slug edits are allowed but warned (they change public status-page URLs).

### 4.2 Personal vs team orgs

| Aspect | Personal org | Team org |
|--------|-------------|----------|
| Created | Automatically at signup | Manually by a user |
| kind | personal | team |
| Purpose | Solo work, never a dead end (master 3) | Shared team workspace (master 2 persona B) |
| Owner | The creator | The creator |
| Can invite members | Yes (same rules) | Yes |
| Can be deleted | Yes, but a user cannot delete their only org while their account is active (it would orphan them, I2); they delete the account instead (section 10) | Yes (section 4.5) |
| Difference in RBAC | None; the four roles work identically | None |

The only real difference is provenance and the I2 protection. Functionally a personal org is just a team org with one member to start. A user who is later invited to a company org keeps their personal org and switches between them (master 3).

### 4.3 Org switching

The org switcher lives in the top nav (master 10 screen 3). It lists the user's orgs, shows the active one and a quick role indicator, and offers "create organization." Selecting an org changes the active org in session state. No re-auth. The next API call is scoped to the new active org and permissions re-evaluate against that org's role (master 3).

### 4.4 Org settings

Editable by owner/admin (master 4). Covers: org name, slug, default check settings, SSRF policy display (read-only, on by default, master 13), audit-log access, and the delete-organization action (owner only). Every settings change is audited (section 9).

### 4.5 Org deletion (grace + data implications)

Owner-only (master 4). Existential, so it never happens by an admin or a leaked key (master 5 key ceiling).

| Step | Behavior |
|------|----------|
| Request | Owner confirms deletion in Org settings. Org moves to `deletion-pending`. A short grace window applies (recommended default 14 days; see section 13) |
| During grace | Monitoring stops for that org (no new checks dispatched), the org is hidden from normal use, but data is recoverable by support/owner. Status pages for the org go offline |
| Data implications | All org-owned resources are scheduled for removal: monitors, channels, incidents, check results, status pages, API keys, invitations, memberships. Billing subscription is cancelled (PRD-006) |
| Members | All memberships end. Each member loses access immediately on `deletion-pending`; their other orgs are untouched |
| After grace | Hard delete of all org data. Deletion honored within the committed window (master 13 GDPR) |
| Last owner leaving | An owner cannot leave or delete themselves out of the org and orphan it; they must transfer ownership or delete the org (I1) |

Downgrade-driven data loss is never silent: bringing usage under a lower plan's limits is a guided action, not an auto-delete (master 11). Org deletion is the only path that removes org data wholesale, and it is gated by grace.

### 4.6 Ownership and the "at least one owner" invariant

At least one owner must always exist on an org (master 4, I1). The last owner cannot be removed or demoted; ownership must be transferred first (master 4).

| Action on the last owner | Result |
|--------------------------|--------|
| Demote last owner to admin/member/viewer | Blocked with "transfer ownership first" |
| Remove last owner | Blocked with "transfer ownership first" |
| Last owner tries to leave | Blocked with "transfer ownership or delete the org" |
| Last owner deletes the org | Allowed (section 4.5) |

### 4.7 Ownership transfer

Owner-only (master 4). Transfer makes another member an owner. Recommended default: the new owner must already be a member of the org (you transfer to someone present, not to an email). The acting owner chooses whether to stay an owner (co-owners allowed) or step down to admin/member. Multiple owners are allowed; the invariant is "at least one," not "exactly one." Every transfer is audited (section 9).

---

## 5. Memberships and seats

Expands (master 3) seat behavior. Seat capacity and the plan tie-in are owned by PRD-006 Billing.

### 5.1 How a membership occupies a seat

An accepted membership occupies exactly one seat (master 3, I1-style 1:1). A pending invitation reserves a seat so a team cannot over-invite past their plan (master 16.1). The seat meter therefore counts accepted members + reserved pending invites (master 11 seats meter).

| Event | Seat effect |
|-------|-------------|
| Invite sent | One seat moves to `reserved-invite` (blocked if none free, with upsell, master 3) |
| Invite accepted | That reserved seat flips to `accepted-member` for the new membership |
| Invite revoked or expired | Reserved seat freed |
| Member leaves or is removed | Their seat freed |
| Plan upgrade | More seats become available (PRD-006) |
| Plan downgrade below current usage | Owner is prompted to remove members/invites to fit before the downgrade applies (master 11); no silent removal |

### 5.2 Seat limits tie to plan (reference PRD-006)

Seat capacity per org is set by the plan tier (master 11 table: Free 1, Hobby 3, Professional 10, Custom unlimited, with per-seat add-ons on the paid tiers). The exact numbers, add-on seats, and proration are owned by PRD-006. This domain only consumes the entitlement: it asks "is a seat available?" on invite and accept, and it reports occupancy. Enforcement is cross-cutting (master 11): the api blocks an over-seat invite on write; PRD-006 owns the meter and the upsell copy.

### 5.3 Join and leave

| Action | Who | Rule |
|--------|-----|------|
| Join | Any invited user | Only via accepting an invitation (section 6) or via signup (personal org). There is no open self-join to a team org |
| Leave | Any member | Allowed for any role except the last owner (master 4, I1). Leaving frees the seat and ends the membership. The user keeps their other orgs and their account |
| Remove | Owner/admin | Owner can remove anyone except the last owner; admin can remove non-owners (master 4). Removal frees the seat and ends the membership immediately; revocation timing per section 3.4 |

A removed user's owned data does not leave with them: all resources belong to the org, not the creator (master 4 note "all resource access is role-based, not creator-based"). Removing a member never deletes monitors or channels.

---

## 6. Invitation flow

Expands (master 3) invitation behavior into a full state machine. This is the team-onboarding path (master 2 persona B, master 10 screen 10).

### 6.1 Flow (happy path and no-account-yet path)

1. An owner or admin opens Members, enters an email and a target role, sends the invite (master 3, master 4).
2. Pulse creates an Invitation in `pending`, reserves a seat (blocked with upsell if none free), and emails a tokenized accept link. Invitations expire after 7 days (master 3, master 16.1).
3. The invitee clicks the link. Two paths:
   - **Has an account** (signed in or signs in, with the verified email matching the invited email): sees "Join {Org} as {role}?"; on accept a Membership is created and the invitation moves to `accepted`; the reserved seat flips to occupied (master 3).
   - **No account yet**: the link routes them through Google/GitHub sign-in first, which creates the user and their personal org (section 3.2), then returns to the accept step. The signed-in verified email must match the invited email (master 3, master 16.2).
4. Owner/admin can revoke a pending invitation (frees the reserved seat) or resend it (master 3).

### 6.2 Invitation state machine

States: `pending`, `accepted`, `revoked`, `expired`. Terminal: `accepted`, `revoked`, `expired`.

| From | Event | Guard | To | Seat effect | Audit |
|------|-------|-------|----|-------------|-------|
| (none) | Owner/admin sends invite | A free seat exists; no existing `pending` invite for that email in this org (I7); inviter role allows (master 4) | `pending` | Reserve one seat | member.invited |
| `pending` | Invitee accepts | Signed-in verified email matches invited_email (master 16.2); not expired | `accepted` | Reserved seat -> occupied by new membership | member.joined |
| `pending` | Owner/admin revokes | Inviter role allows | `revoked` | Free the reserved seat | invitation.revoked |
| `pending` | 7 days pass | now > expires_at | `expired` | Free the reserved seat | invitation.expired |
| `pending` | Owner/admin resends | Inviter role allows | `pending` (same row, new token, refreshed expiry) | No change (still reserved) | invitation.resent |
| `accepted` / `revoked` / `expired` | any | terminal | (no transition) | none | none |

### 6.3 Email matching on accept

Recommended default: the signed-in verified email must match the invited email (master 16.2). Reasoning: a forwarded invite link cannot be accepted by the wrong person. Trade-off: someone whose GitHub email differs from the invited address must sign in with the matching identity or ask for a re-invite (master 16.2). This is acceptable for security and is the locked recommended default.

Edge handling:

| Case | Behavior |
|------|----------|
| Invited email matches a verified email on a linked identity (not the primary) | Match succeeds; the user has proven control of that email by linking it |
| Signed-in user's email does not match | Accept is refused with "this invite was sent to {invited_email}; sign in with that account or ask for a new invite" |
| Invited user already a member | Accept is a no-op success; the pending invite (if any) is closed and its seat freed |
| Invite expired or revoked when clicked | Show "this invitation is no longer valid; ask {org} for a new one" |

### 6.4 Seat reservation and expiry

Pending invites reserve a seat (master 16.1) so billing is predictable and a team cannot over-invite. The 7-day expiry (master 3) bounds how long a seat stays reserved by an unaccepted invite; on expiry the seat frees automatically. Resend refreshes the token and the 7-day clock without changing seat reservation.

### 6.5 No-account-yet path detail

The invited person may not have a Pulse account. The accept link sends them through OAuth first (section 3.2), which both creates their user and personal org and gives them a verified email to match against the invite. After sign-in they land back on "Join {Org} as {role}?" and accept. This makes the cold-invite path one continuous flow with no separate registration step (master 2 persona B "productive in minutes").

---

## 7. RBAC

Four roles, ordered by power: owner > admin > member > viewer (master 4). Roles are per org. At least one owner must always exist (master 4, I1). This section restates the master matrix and adds identity-specific rows so the full identity/org/member/billing-visibility/api-key surface is explicit in one place.

### 7.1 Role summary

| Role | One-line | Mental model |
|------|----------|--------------|
| Owner | Controls money and existence | "Whose org it is" |
| Admin | Runs the org except money | "Runs the place day to day" |
| Member | Operator of monitoring | "Does the work" |
| Viewer | Read-only | "Watches" |

### 7.2 Complete permission matrix

Legend: Y = allowed, N = not allowed, Self = applies to their own membership only. Rows from the master matrix (master 4) are kept verbatim in meaning; identity/org-specific rows added by this sub-PRD are marked (+).

| Capability | Owner | Admin | Member | Viewer |
|-----------|:-----:|:-----:|:------:|:------:|
| **Identity and account (per acting user)** | | | | |
| Sign in / sign out (Self) (+) | Y | Y | Y | Y |
| Manage own profile, linked providers, sessions (Self) (+) | Y | Y | Y | Y |
| Log out of all own devices (Self) (+) | Y | Y | Y | Y |
| Delete own Pulse account (Self) (+) | Y | Y | Y | Y |
| View "orgs I belong to" (Self) (+) | Y | Y | Y | Y |
| **Org membership and people** | | | | |
| View member list and roles (+) | Y | Y | Y | Y |
| Invite members / set invited role | Y | Y | N | N |
| Resend / revoke a pending invitation (+) | Y | Y | N | N |
| Change a member's role | Y | Y (not to/from owner) | N | N |
| Remove a member | Y | Y (not an owner) | N | N |
| Transfer ownership | Y | N | N | N |
| Leave the org | Y (if not last owner) | Y | Y | Y |
| **Organization lifecycle** | | | | |
| Create a new organization (+) | Y | Y | Y | Y |
| Edit org settings (name, slug, defaults) | Y | Y | N | N |
| View audit log | Y | Y | N | N |
| Delete the organization | Y | N | N | N |
| **Billing visibility (detail in PRD-006)** | | | | |
| View billing and usage | Y | Y | N | N |
| Manage billing (plan, payment, invoices) | Y | N | N | N |
| **API keys (detail in PRD-005)** | | | | |
| Create / revoke API keys | Y | Y | N | N |
| View API keys list (metadata, not secret) | Y | Y | N | N |
| **Monitoring surface (from master 4, summarized)** | | | | |
| View monitors, incidents, history, status | Y | Y | Y | Y |
| Create / edit / delete monitors, run check-now | Y | Y | Y | N |
| Create / edit / delete channels, send test | Y | Y | Y | N |
| Acknowledge / annotate incidents | Y | Y | Y | N |
| Manually close an incident | Y | Y | N | N |
| Create / edit / publish status pages | Y | Y | Y | N |
| Configure custom domain for status page | Y | Y | N | N |

### 7.3 Identity-specific design notes

- **Account actions are always Self and role-independent.** Profile, linked providers, sessions, log-out-all, account deletion, and "orgs I belong to" are about the person, not the org, so every role can do them for themselves. A viewer in a company org still fully controls their own account.
- **Creating a new org is open to everyone.** Any user can create an org and becomes its owner; this does not depend on their role in any existing org. It mirrors the signup path (section 4.1).
- **Admin cannot touch owner.** Admin can manage people and keys but cannot change a role to or from owner, cannot remove an owner, cannot transfer ownership, cannot manage billing, and cannot delete the org (master 4). Those are owner-only.
- **Members manage monitoring, not people, money, or keys** (master 4). Inviting, role changes, key creation, and audit-log access are admin+.
- **API keys inherit a role and cannot exceed it; keys max out at admin, no owner-equivalent keys** (master 5, master 16.5). So billing, ownership transfer, and org deletion stay UI-only human actions and cannot be automated by a leaked key. PRD-005 owns key mechanics.

---

## 8. Account and profile

Account settings (master 10 screen 14). Owned by the user, role-independent (section 7.3).

### 8.1 Profile from provider

| Field | Source | Editable |
|-------|--------|----------|
| Display name | Provider profile at sign-in | Recommended: editable override stored on the user; defaults to provider name. (See section 13) |
| Avatar | Provider profile | Follows provider; no custom upload in v1 |
| Primary email | Verified provider email | Not free-text editable; changes only by linking/unlinking identities |

Profile is seeded from the provider and refreshed opportunistically on sign-in. We never collect a password or a separately-entered email.

### 8.2 Linked providers management

| Action | Rule |
|--------|------|
| View linked providers | Shows Google and/or GitHub, which is connected, and the email each reports |
| Connect a provider | Manual link path (section 3.3); completes that provider's OAuth and attaches the identity to the current user |
| Disconnect a provider | Allowed only if the user has another linked identity (cannot remove your only way to sign in; that would lock you out). Disconnecting your last identity is refused with "connect another provider first or delete your account" |

### 8.3 Orgs-I-belong-to view

Account settings shows every org the user is a member of, with the user's role in each and a quick switch (master 10 screen 14, master 3). This is the personal counterpart to the org switcher (section 4.3): the switcher is for fast context change in the top nav, this view is the full self-service list including "leave org" per row (subject to the last-owner rule, I1).

---

## 9. Audit log entries this domain produces

Sensitive actions are recorded per org and visible to owner/admin (master 13.5, master 4). Each entry records who, what, when, from where (IP/agent), and the target resource (master 13.5). Retention follows the plan tier (master 11). This domain produces:

| Event | When | Target | Notes |
|-------|------|--------|-------|
| member.invited | Invite sent | invited_email, target_role | Includes inviter |
| member.joined | Invite accepted | new member user | Closes the invite |
| invitation.resent | Pending invite resent | invitation | |
| invitation.revoked | Pending invite revoked | invitation | Seat freed |
| invitation.expired | 7-day expiry | invitation | System actor; seat freed |
| member.role_changed | Role change | member, old_role, new_role | |
| member.removed | Member removed | removed member | By owner/admin |
| member.left | Member leaves | the leaving member | Self-initiated |
| ownership.transferred | Ownership transfer | from_user, to_user | Owner only |
| org.settings_changed | Settings edited | changed fields | Name/slug/defaults |
| org.deletion_requested | Org deletion started | org | Enters grace |
| org.deleted | Grace ends, hard delete | org | System actor at grace end |
| auth.login | Successful sign-in worth auditing | user, provider | Recommended: record for visibility; high volume, see section 13 |
| auth.logout_all | "Log out of all devices" used | user | Security-relevant |
| identity.linked | Provider connected | provider | |
| identity.unlinked | Provider disconnected | provider | |

API key created/revoked and billing/plan change are also audited (master 13.5) but are owned by PRD-005 and PRD-006 respectively; this domain references them, it does not define their entries.

---

## 10. GDPR

Builds on (master 13 GDPR). Deletions honored within the committed window.

### 10.1 Account deletion behavior

A user can delete their account (master 13). The hard part is owned data and the last-owner invariant. On a delete-account request:

1. The account moves to `deletion-pending`; sign-in is blocked and "log out of all devices" runs immediately (section 3.4) so the account stops acting.
2. For each org the user belongs to, resolve their memberships:

| Situation | Behavior |
|-----------|----------|
| User is a non-owner member (any role) | Membership ends, seat freed; the org is untouched |
| User is one of several owners | Membership ends; the org keeps its other owners (I1 holds) |
| User is the sole owner of a team org with other members | Deletion is blocked until the user transfers ownership; UX prompts "transfer or delete these orgs first." We do not silently promote a random member |
| User is the sole owner of a personal org or a team org with no other members | That org is deleted with the account (it has no one else); its grace window aligns with the account grace |

3. After the grace window, the user row and all UserIdentities are hard-deleted. Owned-org data follows the org-deletion path (section 4.5) for orgs deleted alongside the account.

Reasoning: we never orphan an org (I1) and never silently hand someone else's org to a member. The blocking-on-sole-owner rule forces an explicit human decision (transfer or delete), which matches the existential weight of those actions. This is consistent with master 13 ("deleting an account transfers or deletes personal orgs and removes memberships").

### 10.2 Data export of identity data

A user can export their personal data; an owner can export an org's data (master 13). For this domain:

| Export | Actor | Contents |
|--------|-------|----------|
| Personal identity export | The user | Profile (name, email, avatar URL), linked providers and their reported emails, list of orgs and the user's role in each, account timestamps, and the user's own audit entries (login, identity link/unlink, logout-all) |
| Org members export | Owner (part of org export) | Member list with roles and join dates, pending invitations with target roles and state, ownership history |

Export is machine-readable (master 13). Secrets and other users' personal data are never included in a personal export; an org export includes member emails because the owner is the controller for that org's data.

---

## 11. Self-host bootstrap admin

The hosted SaaS is social-login only (master 5). The optional self-host build keeps a single bootstrap admin via env so a fresh self-hosted instance is not a chicken-and-egg lockout before any OAuth app is configured.

| Aspect | Behavior |
|--------|----------|
| Where it exists | Self-host build only. Never present in the hosted multi-tenant SaaS |
| How it is set | A single env-provided superuser (email + secret) read at boot; not stored as a normal user with a provider identity |
| What it can do | Bootstrap the instance: configure the OAuth apps (Google/GitHub client ids/secrets), create the first real org, and grant the first human owner. It is an operator account, not a tenant |
| Coexistence with social login | Once OAuth is configured and a real owner exists via Google/GitHub, day-to-day use is social-login exactly like SaaS. The bootstrap admin remains an operator break-glass, not part of any org's RBAC |
| Constraints | Exactly one bootstrap admin (no multi-superuser, N7). Recommended: its actions are audited as a distinct system/operator actor, and operators are advised to rotate or disable the env secret after initial setup |
| Why not in SaaS | In multi-tenant hosting an env superuser would be a cross-tenant master key, which violates the isolation invariant (master 13.1). So it is strictly a self-host bootstrap affordance |

This mirrors the master's "optional self-host bootstrap admin via env exists" while keeping the SaaS path passwordless and tenant-isolated.

---

## 12. User stories, acceptance criteria, edge cases

Personas from (master 2): Dev (solo), Team (startup), SRE (scaling ops).

### 12.1 User stories per persona

| # | Persona | Story |
|---|---------|-------|
| US1 | Dev | As a solo dev I sign in with GitHub once and immediately have a workspace, so I can create my first monitor without setting up an account |
| US2 | Dev | As a dev I connect Google to the same account so I can use either button |
| US3 | Dev | As a dev I delete my account and trust my personal org and its data go with it |
| US4 | Team | As an owner I invite a teammate by email as a member, and they are productive in minutes |
| US5 | Team | As an admin I change a teammate's role and remove someone who left, without being able to touch billing |
| US6 | Team | As an owner I transfer ownership to a co-founder and step down to admin |
| US7 | Team | As a viewer I can see all monitors and incidents but cannot change anything |
| US8 | SRE | As an owner I read the audit log to see who invited and removed whom and who changed roles |
| US9 | SRE | As an owner I export org members and identity data for compliance |
| US10 | Any | As a user I switch between my personal org and my company org from the top nav |
| US11 | Any | As a user who lost a laptop I log out of all devices from Account settings |

### 12.2 Acceptance criteria (concrete, testable)

| # | Given / When / Then |
|---|---------------------|
| AC1 | Given a brand-new GitHub user, when they complete GitHub sign-in, then exactly one User, one UserIdentity (github), one personal Organization (Free), and one owner Membership on seat 1 are created in one transaction, and they land in onboarding |
| AC2 | Given a user signed in via Google with verified email X, when they sign in via GitHub also returning verified email X, then no new user is created and the GitHub identity links to the existing user |
| AC3 | Given a signed-in user, when they click Connect GitHub and that GitHub account is already linked to another user, then the link is refused and no change is made |
| AC4 | Given an org at its seat limit, when an owner tries to invite, then the invite is blocked with an upsell and no invitation row or seat reservation is created |
| AC5 | Given a pending invite to email X with role member, when a user signed in with verified email X accepts, then a member Membership is created, the invite becomes accepted, and the reserved seat is now occupied |
| AC6 | Given a pending invite to email X, when a user signed in with email Y (not X) clicks the link, then accept is refused with a clear message and no membership is created |
| AC7 | Given a pending invite, when 7 days pass without acceptance, then the invite becomes expired and its reserved seat is freed |
| AC8 | Given an org with exactly one owner, when anyone tries to demote, remove, or have that owner leave, then the action is blocked with "transfer ownership first" |
| AC9 | Given an org with one owner and one member, when the owner transfers ownership to the member and steps down to admin, then the org has one owner (former member) and one admin (former owner) |
| AC10 | Given an admin, when they attempt to manage billing, delete the org, or transfer ownership, then each is denied per the matrix |
| AC11 | Given a member, when they attempt to invite, change roles, create an API key, or view the audit log, then each is denied |
| AC12 | Given a user removed from an org, when their session next refreshes, then they no longer have access to that org and their other orgs are unaffected |
| AC13 | Given a user clicks "log out of all devices", when it completes, then every active session for that user is ended across devices |
| AC14 | Given a sole owner of a team org with other members requests account deletion, when they confirm, then deletion is blocked until they transfer ownership or delete those orgs |
| AC15 | Given a non-owner member requests account deletion, when grace ends, then their memberships end, seats free, owned orgs that have other people are untouched, and personal/empty orgs are deleted with the account |
| AC16 | Given an owner requests org deletion, when confirmed, then the org enters deletion-pending, monitoring stops, status pages go offline, and after the grace window all org data is hard-deleted |
| AC17 | Given any identity event (invite, role change, removal, transfer, settings change, org deletion, logout-all, link/unlink), when it occurs, then an audit entry with who/what/when/from-where is recorded and visible to owner/admin |
| AC18 | Given a user A from org A and a key/user from org B, when either tries to read or affect the other org's identity or member data, then it is denied at the data layer (master 13.1) |
| AC19 | Given a user with two linked identities, when they disconnect one, then it succeeds; when they try to disconnect the last one, then it is refused |
| AC20 | Given a self-host instance with the env bootstrap admin set and OAuth not yet configured, when the operator signs in as the bootstrap admin, then they can configure OAuth and create the first owner; in the hosted SaaS no such admin exists |

### 12.3 Edge cases

| # | Edge case | Handling |
|---|-----------|----------|
| E1 | Provider returns unverified email | Sign-in refused; no user created (section 3.1) |
| E2 | Provider changes a user's email to a new verified value not matching any user | New separate user created; manual merge only (master 16.6, section 3.3) |
| E3 | Two pending invites attempted for the same email in one org | Second is refused; one pending invite per email per org (I7) |
| E4 | Invited person already a member | Accept is a harmless no-op; pending invite (if any) closed and seat freed (section 6.3) |
| E5 | Invite link forwarded to a different person | Accept refused on email mismatch (master 16.2, section 6.3) |
| E6 | Invite clicked after revoke/expiry | "No longer valid" message; no membership (section 6.3) |
| E7 | Last owner tries to leave or delete self | Blocked; must transfer or delete org (I1, section 4.6) |
| E8 | Downgrade would exceed lower plan's seats | Owner prompted to remove members/invites first; no silent removal (master 11, section 5.1) |
| E9 | User in many orgs deletes account while sole owner of some | Blocked on those with other members; deletes empty/personal ones (section 10.1) |
| E10 | Concurrent accept of the same invite from two tabs | Idempotent; first wins, second sees "already a member" no-op (section 6.3) |
| E11 | Org slug change while a status page is public | Allowed with a warning that public URLs change (section 4.1) |
| E12 | Disconnect the only sign-in provider | Refused; would lock the user out (section 8.2) |

---

## 13. Open decisions with recommended defaults

Core behavior is locked by the master. These are the deeper choices this sub-PRD surfaces. Each ships with the recommended default unless overridden. The first two restate the master's already-recommended defaults so this domain is self-contained; the rest are new to this sub-PRD.

| # | Decision | Recommended default | Reasoning / trade-off |
|---|----------|---------------------|------------------------|
| D1 | Invited email must match signed-in provider email on accept | Yes, must match (master 16.2) | Stops forwarded invites being accepted by the wrong person. Trade-off: mismatched provider email needs a re-invite. Matches a verified email on any linked identity, not just primary |
| D2 | Account merge when provider emails diverge | Manual support action in v1 (master 16.6) | Auto-merge is risky and low-volume. Trade-off: rare friction; mitigated by a "you may already have an account" hint and the manual link path |
| D3 | Org deletion grace window length | 14 days | Long enough to recover from a mistaken delete, short enough to honor GDPR promptly. Trade-off: data lingers 14 days; mitigated by stopping monitoring and hiding the org immediately. Shorter (7 days) if legal prefers faster erasure |
| D4 | Display name editable, or always mirror provider | Editable override, defaults to provider name | Lets users fix odd provider names without a password account. Trade-off: a tiny bit of stored profile state. Alternative: mirror-only is simpler but annoys users with bad provider names |
| D5 | auth.login auditing volume | Audit logins, but consider a separate retention/stream from people-changes | Login visibility is useful for security, but it is high volume and could drown member-change events. Trade-off: full parity is noisy; recommend keeping login events queryable but not cluttering the people-change view |
| D6 | Ownership transfer target must be an existing member | Yes, transfer to a present member only | You hand the org to someone who is already in it, not to an unproven email. Trade-off: to transfer to an outsider you invite them as admin first, then transfer; one extra step, much safer |
| D7 | Multiple owners (co-owners) allowed | Yes | The invariant is "at least one owner," not "exactly one" (master 4). Co-owners make succession and vacations safe. Trade-off: more people can do existential actions; acceptable and audited |
| D8 | Removing a member: keep or reassign their owned resources | Keep with the org (no creator-ownership) | All resources belong to the org, not the creator (master 4). Removal never deletes monitors/channels. Trade-off: none material; matches the master's role-based-not-creator-based model |

---

## 14. Dependencies on other sub-PRDs

| Dependency | Direction | What this domain needs / provides |
|------------|-----------|-----------------------------------|
| PRD-006 Billing | This domain depends on it | Seat capacity per plan, the seats meter (accepted + reserved invites), upsell copy, downgrade-fit prompts, plan change effects on seats (master 11). This domain consumes "is a seat available?" and reports occupancy; it does not own plan numbers or proration |
| PRD-005 Public API | This domain provides to it | Roles and the permission matrix that key auth enforces; the rule that keys are role-scoped and max out at admin (master 5, master 16.5). PRD-005 owns key creation, hashing, last-used, and per-endpoint behavior; this domain owns what each role may do |
| PRD (master) sections 3, 4, 5, 13 | Parent | All locked decisions: org as isolation unit, four roles, social-only auth, JWT session contract, tenant isolation, GDPR, audit logging |
| Audit log (master 13.5) | Shared surface | This domain emits the identity/org/member events (section 9); the audit subsystem owns storage, retention by plan, and the owner/admin view |
| Status pages (master 8) | Downstream of org slug | Org slug shapes `{org-slug}.pulsepager.com`; slug changes here affect public URLs there (section 4.1) |

---

## Appendix A - Quick reference: who can do the existential actions

| Action | Owner | Admin | Member | Viewer | Via API key? |
|--------|:-----:|:-----:|:------:|:------:|:------------:|
| Manage billing / plan / payment | Y | N | N | N | No (no owner-equiv keys) |
| Delete the organization | Y | N | N | N | No |
| Transfer ownership | Y | N | N | N | No |
| Demote/remove the last owner | N (blocked, I1) | N | N | N | No |

These four are the deliberately-human, owner-only, non-automatable actions. Everything else flows from the matrix in section 7.2.
