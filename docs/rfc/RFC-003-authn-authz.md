# RFC-003 - AuthN and AuthZ

Status: DRAFT for review
Author: Principal Security / Identity
Audience: api service authors, frontend (RFC-013), public API (RFC-012), and anyone wiring authz into a handler
Parent: `docs/rfc/RFC-000-architecture-overview.md` (section 7 identity propagation, section 10 security, section 12 entitlement enforcement, ADR-0005 RS256 JWT)
Product source of truth: `docs/prd/PRD-001-identity-and-tenancy.md` (identity, RBAC, invitations, linking, sessions, self-host bootstrap), `docs/prd/PRD-005-public-api-and-webhooks.md` (API key behavior), master PRD sections 4 and 5
Reuses: `internal/auth` (v1 bcrypt, session token generation) carried forward into `internal/authn`

House style: no em-dashes. Tables and diagrams over prose. Every load-bearing choice states the decision, the reasoning, and the rejected alternatives.

---

## 1. Overview, scope, and owned contracts

### 1.1 What this RFC is

api is the single place that authenticates external principals and decides what they may do (RFC-000 section 7.1). This RFC fixes how that works: how a human signs in with Google, GitHub, or a passwordless magic link, how a session is carried and revoked, how an API key authenticates a script, how every request resolves to an `(org, role)` pair, and how a role check composes with the entitlement check before any write lands. The admin surface authenticates separately via Cloudflare Access.

These principal types converge on the same authorization model:

| Principal | Authenticates with | Carries org how | Role source |
|-----------|--------------------|-----------------|-------------|
| Human (SPA or any browser) | RS256 access JWT (in a cookie) backed by an opaque refresh token, obtained via Google/GitHub OAuth or an emailed magic link (`internal/authn/magiclink.go`) | active org is request-supplied (path or header), then checked against memberships | membership role in the active org |
| Script (public API client) | per-org API key `pulse_sk_<...>` | org is fixed by the key, not request-supplied | role stamped on the key (member or admin only) |
| Admin operator | Cloudflare Access (`internal/authn/cfaccess.go`, `CFAccessVerifier` checks the Access JWT) | the admin surface is not org-scoped | admin |

Both resolve to the same internal request context `(org_id, actor_id, actor_kind, role)` that authz reads. From the handler's point of view there is one authorization seam regardless of how the caller authenticated.

### 1.2 Scope

In scope: OIDC/OAuth2 login (Google, GitHub), passwordless magic-link login, Cloudflare Access for the admin surface, account linking, RS256 JWT issue and verify, JWKS, refresh tokens with rotation and revocation, browser token delivery and CSRF stance, API key format and hashing and caching, the request -> (org, role) resolution middleware, the RBAC matrix as code and the `Can(...)` seam, how role gate composes with the entitlement gate, the self-host bootstrap admin, and the threat model.

Out of scope (named, owned elsewhere): the user/org/membership/api_key/invitation schema (RFC-001), the entitlement data model and its cache (RFC-009 and RFC-000 section 12), the public REST surface and error envelope (RFC-012), the SPA token handling specifics and org switcher UI (RFC-013), outbound webhook signing (RFC-007), NetworkPolicy and TLS-to-infra runtime (RFC-011). The invitation product flow is PRD-001; this RFC owns only the auth-relevant parts (email match on accept, the login-then-accept path).

### 1.3 Contracts this RFC owns

| Contract | Decision in this RFC |
|----------|----------------------|
| Access token | RS256 JWT, ~15 min lifetime, org NOT in the token (section 3) |
| Refresh token | opaque, DB-backed, rotating on use, reuse-detected, ~30 day lifetime (section 4) |
| Browser delivery | access token and refresh token both in httpOnly+Secure+SameSite cookies, same-origin api (section 4.4) |
| CSRF | double-submit token because auth is cookie-borne (section 4.5) |
| API key | `pulse_sk_<22+ random chars>`, SHA-256 at rest, Redis-cached verify, member/admin only, one org (section 5) |
| Org context | request-supplied for humans (path `/orgs/{id}` for the SPA, fixed by the key for the API), checked against membership every request (section 6) |
| Authz | role gate AND entitlement gate, both must pass; `internal/authz.Can(actor, action, resource)` (section 7) |
| Packages | `internal/authn` and `internal/authz`; v1 bcrypt carries forward (section 8) |

### 1.4 What carries forward from v1 `internal/auth`

The v1 package (`internal/auth/auth.go`) is a single-admin password+cookie-session model. It is replaced, but three pieces carry forward verbatim or near-verbatim:

| v1 piece | Fate |
|----------|------|
| `bcrypt` hashing (`HashPassword`/`VerifyPassword`) | carried forward, but only for the self-host bootstrap admin secret (section 9). Not for API keys (section 5 explains why) |
| `newToken()` (32 random bytes, base64url) | carried forward as the opaque-token generator for refresh tokens and API key secrets |
| The cookie attribute discipline (httpOnly, Secure, SameSite, expiry/MaxAge) and the inline 401 error-envelope writer | carried forward into the new cookie helpers and middleware |

The v1 single-admin / password-login / store-backed-session-by-cookie model itself does not carry forward to the SaaS path; it is social login plus passwordless magic-link, both landing on JWT + refresh, with Cloudflare Access for the admin surface. The bootstrap admin (section 9) is the one place a password+bcrypt path survives, self-host only.

---

## 2. Social login (OIDC / OAuth2)

User-facing sign-in has no passwords. Besides the two social providers below (Google and GitHub), users can sign in with a passwordless magic link emailed to them (`internal/authn/magiclink.go`, the `EmailMagicLink` intent), which lands on the same JWT + refresh session. The admin surface uses Cloudflare Access instead (`internal/authn/cfaccess.go`). This section covers the social providers; api is the only service that talks to them (RFC-000 section 1.2). Both social flows use authorization-code with PKCE.

### 2.1 Provider mechanics

| Aspect | Google | GitHub |
|--------|--------|--------|
| Protocol | OIDC (OAuth2 + ID token) | OAuth2 (no OIDC ID token) |
| Discovery | `https://accounts.google.com/.well-known/openid-configuration` | static endpoints (`github.com/login/oauth/authorize`, `/access_token`) |
| Scopes | `openid email profile` | `read:user user:email` |
| Verified email source | `email` + `email_verified` claims in the ID token (we verify the ID token signature against Google JWKS) | not in the OAuth token. We call `GET https://api.github.com/user/emails` and pick the entry with `primary=true AND verified=true` |
| Profile | ID token + `userinfo` (name, picture) | `GET https://api.github.com/user` (name, avatar_url, login) |
| Provider user id | `sub` claim | numeric `id` from `/user` (stable, not the renamable login) |

