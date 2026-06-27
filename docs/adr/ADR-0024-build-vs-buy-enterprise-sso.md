# ADR-0024: Build-vs-buy enterprise SSO is buy, with an in-house escape hatch

Status: Accepted
Date: 2026-06-22
Deciders: Architecture
Related: RFC-016 sections 2, 1.1, 3, ADR-0005, RFC-003

## Context
Enterprise SSO (SAML + OIDC) and SCIM are a phase-3 sales unblocker, not a product differentiator. A correct SAML SP and SCIM server are large, quirk-heavy, security-critical surfaces (XML-signature validation, per-IdP deviations, cert/metadata rotation) where a subtle bug is a cross-customer auth bypass. Building and maintaining that matrix is orthogonal to uptime monitoring, but some strategic customers cannot accept a third party in their auth path.

## Options considered
- Buy: use an enterprise-SSO provider (reference: WorkOS; Stytch an acceptable equivalent) as the SAML/OIDC/SCIM normalization layer, with Pulse owning the mapping from the provider's verified identity to the Pulse user and membership (chosen). The provider absorbs the per-IdP SAML quirks, the SCIM server burden, and cert/metadata rotation; Pulse codes against one normalized "a verified identity + attributes for an org" abstraction.
- Build SAML + multi-IdP + SCIM in-house - rejected for v1, kept as the documented escape hatch. It is a multi-quarter, security-review-heavy project (a SAML SP with full XML-signature validation, an OIDC RP, a SCIM 2.0 server) that does not move the monitoring product forward. Revisit triggers: provider cost becomes material at scale, or a strategic customer's data-flow / residency rules forbid any third party in the auth path.

## Decision
Use an enterprise-SSO provider as the SAML/OIDC/SCIM normalization layer; Pulse owns the identity mapping onto the same `users` / `memberships` rows. The reference provider is WorkOS (Stytch acceptable). The provider terminates per-IdP SAML/OIDC detail and runs the SCIM server, and hands Pulse one normalized shape: a verified profile (email, name, IdP groups, raw attributes) for connection C belonging to org O. The provider's shape is wrapped behind a Pulse-internal `ssoidentity` interface, so the provider SDK is one adapter behind that interface. The in-house path is the documented alternative implementing the same abstraction with Pulse's own SAML/OIDC/SCIM code, sold as a custom Enterprise option for customers who cannot accept a sub-processor in the auth path.

## Consequences
Pulse gets a working SAML + OIDC + SCIM surface in weeks instead of a multi-quarter security project, and the ongoing spec drift, new-IdP onboarding, and protocol security patches stay with the provider; Pulse maintains only the thin, stable mapping layer. The trade-offs accepted: the provider sees auth metadata (email, name, IdP groups, SCIM payloads, never monitor data) so it is a named sub-processor under a DPA in the SOC 2 vendor process; per-connection or per-active-user cost, acceptable because SSO is Enterprise-tier entitlement-only; and lock-in, bounded because everything sits behind the `ssoidentity` adapter so swapping providers or moving in-house is reimplementing the adapter, not the mapping/session/authz code. The escape hatch keeps the strict-data-flow customer addressable without re-designing identity. Because the abstraction is fixed now, phase-3 work is filling in the adapter and tables, not retrofitting identity.
