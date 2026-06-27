# ADR-0025: Enterprise SSO support set is SAML 2.0 plus OIDC, with strict assertion validation

Status: Accepted
Date: 2026-06-22
Deciders: Architecture
Related: RFC-016 sections 3, 5, 9, ADR-0024, ADR-0005, RFC-003

## Context
Enterprise customers authenticate through their own identity provider (Okta, Azure AD/Entra, Google Workspace, OneLogin, generic IdPs). Two protocols cover that market: SAML 2.0 (still the default for large enterprises) and OIDC (newer, simpler, common for Google Workspace and modern IdPs). SAML in particular is a frequent source of cross-tenant auth bypass when assertion validation is sloppy (signature wrapping, unsigned assertions, audience confusion, replay).

## Options considered
- OIDC only - rejected. Many target enterprises still standardize on SAML; OIDC-only would lose deals.
- SAML only - rejected. Misses the simpler OIDC path that Google Workspace and modern IdPs prefer.
- SAML 2.0 + OIDC, both SP- and IdP-initiated (chosen). Covers the enterprise market. Delivered through the provider chosen in ADR-0024, which terminates the protocol detail, but Pulse still owns the validation contract it requires of any implementation (provider or in-house escape hatch).

## Decision
Support SAML 2.0 (SP-initiated and IdP-initiated) and OIDC, per-org connection. Whether served by the provider (ADR-0024) or the in-house escape hatch, the implementation MUST enforce: the XML signature covers the exact consumed element (anti-XSW, XML-signature-wrapping); reject unsigned or signature-stripped assertions; validate audience/recipient, NotBefore and NotOnOrAfter with bounded clock skew, and InResponseTo for SP-initiated flows; keep an assertion-id replay cache; support optional encrypted assertions with no plaintext downgrade; and constrain the ACS RelayState to a server-side allowlist to prevent open redirects. A connection only ever authenticates into its own org.

## Consequences
The enterprise market is addressable on day one of the SSO feature, and the validation contract is explicit so neither the provider adapter nor the in-house path can quietly ship a weaker check. The cost is two protocols to test against the real IdP matrix; ADR-0024's buy decision absorbs most of that. The security mitigations are stated as hard requirements because each omission is a cross-customer bypass, and they are audited as part of the SOC 2 control set (RFC-015). The model feeds the same verified-identity abstraction (ADR-0024) and the same session/JWT issuance (RFC-003), so adding a protocol later is adapter work, not an identity redesign.