The hard rule: we never act on an unverified provider email (PRD-001 section 3.1, master 5). For Google, `email_verified` must be true in the ID token. For GitHub, the email must come back `verified: true` from `/user/emails`. If no verified email is available, sign-in is refused with a clear message and no user is created (PRD-001 E1).

### 2.2 PKCE, state, nonce, CSRF on the flow

Authorization-code + PKCE is used for both providers, even though api is a confidential client holding a client secret. PKCE adds protection against authorization-code interception at no cost and is the current OAuth2 best practice. Per login attempt api generates:

| Value | Purpose | Storage |
|-------|---------|---------|
| `code_verifier` (43-128 random chars) | PKCE; `code_challenge = base64url(sha256(verifier))` sent on authorize, `verifier` sent on token exchange | server-side, keyed by `state`, in Redis with a short TTL (~10 min) |
| `state` (random, 32 bytes) | CSRF protection on the callback; binds the callback to the browser that started the flow | the random value is the Redis key; a copy is also set in a short-lived httpOnly cookie (`pulse_oauth_state`) so the callback can cross-check cookie vs query |
| `nonce` (random, 32 bytes) | replay protection on the OIDC ID token (Google); echoed in the ID token `nonce` claim and compared | stored alongside `state` in Redis |
| `return_to` (optional) | where to send the user after login (for example an invitation accept page); validated against an allowlist of internal paths to prevent open redirect | stored alongside `state` in Redis |

On the callback api requires: the `state` exists in Redis (not expired, single-use, deleted on consume), the `state` cookie matches the `state` query param, and for Google the ID token `nonce` matches the stored nonce. Any mismatch aborts the flow with a 400 and no session. `return_to` is never taken raw from the query; it is read from the server-side record bound to `state`, and even then it must pass the internal-path allowlist (section 10 open-redirect mitigation).

### 2.3 First sign-in: atomic user + personal-org creation

On a verified-email sign-in that matches no existing identity or user, api creates everything in one Postgres transaction (PRD-001 section 3.2, AC1):

1. Create `User` from the verified provider profile (primary_email = verified email, name, avatar).
2. Create `UserIdentity` (provider, provider_user_id, provider_email, email_verified=true).
3. Create a personal `Organization` (kind=personal, Free plan), named from the user's name or email.
4. Create a `Membership` making the user `owner`, occupying seat 1.
5. Issue tokens (section 3, 4) and route into onboarding.

If any step fails the whole transaction rolls back, so a user is never left with no org (PRD-001 I2). RFC-001 owns the schema and the transaction boundary; this RFC owns that the auth callback is what triggers it and that token issue happens only after the commit.

### 2.4 Account linking rules (PRD-001 section 3.3)

| Path | Trigger | Rule |
|------|---------|------|
| Auto-link on verified-email match | Sign-in with provider B, whose verified email equals an existing user's verified primary email (or a verified email on an already-linked identity) | Link the provider B identity to that existing user. No new user. Both emails must be verified |
| Manual link | A signed-in user clicks "Connect GitHub" / "Connect Google" in Account settings and completes that provider's OAuth | Attach the new identity to the current user even if its email differs, because the user proved control of both sessions. Refused if that provider account is already linked to another user (PRD-001 I5) |
| Divergent-email new user | Provider B returns a verified email matching no existing user | Create a new separate User (PRD-001 section 3.3, E2). No auto-merge. Merge is a manual support action in v1 |

The manual-link path requires an active authenticated session at the moment of the callback, so a forwarded OAuth callback cannot attach a stranger's provider to your account (PRD-001 section 3.3). api distinguishes "login flow" from "link flow" by a flag stored in the server-side `state` record set when the flow was started; a link flow with no current valid session is rejected.

Uniqueness invariants that auth enforces on link/sign-in (schema in RFC-001): at most one identity per provider per user (I4), a provider account maps to at most one user (I5).

### 2.5 Sequence: login (returning or first-time)

```
Browser            api                         Provider (Google/GitHub)     Postgres/Redis
  |                  |                                  |                          |
  | GET /auth/{p}/login                                 |                          |
  |----------------->|                                  |                          |
  |                  | make state,nonce,verifier        |                          |
  |                  | store {verifier,nonce,flow,return_to} keyed by state ------->| Redis (TTL 10m)
  |                  | Set-Cookie pulse_oauth_state=state (httpOnly, short)         |
  |                  | 302 to provider authorize?code_challenge,state,scope        |
  |<-----------------|                                  |                          |
  | follow 302 ------------------------------------------>| user consents          |
  |                  |                                  |                          |
  | 302 back to /auth/{p}/callback?code&state            |                          |
  |----------------->|                                  |                          |
  |                  | check state cookie == query state                           |
  |                  | load {verifier,nonce,flow} by state; delete it (single use)->| Redis
  |                  | POST token exchange (code + verifier + client secret) ------>|
  |                  |<-- access token (+ id_token for Google) ---------------------|
  |                  | Google: verify id_token sig vs provider JWKS, check nonce    |
  |                  | GitHub: GET /user, GET /user/emails -> pick primary+verified |
  |                  | refuse if no verified email                                  |
  |                  | match UserIdentity(provider, provider_user_id) ------------->| Postgres
  |                  |   hit  -> resume user                                        |
  |                  |   miss -> verified-email match? auto-link : create user+org  | (atomic txn)
  |                  | create refresh token row; sign access JWT ----------------->| Postgres
  |                  | Set-Cookie access + refresh (httpOnly,Secure,SameSite)       |
  |                  | 302 to return_to (allowlisted) or app home                   |
  |<-----------------|                                  |                          |
```

### 2.6 Sequence: invitation-accept via login (cold invite, PRD-001 section 6.5)

```
Invitee browser     api                                  Postgres/Redis
  |                   |                                        |
  | GET /invite/{token}                                        |
  |------------------>| load invitation by token ------------->| pending? not expired?
  |                   | no valid session -> need login          |
  |                   | start login flow with                  |
  |                   |   return_to=/invite/{token}/accept,     |
  |                   |   flow=login ------------------------->| Redis (state record)
  |                   | 302 to provider (section 2.5 login)     |
  |<------------------|                                        |
  |   ...full login round trip (creates user+personal org if new) ...
  |                   | after login, 302 to return_to=/invite/{token}/accept
  |------------------>| POST /invite/{token}/accept             |
  |                   | guard: signed-in verified email == invitation.invited_email
  |                   |   (match on primary OR any verified linked identity)
  |                   | mismatch -> 403 "sign in with {invited_email}"
  |                   | ok -> create Membership(role=target_role),
  |                   |        invite pending -> accepted, seat reserved -> occupied
  |                   |        (one txn) -------------------------------------->| Postgres
  |                   | active org may switch to the joined org (section 6)     |
  |<------------------| 200, land in the org                    |
```

