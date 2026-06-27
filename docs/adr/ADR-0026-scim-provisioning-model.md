# ADR-0026: SCIM 2.0 provisioning with JIT fallback, group-to-role mapping, and fast deprovisioning

Status: Accepted
Date: 2026-06-22
Deciders: Architecture
Related: RFC-016 sections 6, 7, 8, ADR-0024, ADR-0025, RFC-003 sections 4.3 and 6.3

## Context
Enterprises expect users to appear and disappear in Pulse automatically as they join and leave the company, driven by the IdP rather than by manual invites. The two mechanisms are just-in-time (JIT) provisioning on first SSO login and SCIM 2.0 (the IdP pushes the user lifecycle). Security reviews specifically want SCIM because JIT-only leaves offboarding manual, which is an access-revocation gap.

## Options considered
- JIT only - rejected as the sole mechanism. It creates users on first login but never deactivates them, so a removed employee keeps access until someone notices. Acceptable only as a fallback.
- SCIM only - rejected. SCIM setup is heavier; JIT is a good minimum so SSO works before SCIM is wired.
- JIT plus SCIM (chosen). JIT is the minimum path (first SSO login creates the user and a membership at the default role); SCIM is the authoritative lifecycle and the fast-deprovision path.

## Decision
Support both. JIT: first SSO login for a connection creates the user (linked by verified email per RFC-003 account-linking) and a membership at the default role (member). SCIM 2.0: a per-connection SCIM server surface (`/scim/v2/Users`, `/scim/v2/Groups`) authenticated by a per-connection bearer token stored hashed (SHA-256); it handles create/update/deactivate and group membership idempotently. IdP groups map to the four Pulse roles via configured `group_role_mappings`, default member, highest-role-wins, clamped to admin (SSO/SCIM can never grant owner), and the at-least-one-owner invariant cannot be broken by provisioning. SCIM deactivate (and group removal) disables the membership, revokes the user's refresh tokens, and busts the membership cache, reusing the existing revocation and cache paths (RFC-003 4.3 / 6.3), so loss of access takes effect on the next request.

## Consequences
Offboarding is automatic and fast, closing the access-revocation gap that worries security reviewers, while JIT keeps SSO usable before SCIM is configured. Role mapping is driven by the IdP's groups, so customers manage access in one place, and the admin/owner clamp plus the last-owner invariant keep provisioning from escalating privilege or orphaning an org. The cost is a SCIM server surface to implement and keep idempotent, largely absorbed by the provider (ADR-0024); the in-house escape hatch must implement the same endpoints and the same revocation wiring. SCIM config changes and provisioning events are audit-logged (RFC-015).
