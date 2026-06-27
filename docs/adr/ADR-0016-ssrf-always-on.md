# ADR-0016: SSRF protection always-on, not customer-disableable

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-000 section 10, RFC-005 sections 2.1 and 2.2, master PRD section 13, ADR-0010

## Context
Workers make outbound HTTP requests to customer-supplied URLs from inside the cluster. In a multi-tenant SaaS, a customer URL pointing at loopback, link-local, RFC1918, or cloud-metadata addresses is a server-side request forgery path into internal services. The v1 monolith left SSRF guarding opt-in via a config flag; that stance inverts in multi-tenant SaaS where the operator, not the customer, owns the blast radius.

## Options considered
- SSRF always-on at multiple layers, not customer-disableable - resolution-time IP validation, a dialer Control re-check of the connected IP per redirect hop, and pod egress NetworkPolicy blocking the dangerous ranges.
- Keep the v1 opt-in flag - lets a customer turn off the guard, which is unacceptable once one tenant's config can reach another tenant's or the operator's internal services.

## Decision
SSRF protection is always-on and not customer-disableable on workers. The internal/checker SSRF block (BlockPrivateNetworks) is forced on by config (a config flag, not a code change). Three layers: resolution-time validation refuses loopback/link-local/RFC1918/cloud-metadata before connecting; the dialer Control callback re-checks the actual connected IP on every dial including each redirect hop (the authoritative guard against DNS rebinding and TOCTOU); and pod egress NetworkPolicy blocks loopback, link-local, RFC1918, and cloud-metadata so a bypass still cannot reach internal services.

## Consequences
A customer can never reach internal services or cloud metadata through a monitor, and the dialer Control re-check means even DNS rebinding hits the connected-IP block. Egress NetworkPolicy is a defense-in-depth backstop independent of the application guard (ADR-0010). One known carry-forward gap is flagged for the security pass: the pre-resolve only covers the first URL, not every redirect hop, but Control fires on every hop so the connected-IP block still applies; the security review confirms this is sufficient or adds an explicit per-hop pre-resolve. Revisit if a residency or proxy feature ever needs a controlled exception, which would have to be operator-scoped, never customer-toggled.