The email-match guard on accept is the security-load-bearing step (PRD-001 D1, AC6): a forwarded invite link cannot be accepted by the wrong person because the signed-in verified email must equal the invited email. The new-user case is just the login leg creating the user first, then the same accept guard runs.

### 2.7 Enterprise SSO and SCIM (additional path, RFC-016)

Phase 3 adds enterprise SSO (SAML 2.0 and OIDC) and SCIM provisioning, defined in `docs/rfc/RFC-016-enterprise-sso-and-scim.md`. They do not replace social login; they extend it:

- **SSO** is one more way to authenticate. After an enterprise IdP verifies the user, RFC-016 maps them to the same `User` / `UserIdentity` / `Membership` model using the same account-linking rules (section 2.4: link by verified email, JIT-create if no user yet), then issues the **same** access JWT and refresh token (sections 3, 4). Everything downstream is unchanged: the access token stays identity-only (section 3.2), the request -> `(org, role)` resolution and the membership/role lookup (section 6) run exactly as they do for a social login, and the two-gate authz (section 7) is the same. An SSO identity is a `user_identities` row with `provider='sso'`.
- **SCIM** is a separate, IdP-driven provisioning API (no authentication of a person). It writes the same membership rows; a SCIM deactivate ends the membership and revokes the session using the same revocation path this RFC defines (section 4.3) and the same membership-cache bust (section 6.3), so offboarding takes effect on the next request.

This RFC's token, session, revocation, and authz contracts are the seam RFC-016 plugs into; RFC-016 owns the per-org connection model, domain routing, the assertion/token validation, and the SCIM server.

---

## 3. JWT access token (RS256)

### 3.1 Decision and claims

Decision: access tokens are RS256-signed JWTs with a ~15 minute lifetime. The token identifies the user, not the org.

| Claim | Value | Why |
|-------|-------|-----|
| `iss` | `https://api.pulsepager.com` | issuer, checked on verify |
| `aud` | `pulse-api` | audience, checked on verify |
| `sub` | user id (uuid) | the principal |
| `email` | user's verified primary email | convenience for the SPA and logs; authoritative source is still the DB |
| `iat` | issue time (epoch) | freshness |
| `exp` | iat + 15 min | short lifetime bounds stolen-token value |
| `jti` | random token id (uuid) | unique id, lets us deny-list a specific access token if ever needed |
| `typ` (custom claim `token_use`) | `access` | so a refresh-flow token cannot be replayed as an access token and vice versa |
| `kid` (header) | active signing key id | tells the verifier which JWKS key to use, enables rotation |

There is deliberately no `org`, no `role`, and no `scope` claim.

### 3.2 Why org is NOT in the token (confirmed)

RFC-000 says org switching needs no token reissue. Confirmed and explained:

- A user belongs to many orgs and switches between them with no re-auth (PRD-001 section 4.3, AC10... the switcher changes active context only). If the org lived in the token, every switch would force a token reissue, adding a round trip to a frequent UI action.
- Authorization must be evaluated against the active-org membership at request time (PRD-001 section 3.5, master 3, master 5). A role baked into a token goes stale the moment a role changes; a removed or demoted user must lose access within the refresh window (PRD-001 section 3.4 revocation timeliness). Putting role in the token would make revocation wait for the token to expire instead of the membership check catching it on the next request.
- So the token answers only "who is this person." The org comes from the request (section 6), and the role comes from a fresh membership lookup (section 6.3) on every request. This is what makes a role change take effect on the next request, not on the next token expiry.

Trade-off accepted: every authenticated request does a membership+role lookup. That lookup is Redis-cached (section 6.3), so it is sub-millisecond on the hot path and invalidated on membership change, which is exactly what makes timely revocation cheap.

### 3.3 Lifetime decision

Decision: access token ~15 minutes.

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| ~15 min (chosen) | chosen | Short enough that a stolen access token is useful only briefly and that a membership/role change takes effect within one refresh cycle (PRD-001 revocation timeliness). Long enough that refresh traffic is modest |
| ~60 min | rejected | A stolen token and a stale role both linger up to an hour; revocation window too wide for "takes effect quickly" |
| ~5 min | rejected | Refresh churn (and a refresh-rotation DB write per cycle, section 4) gets noisy for little extra security over 15 min |

Because role is not in the token, the practical revocation window is bounded by the membership-cache TTL (section 6.3), not by the 15 min token lifetime, for role and removal changes. The 15 min figure bounds the value of a stolen access token specifically.

### 3.4 Signing key management and rotation

- The RS256 private key lives only in api, loaded at boot from a Kubernetes secret sourced from KMS (RFC-000 section 2.1, section 10). It is never in an env var baked into an image and never leaves api.
- Each key has a `kid`. api signs with the current key and stamps its `kid` in the JWT header.
- JWKS (section 3.5) publishes the public key(s) by `kid`. During rotation, both the outgoing and incoming public keys are published so tokens signed by either verify cleanly.

Rotation strategy (overlap, no flag day):

| Step | Action |
|------|--------|
| 1 | Generate a new keypair with a new `kid`; add its public half to JWKS as a second key. Verifiers now accept both |
| 2 | Wait at least one access-token lifetime so any in-flight verifier has refreshed JWKS |
| 3 | Flip api to sign with the new `kid` |
| 4 | After the old key's last token has expired (>= one access lifetime past the flip), drop the old public key from JWKS |

Refresh tokens are opaque and DB-backed (section 4), so a signing-key rotation does not invalidate sessions; only the short-lived access tokens roll over, and the refresh on next use issues an access token under the new key.

### 3.5 JWKS endpoint

api serves `GET /.well-known/jwks.json` (RFC-000 section 1.2, 2.1). It returns the public key(s) in JWKS form (`kty`, `n`, `e`, `kid`, `use:sig`, `alg:RS256`). Purpose:

- Any future internal verifier or external client can verify a Pulse access token without a shared secret. In v1 no internal service verifies user JWTs (RFC-000 section 7.1); the endpoint exists so the seam is ready and so the SPA can verify locally if it ever wants to.
- It is cacheable (a short `Cache-Control`) so a verifier polls cheaply and picks up a rotated key.

### 3.6 RS256 over HS256 (ADR-0005)

Decision: RS256 (asymmetric), per RFC-000 ADR-0005.

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| RS256 (chosen) | chosen | The private signing key lives only in api. Verification uses the public key, so adding a verifier never means sharing a signing secret. A compromised verifier cannot mint tokens |
| HS256 | rejected | Symmetric: every verifier needs the same secret that signs. Sharing a signing secret across services widens the blast radius of any one compromise and there is no clean rotation story. The only win is marginally cheaper verification, which does not matter at our request rate |

---

## 4. Refresh tokens

### 4.1 Decision

