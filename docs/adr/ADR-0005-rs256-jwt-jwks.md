# ADR-0005: RS256 JWT access tokens with JWKS, identity-only token

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-000 section 7.1, RFC-003 sections 3.1 and 3.2, ADR-0012

## Context
api is the only service that authenticates external principals. Humans sign in via Google or GitHub OIDC and carry a session as an access token. A user belongs to many orgs and switches between them often with no re-auth, and a role or membership change must take effect quickly. We need a token format and a verification model that fits both.

## Options considered
- RS256 (asymmetric) JWT plus a JWKS endpoint - the private signing key lives only in api, verification needs only the public key, no shared secret across verifiers.
- HS256 (symmetric) JWT - simpler, but the signing secret would have to be shared with every verifier, widening where the secret lives.

## Decision
Access tokens are RS256-signed JWTs with a ~15 minute lifetime. The RS256 private key lives only in api, loaded at boot from a KMS-backed Kubernetes secret, and the public key is published at /.well-known/jwks.json. The token identifies the user only: it deliberately carries no org, no role, and no scope claim. The active org comes from the request (path or header, checked against membership) and the role comes from a fresh, Redis-cached membership lookup on every request.

## Consequences
The signing key never leaves api and verification never needs a shared secret, so adding a future verifier means fetching a public key, not distributing a secret. Org switching needs no token reissue because org is not in the token, so a frequent UI action stays a one-round-trip context change. Because role is not baked in, a role change or removal takes effect on the next request (bounded by the membership-cache TTL), not on token expiry; the 15 minute lifetime bounds the value of a stolen access token specifically. The cost is a membership and role lookup on every authenticated request, which the Redis cache keeps sub-millisecond. Revisit the lifetime if revocation timeliness or refresh traffic shifts.