Decision: refresh tokens are opaque (not JWTs), stored in Postgres, rotate on every use with reuse-detection, ~30 day lifetime.

| Property | Value |
|----------|-------|
| Format | opaque random (the v1 `newToken()` generator: 32 random bytes, base64url). What the client holds is the random secret; what we store is a hash of it (so a DB read cannot replay sessions) |
| Storage | a `refresh_tokens` row per active token (RFC-001 owns the table): `id`, `user_id`, `token_hash`, `family_id`, `issued_at`, `expires_at`, `revoked_at`, `replaced_by`, plus device/UA/IP for the sessions list |
| Lifetime | ~30 days sliding via rotation: each use issues a fresh refresh token; idle longer than 30 days and the token expires and the user re-logs-in |
| Rotation | on every refresh, the presented token is marked used and a new one is issued in the same `family_id` (a token "family" is one login chain) |
| Reuse detection | if a token that was already rotated (has a `replaced_by`) is presented again, the entire family is revoked and the user must re-login. A replayed old refresh token is the classic theft signal |

### 4.2 Why opaque + DB-backed (not a refresh JWT)

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| Opaque DB-backed (chosen) | chosen | A refresh token is the long-lived credential, so it must be individually and instantly revocable (logout, log-out-all, role change, removal). A DB row gives O(1) revoke. Rotation + reuse-detection is natural with a row. The DB read happens only on refresh (~ every 15 min per active session), not per request, so it is not a hot path |
| Refresh as a long-lived JWT | rejected | A self-contained JWT cannot be revoked before expiry without a deny-list, which is just a DB read by another name but with worse ergonomics. A 30-day un-revocable credential is exactly what we must not have |

### 4.3 Revocation

Revocation is the point of the DB-backed design. All of these are row updates that take effect on the next refresh (and the next membership check for role/removal):

| Trigger | Effect |
|---------|--------|
| Sign out (this device) | revoke the one refresh token (and its family); clear the cookies on that device |
| Log out of all devices (PRD-001 section 3.4, AC13) | revoke every non-revoked refresh token for the user across all families |
| Role change / removal from an org | does NOT revoke the session (the user still belongs to other orgs). It invalidates the membership cache for that (user, org) so the next request re-reads the new role or sees no membership. Access takes effect on the next request (PRD-001 AC12), bounded by the membership-cache TTL, not the refresh lifetime |
| Account deletion request (PRD-001 section 10) | runs log-out-all immediately so a deletion-pending account stops acting |
| Reuse detected | revoke the whole family |

The split matters: a role change is an authz fact, not a session fact, so it is handled by invalidating the membership cache (section 6.3), not by killing the user's session everywhere. Logout and log-out-all are session facts, handled by revoking refresh rows.

### 4.4 Browser delivery: access token cookie vs memory (decision)

Decision: both the access token and the refresh token are delivered as httpOnly cookies, same-origin to api.

| Cookie | Attributes | Path | Lifetime |
|--------|-----------|------|----------|
| `pulse_at` (access JWT) | HttpOnly, Secure, SameSite=Lax | `/` | session cookie (no Expires) or ~15 min |
| `pulse_rt` (refresh token) | HttpOnly, Secure, SameSite=Lax | `/auth` (sent only to the refresh and logout endpoints) | ~30 days |
| `pulse_csrf` (CSRF token) | Secure, SameSite=Lax, NOT HttpOnly (JS must read it to echo it) | `/` | matches access token |

Reasoning:

- The SPA is served same-origin behind the same nginx that proxies `/api` (RFC-000 section 3, 11.1), so cookies are first-party and SameSite=Lax is effective. There is no cross-site XHR that needs a bearer header.
- Putting the access token in an httpOnly cookie keeps it out of reach of any XSS that manages to run JS (it cannot read the token), which is a stronger default than localStorage or a JS-held variable.
- The refresh token's cookie is path-scoped to `/auth` so it is only sent to the refresh and logout endpoints, never on ordinary API calls. This shrinks where the long-lived credential travels.

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| Access in httpOnly cookie (chosen) | chosen | XSS cannot read it; same-origin makes cookies clean; one delivery mechanism for both tokens. Cost is we must defend CSRF (section 4.5), which is cheap on a same-origin SPA |
| Access in JS memory, refresh in httpOnly cookie | rejected for v1 | Avoids CSRF on the access path, but the access token is then reachable by XSS, and a memory-only token is lost on every tab reload forcing a refresh round trip on each load. The XSS exposure is the deciding factor. RFC-013 may keep a non-sensitive copy of claims (email, sub) in memory decoded from the cookie-less `/api/v1/me` response for rendering, but the token itself stays httpOnly |
| Access token in localStorage | rejected | Readable by any XSS, persists across tabs, worst option for a stolen-token threat. Not considered beyond naming it |

RFC-000 section 1.2 notes "JWT in Authorization header (or httpOnly cookie carrying it)". For the public API (RFC-012) the bearer header is the model (API keys, and machine JWT is not a thing here). For the browser SPA this RFC picks the httpOnly cookie. The Authorization-header path stays available for any non-browser caller that holds a JWT, but the SPA does not use it. This is consistent, not a deviation: header for machines, cookie for the browser.

### 4.5 CSRF stance

Because the browser auth is cookie-borne, CSRF must be defended. Decision: double-submit token plus SameSite=Lax, with an Origin/Referer check on unsafe methods.

| Layer | Mechanism |
|-------|-----------|
| SameSite=Lax | the access and refresh cookies are not sent on cross-site POST/PUT/DELETE initiated by a third-party page. This alone stops the classic cross-site form POST |
| Double-submit CSRF token | on login api sets a non-httpOnly `pulse_csrf` cookie. The SPA reads it and echoes it in an `X-CSRF-Token` header on every unsafe request. api requires header value == cookie value. A cross-site attacker cannot read the cookie (it is first-party) so cannot forge the header |
| Origin/Referer check | on unsafe methods api also checks the `Origin` (or `Referer`) is the app origin, as defense in depth |

The public API (API-key auth) is exempt from CSRF: it uses the `Authorization` header, not cookies, so there is no ambient credential to ride. CSRF applies only to the cookie-authenticated browser surface.

---

## 5. API keys

### 5.1 Format

`pulse_sk_<random>` where the random part is at least 22 base62 characters (>= 128 bits of entropy from the v1 `newToken()` generator, re-encoded base62 for a clean copy-paste token).

| Part | Example | Stored? |
|------|---------|---------|
| Fixed prefix | `pulse_sk_` | implied |
| Visible identifier prefix | first ~8 chars of the random part, for example `ab12cd34` | yes, as `prefix`, non-secret, shown in the list and safe in logs |
| Secret body | the remaining random chars | no, never stored in clear |

The full value presented on a request is `pulse_sk_<prefix><body>`. The client sends it as `Authorization: Bearer pulse_sk_...` (PRD-005 section 2.2). Note PRD-005 uses the spelling `pulse_live_` in its examples; this RFC standardizes on `pulse_sk_` ("secret key") and flags the difference for RFC-012 to align the OpenAPI spec. The behavior is identical; only the literal prefix string differs. (Deviation flagged.)

### 5.2 Hashing at rest (decision)

Decision: store SHA-256 of the full presented key (not bcrypt), and serve verification from a Redis cache.

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| SHA-256 (chosen) | chosen | An API key is 128 bits of uniform random secret, not a low-entropy human password. There is nothing to brute-force offline: a SHA-256 of 128 random bits is not crackable by dictionary or GPU. bcrypt's whole point is slowing down guesses of guessable secrets, which does not apply here. SHA-256 is a single fast hash, so it is cheap on the per-request verify path |
| bcrypt per request | rejected | bcrypt is deliberately slow (tens of ms at a sane cost). Running it on every public-API request, at the per-key rate limits in PRD-005 (up to ~600 req/min per key, many keys), would burn CPU for zero security gain over SHA-256 on a high-entropy secret. bcrypt is right for the bootstrap admin password (section 9), wrong for keys |
| HMAC-SHA-256 with a server pepper | considered, optional | A keyed hash means a DB dump alone cannot match keys without also stealing the pepper. We keep this as an easy hardening option (the pepper lives in the same KMS-backed secret as other api secrets); SHA-256 plain is the baseline and HMAC is a drop-in if we want the extra margin. Either is fast |

The deciding line: bcrypt-per-request is expensive and pointless for a random 128-bit secret. A fast hash plus a Redis cache (next) is the correct shape for a high-rate verify path.

### 5.3 Lookup path (Redis cache + revocation invalidation)

The presented key is hashed and looked up. The hash is the cache key so the clear secret is never in Redis either.

```
Request: Authorization: Bearer pulse_sk_ab12cd34<body>
  |
  | h = sha256("pulse_sk_ab12cd34<body>")
  v
Redis GET apikey:{h}
  |-- hit  -> {org_id, role, key_id, revoked?} (cached non-secret descriptor)
  |            revoked? -> 401. else use it.
  |
  |-- miss -> Postgres: SELECT ... FROM api_keys WHERE token_hash = h
  |            not found / revoked -> 401 (cache a short negative TTL to blunt scans)
  |            found            -> cache {org_id, role, key_id} (TTL ~5 min)
  v
context = (org_id from key, actor_kind=api_key, actor_id=key_id, role from key)
async: update last_used_at (throttled, not on every request)
```

| Concern | Handling |
|---------|----------|
| Revocation immediacy (PRD-005 AC3: revoked key fails on the very next request) | on revoke, api deletes the `apikey:{h}` cache entry (and marks the row revoked). The next request misses the cache, reads the row, sees revoked, returns 401. Cache TTL is the only window and revoke proactively busts it, so "next request" holds |
| last-used (PRD-005 section 2.1) | updated asynchronously and throttled (for example at most once per minute per key) so the hot path does not write on every request |
| negative caching | unknown-hash lookups cache a short-TTL negative result so a scan of random keys does not hammer Postgres |
| Redis down | fail closed for keys: if the cache is unavailable, fall back to the Postgres lookup directly (it is correct, just slower). Unlike entitlements, key verify must never fail open |

### 5.4 Role-scoping, per-org binding, and resolution without a JWT

| Rule | Behavior | Source |
|------|----------|--------|
| Role | a key is created with `member` or `admin` only; never owner-equivalent | PRD-001 section 7.3, PRD-005 section 2.1 |
| Per-org | a key belongs to exactly one org and acts only within it; no key spans orgs | PRD-005 section 2.1 |
| (org, role) without a JWT | the key row carries `org_id` and `role`. The verify step returns both, so the request context is fully resolved from the key alone with no JWT and no org header. The org is fixed by the key; a request cannot ask a key to act in a different org | PRD-005 section 2.3 |
| Owner actions unreachable | because no key is ever owner, billing management, ownership transfer, and org deletion fail the role gate for every key, which is exactly why they stay UI-only human actions (PRD-001 Appendix A, PRD-005 section 4.7) | PRD-001, PRD-005 |
| Created-by, name | each key records who created it and a human label | PRD-005 section 2.1 |

So an API-key request needs no token and no org context from the request: the key is the credential, the org, and the role at once.

---

## 6. Request -> (org, role) resolution

This is the middleware chain in api that turns an incoming request into the `(org_id, actor, role)` context that authz reads.

### 6.1 The chain

```
incoming request
   |
   v
[1] authn middleware: identify the principal
      - Authorization: Bearer pulse_sk_...  -> API key path (section 5.3)
            -> context = (org_id from key, actor=key, role from key); SKIP step 2 and 3
      - cookie pulse_at (or Authorization: Bearer <jwt>) -> JWT path
            -> verify RS256 (iss, aud, exp, token_use=access, kid->JWKS)
            -> context.user_id = sub ; no org, no role yet
      - neither / invalid -> 401 unauthenticated
   |
   v
[2] org-context resolution (JWT path only)
      - active org from the request: path /api/.../orgs/{org_id}/... for the SPA,
        or X-Pulse-Org header as the alternate; one mechanism, see 6.2
   |
   v
[3] membership + role lookup (JWT path only)
      - role = membership(user_id, org_id).role  (Redis-cached, 6.3)
      - no membership -> 403 (the user is not in that org; do not 404-leak existence)
   |
   v
context = (org_id, actor_id, actor_kind, role)  ->  authz (section 7) + entitlements (RFC-009)
   |
   v
   set Postgres session var app.current_org for RLS (RFC-000 section 6.1) in the request txn
```

### 6.2 Org-context mechanism (decision)

Decision: for human (JWT) requests the active org is request-supplied and explicit. The SPA primary mechanism is path-based under `/orgs/{org_id}`; an `X-Pulse-Org` header is the accepted alternate for non-path-shaped calls. For API-key requests the org is fixed by the key and no org parameter is read.

| Option | Verdict | Reasoning |
|--------|---------|-----------|
| Path `/orgs/{org_id}` for SPA + header alternate, key fixes org for API (chosen) | chosen | The active org is session/UI state and switching is a frequent no-reauth action (PRD-001 section 3.5, 4.3). Making the org explicit per request (in the path) means the org switcher just changes which path the SPA calls, with no token reissue and no server session mutation. It is visible, cacheable, and unambiguous in logs. The header alternate covers calls that are not naturally path-scoped. For the public API the org must be implied by the key (PRD-005 section 4 "Org is implied by the key"), so the key path reads no org parameter at all |
| Active org stored server-side in the session and mutated on switch | rejected | Makes the session stateful and the active org a hidden, mutable global. Two browser tabs on different orgs would fight over one server-side active org. Switching would be a write. Explicit per-request org avoids all of this |
| Org only ever in a cookie | rejected | A cookie active-org is ambient and shares the two-tab problem; it is also easy to desync from what the URL shows. A cookie may mirror the last-used org as a UX convenience (where to land on next visit) but it is not the authority for a request; the path/header is |

Whichever way the org arrives for a human, it is always checked against the user's membership (step 3) before it is trusted. Supplying an org you are not a member of yields 403, never access. This is the cross-org escalation guard (section 10).

### 6.3 Membership + role lookup (cached, invalidated on change)

- The lookup is `(user_id, org_id) -> role` (or "no membership"). It runs on every JWT request, so it is Redis-cached: `member:{user_id}:{org_id} -> role` with a TTL (~5 min) and Postgres as source of truth.
- Invalidation is event-driven: a role change, a removal, or a new membership deletes the relevant `member:{user_id}:{org_id}` cache entry so the next request re-reads. This is what makes "a demoted or removed user sees the change on their next refreshed request" hold (PRD-001 section 3.4, AC12) without waiting for token expiry.
- The cache TTL is the worst-case staleness if an invalidation is missed; proactive bust on the change keeps the real window to one request.

This is the same cache-with-invalidation shape as entitlements (RFC-000 section 12), but for membership role. It feeds authz; entitlements is a separate cache feeding the entitlement gate.

---

## 7. AuthZ / RBAC

### 7.1 The model as code

RBAC is evaluated in api, in `internal/authz`, against the caller's active-org role, on every request (RFC-000 section 7.3). The PRD-001 section 7.2 matrix and the master 4 matrix are expressed as a permission set per role. A role's set is the union of every action it may take; a higher role is a superset of the lower where the matrix says so, except the deliberately owner-only existential actions.

### 7.2 Permission set per role

Actions are named capabilities (the matrix rows). `Y` = the role's set contains the action.

| Action (capability) | Owner | Admin | Member | Viewer | API key reachable? |
|---------------------|:-----:|:-----:|:------:|:------:|:------------------:|
| view monitors / incidents / history / status / channels (read) | Y | Y | Y | Y | yes (any key) |
| create/edit/delete monitor, check-now | Y | Y | Y | N | yes (member+) |
| create/edit/delete channel, send test | Y | Y | Y | N | yes (member+) |
| acknowledge / annotate incident | Y | Y | Y | N | yes (member+) |
| create/edit/publish status page | Y | Y | Y | N | yes (member+) |
| manual close incident | Y | Y | N | N | yes (admin) |
| view member list and roles | Y | Y | Y | Y | yes (read) |
| invite member / set invited role | Y | Y | N | N | yes (admin) |
| resend / revoke invitation | Y | Y | N | N | yes (admin) |
| change member role (admin: not to/from owner) | Y | Y* | N | N | yes (admin) |
| remove member (admin: not an owner) | Y | Y* | N | N | yes (admin) |
| view audit log | Y | Y | N | N | yes (admin) |
| edit org settings (name, slug, defaults) | Y | Y | N | N | yes (admin) |
| configure custom domain for status page | Y | Y | N | N | yes (admin) |
| create / revoke API key | Y | Y | N | N | yes (admin) |
| view billing and usage | Y | Y | N | N | yes (admin, read) |
| create a new organization | Y | Y | Y | Y | n/a (per-user, not org-scoped) |
| transfer ownership | Y | N | N | N | NO (owner-only) |
| manage billing (plan, payment, invoices) | Y | N | N | N | NO (owner-only) |
| delete the organization | Y | N | N | N | NO (owner-only) |
| demote / remove the last owner | N (blocked, I1) | N | N | N | NO |

`*` admin restrictions: admin may change roles and remove members but never to/from owner and never an owner (PRD-001 section 7.2). These are encoded as guards inside the action, not separate rows.

Self-scoped account actions (manage own profile, linked providers, sessions, log-out-all, delete own account, "orgs I belong to") are role-independent and available to every role for their own user (PRD-001 section 7.3). They are not org-scoped capabilities and so are not in the matrix above; they are authorized by "actor == subject," not by role.

### 7.3 The `Can` evaluation seam

`internal/authz` exposes one decision function:

```
authz.Can(actor Actor, action Action, resource Resource) Decision
  Actor    = { Kind: human|api_key, UserID/KeyID, OrgID, Role }
  Action   = a named capability (the rows above)
  Resource = the target (its org_id, and for member/role actions the target's role)
  Decision = Allow | Deny(reason)
```

- The permission-set-per-role is a static table in `internal/authz`. `Can` looks up whether `actor.Role`'s set contains `action`, then applies action-specific guards that need the resource (the at-least-one-owner invariant, admin-cannot-touch-owner, target-org == actor-org).
- It is pure: no I/O, no DB. The middleware has already resolved `(org, role)` (section 6) and fetched whatever resource fields the guard needs, then calls `Can`. Purity makes it unit-testable against the matrix exhaustively.
- It runs in api handlers. A handler computes the required `action` for its operation, builds the `Actor` from the request context and the `Resource` from the target, and calls `Can`. A deny is a 403 with the standard envelope (`code: "forbidden"`).

### 7.4 Composition with the entitlement gate (two independent gates)

Per PRD-005 section 9.2 and RFC-000 section 12, every write is checked against two gates and both must pass:

```
                handler for an unsafe operation
                          |
              +-----------+-----------+
              v                       v
       [Role gate]              [Entitlement gate]
   authz.Can(actor,           entitlements.Check(org, limit)
     action, resource)          (RFC-009, RFC-000 section 12)
   role >= action min-role    plan allows it: Free read-only,
                              monitor cap, interval floor,
                              region set, seat cap, ...
              |                       |
         Allow/Deny              Allow/Deny(entitlement_exceeded)
              |                       |
              +-----------+-----------+
                          v
              both Allow -> perform ; any Deny -> reject
```

| Property | Role gate | Entitlement gate |
|----------|-----------|------------------|
| Question | "is this actor's role allowed to do this action?" | "does this org's plan allow this?" |
| Owns | `internal/authz` (this RFC) | `internal/entitlements` (RFC-009, RFC-000 section 12) |
| Deny code | `forbidden` (403) | `entitlement_exceeded` (with upsell) |
| Independence | neither trusts the other | a Free-org admin key passes the role gate but fails the entitlement gate on a write (PRD-005 section 9.1), so both are needed |

The two gates are deliberately separate. Role is "who you are in this org"; entitlement is "what this org's plan permits." A Free org's admin still cannot write, not because of role but because of plan. Owner-only billing management fails the role gate for every key, which is why it stays UI-only (PRD-005 section 4.7).

### 7.5 The at-least-one-owner invariant

The invariant "every active org has at least one owner" (PRD-001 I1, section 4.6) is enforced as a guard inside the relevant actions in `internal/authz` plus a database-level check (RFC-001):

| Attempted action | Guard |
|------------------|-------|
| demote the last owner | `Can` denies if the target is the only owner of the org |
| remove the last owner | denied if target is the only owner |
| last owner leaves | denied; "transfer ownership or delete the org" |
| last owner deletes the org | allowed (the only path that ends the org) |

The guard needs the org's owner count, which the handler supplies in the `Resource` (a count or a "is-last-owner" flag read in the same transaction). RFC-001 backs this with a constraint so a race cannot drop the last owner even if two requests run concurrently. Co-owners are allowed (PRD-001 D7); the invariant is "at least one," not "exactly one."

---

## 8. Package design

### 8.1 `internal/authn`

Owns everything about proving who a caller is and minting/verifying their credentials.

| Area | Responsibility |
|------|----------------|
| OIDC/OAuth2 | provider config (Google OIDC, GitHub OAuth2), authorize-URL building with PKCE/state/nonce, callback handling, token exchange, ID-token verification (Google), GitHub `/user` + `/user/emails`, verified-email extraction |
| Linking | the three account-linking paths (section 2.4) against RFC-001 identity rows |
| JWT | RS256 issue (claims of section 3.1), verify (iss/aud/exp/token_use/kid), key holder and rotation, the `kid` selection |
| JWKS | serve `/.well-known/jwks.json` from the current public key set |
| Refresh | issue, rotate, reuse-detect, revoke, log-out-all (section 4) |
| API keys | generate (`pulse_sk_...` + prefix), hash (SHA-256), verify with the Redis cache, revoke + cache bust, throttled last-used (section 5) |
| Cookies | the cookie helpers (httpOnly/Secure/SameSite, path-scoped refresh, CSRF cookie), carried forward from the v1 cookie discipline |

### 8.2 `internal/authz`

Owns the decision, nothing about identity.

| Area | Responsibility |
|------|----------------|
| RBAC matrix | the permission-set-per-role table (section 7.2) |
| `Can(actor, action, resource)` | the pure evaluation seam (section 7.3), including the owner-only and at-least-one-owner guards |
| Action catalog | the named capabilities, so handlers and RFC-012 (min-role per endpoint) refer to one list |

`authz` imports nothing from `authn`; it takes an already-resolved `Actor`. This keeps the decision pure and testable and lets either package change without dragging the other.

### 8.3 What carries forward from v1 `internal/auth`

As section 1.4: bcrypt (now only for the bootstrap admin), `newToken()` (now the opaque-secret generator for refresh tokens and API keys), and the cookie + 401-envelope discipline. The v1 single-admin/password/session-by-cookie model itself is replaced by `authn` + `authz`.

### 8.4 api wiring

```
nginx -> api router
  middleware stack (order):
    1. obs (trace id, slog)            [RFC-010]
    2. authn.Identify                  -> sets actor (user or key) or 401
    3. org-context + membership/role   -> sets (org_id, role) or 403   [JWT path]
    4. set app.current_org for RLS     [RFC-000 6.1 / RFC-001]
  handler:
    action := required capability for this route
    if !authz.Can(actor, action, resource): 403
    if !entitlements.Check(org, limit): entitlement_exceeded   [RFC-009]
    perform; emit audit.events with the actor (RFC-000 5.1)
  public routes (no authn): OAuth login/callback, /.well-known/jwks.json,
    status-page read path, Paddle webhook (its own signature check)
```

### 8.5 Service-to-service: no re-verification

Internal services (scheduler, worker, alerting, notifier) do not verify user JWTs or API keys (RFC-000 section 7.1). api authorizes once at the edge and carries `org_id` and the acting principal as data on the events it publishes (`monitor.changed`, `audit.events`). Downstream services trust that data because it arrived over the authenticated internal channel, protected by Kubernetes NetworkPolicy + TLS-to-infra (RFC-000 section 7.2, RFC-011), not by re-checking a token. A check job that outlives its triggering access token must still run, which is exactly why identity is propagated as immutable data, not as a re-verifiable token.

---

## 9. Self-host bootstrap admin

The hosted SaaS is social-login only and has no superuser. The optional self-host build keeps a single env-provided bootstrap admin so a fresh instance is not a chicken-and-egg lockout before any OAuth app is configured (PRD-001 section 11).

| Aspect | Behavior |
|--------|----------|
| Where | self-host build only; compiled/guarded out of the hosted SaaS so it can never exist in multi-tenant hosting (an env superuser there would be a cross-tenant master key, violating the isolation invariant, PRD-001 section 11) |
| How set | a single env-provided superuser (email + secret) read at boot. The secret is bcrypt-hashed (this is the one place v1 bcrypt password handling carries forward, section 1.4 / 8.3). It is not stored as a normal `User` with a provider identity |
| What it can do | bootstrap only: configure the OAuth apps (client ids/secrets), create the first real org, grant the first human owner. It is an operator/break-glass account, not a tenant, and is not part of any org's RBAC |
| Coexistence with OIDC | once OAuth is configured and a real owner exists via Google/GitHub, day-to-day is social-login exactly like SaaS. The bootstrap admin stays an operator break-glass |
| Kept off the SaaS hot path | the bootstrap-admin code path is behind a self-host build flag and is never reached on a hosted request. The normal authn middleware (JWT / API key) has no branch for it in the SaaS build, so it adds nothing to the hosted request path |
| Constraints | exactly one bootstrap admin (PRD-001 N7); its actions are audited as a distinct operator actor; operators are advised to rotate or disable the env secret after setup |

So bcrypt + password lives only here, only in self-host, only for the one operator secret, and the SaaS path never sees it.

---

## 10. Threats and mitigations

Tied to RFC-000 section 10 (security) and RFC-011 (deployment security: NetworkPolicy, TLS, KMS).

| Threat | Vector | Mitigation |
|--------|--------|------------|
| Access-token theft | XSS reads the token, or it is sniffed | access token in httpOnly+Secure cookie (XSS cannot read it; TLS everywhere stops sniffing); ~15 min lifetime bounds usefulness; `jti` allows a targeted deny-list if ever needed (section 3, 4.4) |
| Refresh-token theft / replay | a stolen long-lived refresh token | refresh tokens rotate on every use with reuse-detection: a replay of an already-rotated token revokes the whole family and forces re-login (section 4.1); the refresh cookie is path-scoped to `/auth` so it travels less |
| CSRF | a third-party page rides the cookie | SameSite=Lax + double-submit CSRF token (attacker cannot read the first-party CSRF cookie to forge the header) + Origin check on unsafe methods (section 4.5). API-key surface is header-auth so it is not CSRF-exposed |
| Open redirect on OAuth callback | attacker sets `return_to` to an external URL to phish post-login | `return_to` is never read raw from the query; it is read from the server-side record bound to `state`, and must pass an internal-path allowlist. No external origins are honored (section 2.2) |
| OAuth CSRF / fixation on the flow | attacker forces a victim through the attacker's login or replays a callback | `state` bound to a single browser via cookie cross-check, single-use and Redis-TTL'd; OIDC `nonce` checked on Google ID tokens; PKCE binds the code to the verifier (section 2.2) |
| API key leakage | a key committed to a repo or leaked in logs | only the non-secret prefix is ever stored/logged; the secret is shown once and stored only as a SHA-256 hash; keys are revocable immediately with cache bust (section 5); keys max out at admin so a leak cannot touch billing/ownership/org-deletion (PRD-001 Appendix A) |
| Privilege escalation across orgs | a user or key supplies another org's id | the supplied org is always checked against membership (section 6.1 step 3): no membership -> 403. An API key's org is fixed by the key and cannot be overridden by a request. RLS (`app.current_org`, RFC-000 6.1) is the DB backstop so even a missed app check cannot read another org's rows |
| Privilege escalation within an org | a member calls an admin-only operation | the role gate (`authz.Can`) denies; min-role per endpoint is the matrix row (section 7.2, PRD-005 section 4). The role is re-read fresh per request (section 6.3) so a demotion takes effect on the next request |
| Last-owner removal | a race or a buggy call drops the only owner | the `Can` owner-count guard plus an RFC-001 DB constraint enforce I1 even under concurrency (section 7.5) |
| Session fixation | attacker fixes a session id before login | sessions are token-based, not a server-set id the attacker can pre-seed; a fresh refresh token (and access token) are minted only after a successful provider sign-in, in a new family; the pre-login `state`/PKCE artifacts are single-use and unrelated to the post-login session (section 2, 4) |
| Stolen signing key | api private key compromise | key lives only in api from a KMS-backed secret (never in an image/env); rotation via `kid` overlap can retire a key without a flag day (section 3.4); RS256 means a compromised verifier cannot mint tokens (section 3.6) |
| Token confusion | a refresh token used as an access token or vice versa | `token_use` claim on the JWT and the opaque/JWT format difference make the two non-interchangeable (section 3.1, 4) |

---

## 11. Open questions and dependencies

### 11.1 Open questions

| # | Question | Lean |
|---|----------|------|
| Q1 | API key prefix string: PRD-005 examples say `pulse_live_`, this RFC standardizes `pulse_sk_`. Which wins? | `pulse_sk_` (clearer "secret key"); RFC-012 to align the OpenAPI auth scheme. Flagged as a deviation in section 5.1 |
| Q2 | Membership-cache TTL vs access-token lifetime. The real revocation window for role/removal is the membership-cache TTL (section 6.3). Is ~5 min acceptable for "takes effect quickly," or should role changes also force a refresh-token re-eval? | ~5 min TTL with proactive bust on change keeps the real window to one request; no session kill needed for a role change. Confirm with security review |
| Q3 | Should we keyed-hash (HMAC + pepper) API keys from day one, or start with plain SHA-256? | start SHA-256, HMAC is a drop-in hardening (section 5.2); decide with RFC-011 on whether the pepper is worth the extra KMS secret |
| Q4 | Access-token cookie vs memory for the SPA. This RFC picks httpOnly cookie; RFC-013 must confirm the SPA never needs the raw token in JS (it should not, same-origin) | cookie; RFC-013 to confirm and own the `/api/v1/me` claims-for-render shape (section 4.4) |
| Q5 | Distinguishing human vs API-key actors in the audit trail (RFC-000 open question 2) | carry `actor_kind` in `audit.events`; RFC-001/RFC-000 to settle the audit schema |

### 11.2 Dependencies

| RFC | Direction | What |
|-----|-----------|------|
| RFC-001 (data model) | this RFC depends on it | the user / user_identity / organization / membership / api_key / refresh_token / invitation tables, the at-least-one-owner DB constraint, RLS (`app.current_org`), the cross-tenant test suite |
| RFC-009 (entitlements) | this RFC composes with it | the entitlement gate that runs alongside the role gate (section 7.4); the Free read-only and metered-limit checks |
| RFC-012 (API) | depends on this RFC | the bearer-key auth scheme in OpenAPI, the per-endpoint min-role (matrix), the 401/403 distinction, the two-gate behavior, the `pulse_sk_` prefix alignment (Q1) |
| RFC-013 (frontend) | depends on this RFC | the cookie-based token handling, the org switcher driving the `/orgs/{id}` path, the CSRF header echo, the login redirect on 401 |
| RFC-007 (notifier) | depends on this RFC | outbound webhook signing secrets are a key-like secret stored hashed/encrypted; signing format is owned by RFC-007, the secret-handling discipline is shared here |
| RFC-010 (observability) | this RFC feeds it | the auth SLIs (login success/failure, token verify failures, key verify cache hit ratio) and the actor in structured logs |
| RFC-011 (deploy/security) | this RFC depends on it | KMS-backed signing key and pepper secrets, NetworkPolicy + TLS for the service-to-service trust this RFC relies on |

---

## 12. Decisions summary

| # | Decision | Rejected alternative |
|---|----------|----------------------|
| D1 | OIDC/OAuth2 with authorization-code + PKCE, state cookie cross-check, OIDC nonce, server-side `return_to` allowlist | implicit flow; raw `return_to` from query (open redirect) |
| D2 | RS256 access JWT, ~15 min, org NOT in the token | HS256 (shared secret); ~60 min (revocation window too wide); org-in-token (forces reissue on switch, stale role) |
| D3 | Opaque DB-backed refresh tokens, rotating, reuse-detected, ~30 days | refresh-as-JWT (not revocable before expiry) |
| D4 | Access + refresh in httpOnly+Secure+SameSite cookies (same-origin), CSRF via double-submit | access token in JS memory / localStorage (XSS-reachable) |
| D5 | API key `pulse_sk_<random>`, SHA-256 at rest, Redis-cached verify with revoke-bust | bcrypt per request (slow, pointless on a 128-bit random secret) |
| D6 | Org context: path `/orgs/{id}` (+ header alt) for humans, fixed by the key for the API; always checked vs membership | server-side mutable active org; cookie as the authority |
| D7 | Two independent gates: `authz.Can` role gate AND entitlement gate, both must pass | a single combined check that conflates role and plan |
| D8 | `internal/authn` (identity/tokens/keys) + `internal/authz` (pure `Can`); v1 bcrypt only for the self-host bootstrap admin | one monolithic auth package; bcrypt for API keys |
