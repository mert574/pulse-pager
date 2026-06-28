# RFC-016 - Enterprise SSO and SCIM

Status: DRAFT for review
Author: Principal Identity Architect
Audience: api service authors, frontend (RFC-013), data model (RFC-001), entitlements (RFC-009), compliance (RFC-015), and anyone wiring enterprise identity into Pulse
Parent: `docs/rfc/RFC-003-authn-authz.md` (the identity model SSO extends), `docs/rfc/RFC-000-architecture-overview.md` (section 7 identity propagation, section 10 security, section 12 entitlement enforcement)
Product source of truth: `docs/prd/PRD-001-identity-and-tenancy.md` (users, orgs, memberships, seats, invitations, linking; N2 names SSO/SCIM as phase 3), `docs/prd/PRD-006-billing-and-entitlements.md` (SSO/SCIM is an Enterprise-tier entitlement), `docs/PRD.md` section 15 (phase 3: SSO/SCIM)
Phase: phase 3 enterprise (master section 15). This RFC designs the identity seam now so the user/membership model accommodates SSO without a later rewrite.

House style: no em-dashes. Tables and diagrams over prose. Every load-bearing choice states the decision, the reasoning, and the rejected alternative.

---

## 1. Overview, scope, and how SSO fits the existing model

### 1.1 What this RFC is

Enterprises want their people to sign in to Pulse through their own identity provider (Okta, Azure AD / Entra, Google Workspace, OneLogin, or any generic SAML 2.0 / OIDC IdP) and to have user accounts created and removed automatically by that IdP. This RFC fixes how Pulse does both:

- **SSO** is one more way to authenticate. It does not replace social login (RFC-003 section 2). After an enterprise user is authenticated by their IdP, Pulse resolves them to the same `User` / `Membership` model and issues the same Pulse session and access JWT as any other login. Everything downstream of authentication (the request -> `(org, role)` resolution, the RBAC matrix, the two-gate authz) is unchanged (RFC-003 sections 3, 4, 6, 7).
- **SCIM** is a separate provisioning API the IdP calls to create, update, and deactivate users and to push group membership. It does not authenticate anyone; it manages the user/membership rows that SSO logins then resolve against.

The one-line abstraction that the rest of this RFC holds to: **an SSO connection yields a verified identity plus attributes scoped to one org.** Whether the SAML/SCIM normalization is done in-house or by a provider (section 2), Pulse always owns the mapping from that verified identity to a Pulse user and membership.

### 1.2 Where SSO plugs into the RFC-003 pipeline

```
                  RFC-003 today                         RFC-016 adds
  +-------------------------------------+   +-------------------------------------+
  | social login (Google/GitHub OAuth)  |   | enterprise SSO (SAML 2.0 / OIDC)    |
  | -> verified email + provider profile|   | -> verified email + IdP attributes  |
  +------------------+------------------+   +------------------+------------------+
                     |                                         |
                     +--------------------+--------------------+
                                          v
                       link by verified email / JIT provision   (RFC-003 2.4 rules,
                       -> Pulse User + Membership                 extended in section 5,6)
                                          v
                       issue access JWT + refresh token          (RFC-003 sections 3, 4
                       -> normal Pulse session                    UNCHANGED)
                                          v
                       request -> (org, role) resolution         (RFC-003 section 6
                       -> authz.Can + entitlement gate            UNCHANGED)
```

SSO is a new producer of "a verified identity for an org" that feeds the exact same consumer (link/JIT -> session issue -> authz). SCIM is a parallel writer of the same user/membership rows.

### 1.3 Scope

In scope: the build-vs-buy decision (section 2); the per-org SSO connection model for SAML and OIDC (section 3); domain capture and login routing, including enforced SSO (section 4); the SAML and OIDC authentication flows with their full assertion/token validation (section 5); JIT vs SCIM provisioning (section 6); the SCIM 2.0 server, deprovisioning, and role mapping (sections 7, 8); the multi-tenancy and security model with the SAML-specific threats (section 9); the data model additions for RFC-001 (section 10); the Enterprise entitlement gate (section 11); compliance and audit (section 12); phasing (section 13); the touchpoints to reconcile (section 14); and the ADRs to capture (section 15).

Out of scope (named, owned elsewhere): the social-login flows, JWT crypto, refresh tokens, API keys, and the request -> `(org, role)` middleware (RFC-003, all unchanged); the user/org/membership/identity schema as DDL (RFC-001, which this RFC adds tables to conceptually in section 10); the entitlement cache and enforcement mechanism (RFC-009 / PRD-006); the audit storage and retention (RFC-002 section 4.8, RFC-010); the SSO login button and the admin connection-config UI (RFC-013). Custom roles beyond the four are still phase 3 and out of scope here (PRD-001 N3); SSO maps IdP groups onto the existing four roles only.

### 1.4 Contracts this RFC owns

| Contract | Decision in this RFC |
|----------|----------------------|
| Build vs buy | use an enterprise-SSO provider (WorkOS as the reference) for the SAML/OIDC/SCIM normalization layer; Pulse owns the user/membership mapping (section 2) |
| SSO connection | per-org, one or more connections, protocol `saml` or `oidc`, status `draft`/`active` (section 3) |
| Domain routing | verified email-domain claims per org, DNS-TXT verified, optional enforced SSO (section 4) |
| Identity link | an SSO identity is a `user_identities` row with `provider='sso'` and a connection id; same uniqueness rules as RFC-003 (sections 5, 10) |
| JIT default role | `member`, overridable by group mapping; never `owner` via JIT or SCIM (sections 6, 8) |
| SCIM | per-connection bearer token (hashed), `/scim/v2/Users` and `/scim/v2/Groups`, deactivate -> revoke session/refresh immediately (section 7) |
| Entitlement | SSO and SCIM configuration is Enterprise-tier only, gated by the entitlement (section 11) |

---

## 2. The build-vs-buy decision (the key ADR)

### 2.1 Decision

**Use an enterprise-SSO provider as the SAML / OIDC / SCIM normalization layer, and have Pulse own the mapping from the provider's verified identity to the Pulse user and membership.** The reference provider is **WorkOS**; Stytch is an acceptable equivalent. The provider terminates the per-IdP SAML and OIDC protocol details and exposes Pulse one normalized shape: "a verified profile (email, name, IdP groups, raw attributes) for connection C, which belongs to org O." The provider also runs the SCIM server surface and forwards normalized provisioning events (user created / updated / deactivated, group membership changed) to Pulse, which applies them to the same `users` / `memberships` rows that section 7 describes.

The whole rest of this RFC is written so it holds for **either** path. The abstraction Pulse codes against is "an SSO connection yields a verified identity + attributes for an org" (section 1.1). The in-house path (section 2.4) implements that same abstraction with Pulse's own SAML/OIDC/SCIM code instead of a provider SDK. Nothing in sections 3 through 12 changes shape based on the choice; only who parses the XML and hosts the SCIM endpoints changes.

### 2.2 Why buy (the reasoning)

| Factor | Why it favors a provider |
|--------|--------------------------|
| SAML per-IdP quirks | SAML 2.0 is a large spec and every IdP (Okta, Entra, OneLogin, ADFS, Google) implements a slightly different subset: NameID formats, attribute names, signing vs encryption choices, IdP-initiated vs SP-initiated support, clock-skew tolerances. A provider has already absorbed thousands of these quirks. Building and maintaining that matrix in-house is a long tail of customer-specific bugs that has nothing to do with uptime monitoring. |
| SCIM server burden | A correct SCIM 2.0 server is more than two endpoints: it is PATCH semantics, filtering, pagination, the `Groups` vs `Users` membership model, and per-IdP deviations (Okta and Entra both bend the spec differently). A provider runs this surface and hands Pulse normalized events. |
| Certificate / metadata rotation | SAML signing certificates expire and rotate; IdP metadata changes. A provider tracks rotation and re-fetches metadata. In-house we would own a rotation/expiry monitoring job and the on-call pages when a customer's cert silently expires. |
| Time to market | SSO/SCIM is a phase-3 sales unblocker, not a differentiator. A provider gets a working SAML + OIDC + SCIM surface in weeks; in-house is a multi-quarter project with a security-review cost. We want the deal-closing checkbox, not a SAML library. |
| Maintenance | The provider absorbs the ongoing spec drift, new-IdP onboarding, and security patches in the protocol layer. Our maintenance is the thin mapping layer, which is stable. |

### 2.3 Why buy is not free (the trade-offs we accept)

| Trade-off | Our stance |
|-----------|------------|
| A provider sees auth metadata | The provider sees the SAML assertion / OIDC token and the user's email, name, and IdP groups at login, and the SCIM payloads. It does not see monitor data, incidents, or anything in Pulse's tenant tables; it only ever sees identity metadata. We treat the provider as a sub-processor: it goes in the sub-processor list, under a DPA, and is named in the SOC 2 vendor-management process (section 12, RFC-015). For a customer with strict data-flow requirements that cannot accept a sub-processor in the auth path, the in-house path (2.4) is the escape hatch, sold as a custom Enterprise option. |
| Lock-in | The provider's normalized shape is wrapped behind a Pulse-internal `ssoidentity` interface (the section 1.1 abstraction), so the provider SDK is one adapter behind that interface. Swapping providers, or moving to the in-house path, is reimplementing that adapter, not rewriting the mapping, session, or authz code. We do not let the provider's data model leak past the adapter. |
| Cost | Per-connection or per-active-user pricing. Acceptable because SSO is Enterprise-tier only (section 11) where the contract value dwarfs the per-seat SSO cost. We do not pay for SSO on Free/Hobby/Professional because it is entitlement-gated off there. |
| Compliance posture | A reputable provider is itself SOC 2 / ISO 27001 certified, which is usually a net positive for our own SOC 2 story (a vetted sub-processor) rather than a negative. |

### 2.4 The rejected alternative: build SAML + multi-IdP + SCIM in-house

| Aspect | In-house path |
|--------|---------------|
| What we would build | A SAML 2.0 SP (metadata, ACS endpoint, assertion parsing and the full XML-signature validation of section 9), an OIDC RP (already partly present from RFC-003 social login, but per-customer issuer/JWKS handling is new), and a SCIM 2.0 server (`/scim/v2/Users`, `/scim/v2/Groups`, PATCH, filtering, pagination). |
| Why rejected for v1 | The SAML XML-signature validation surface (XSW, signature stripping, comment injection in NameID) is exactly the kind of security-critical code where a subtle bug is a cross-customer auth bypass. Getting it right and keeping it right across IdP quirks is a dedicated, ongoing security investment that does not move the monitoring product forward. The time-to-market and maintenance costs (2.2) are the disqualifiers. |
| When we would revisit | Two triggers: (1) provider cost becomes material at scale, or (2) a strategic customer's data-flow / data-residency rules forbid any third party in the auth path. Because everything is behind the section 1.1 abstraction, revisiting means writing the in-house adapter, not redesigning identity. The OIDC RP work is the cheapest to in-source first (we already verify ID tokens for Google in RFC-003); SAML and SCIM are the expensive parts and stay with the provider longest. |

The deciding line: building a SAML SP and a SCIM server is a security-critical, quirk-heavy, never-finished project that is orthogonal to uptime monitoring. We buy the protocol layer, own the identity mapping, and keep the in-house path open behind an abstraction.

---

## 3. SSO connection model (per-org)

### 3.1 The connection

Each enterprise org configures one or more **SSO connections**. A connection is the binding between one org and one IdP, plus how that IdP's attributes map onto Pulse.

| Field | SAML | OIDC |
|-------|------|------|
| `org_id` | the owning org; a connection authenticates only into this org (section 9) | same |
| `protocol` | `saml` | `oidc` |
| IdP identity | `idp_entity_id` (IdP entityID), `idp_sso_url` (the IdP SSO/redirect endpoint), `idp_x509_cert` (the IdP signing certificate(s), 1..2 for rotation) | `oidc_issuer`, `oidc_client_id`, `oidc_client_secret` (encrypted), discovery via `{issuer}/.well-known/openid-configuration` |
| SP / RP identity Pulse exposes | SP entityID `https://api.pulsepager.com/sso/saml/{connection_id}`, ACS URL `https://api.pulsepager.com/sso/saml/{connection_id}/acs` | redirect URI `https://api.pulsepager.com/sso/oidc/{connection_id}/callback` |
| `status` | `draft` (configured, not yet live) -> `active` (login routing on) | same |
| `attribute_mapping` | which SAML attributes map to email / name / groups (defaults to common Okta/Entra names, overridable) | which OIDC claims map to email / name / groups |
| `enforced` | whether members of this org must use SSO (section 4.3) | same |

When the provider path (section 2) is used, the IdP-facing fields (`idp_x509_cert`, `idp_sso_url`, issuer, client creds, metadata) live in the provider and Pulse stores the provider's connection id plus the org binding and the Pulse-owned mapping/enforcement fields. The conceptual columns in section 10 cover both: the in-house path fills the protocol fields, the provider path leaves them null and fills `provider_connection_id`.

### 3.2 Metadata exchange

SSO setup is a two-way metadata exchange between Pulse (the Service Provider / Relying Party) and the customer's IdP.

| Direction | SAML | OIDC |
|-----------|------|------|
| Pulse -> IdP (what Pulse publishes) | Pulse exposes SP metadata XML at `GET /sso/saml/{connection_id}/metadata` containing the SP entityID, the ACS URL, the NameID format requested, and Pulse's own signing/encryption certificate. The admin hands this to their IdP. | Pulse publishes the redirect URI and the standard OIDC RP details; the admin registers Pulse as an OAuth client in their IdP and copies back client id/secret. |
| IdP -> Pulse (what the admin enters) | The admin uploads the IdP metadata XML (or its URL), which gives Pulse the IdP entityID, SSO URL, and signing certificate in one document; manual field entry is the fallback. | The admin enters the issuer URL; Pulse fetches discovery and JWKS automatically. |

With the provider path, this exchange happens through the provider's admin portal (which can be embedded in Pulse's settings UI via the provider's admin-portal link), and Pulse stores only the resulting connection id and binding.

### 3.3 Certificate and metadata rotation

| Concern | Handling |
|---------|----------|
| IdP SAML signing cert rotation | a connection holds up to two IdP certs at once (`idp_x509_cert` is a small set), so an assertion verifies if it was signed by either, letting the IdP roll certs with no downtime. The admin adds the new cert before the IdP flips, then removes the old one after. With the provider path this is the provider's job. |
| Pulse SP cert rotation | Pulse's own SP signing/encryption cert is rotated on the same overlap discipline as the JWT signing key (RFC-003 section 3.4): publish the new cert in SP metadata alongside the old, let IdPs pick it up, then retire the old. |
| OIDC key rotation | OIDC JWKS rotation is automatic: Pulse re-fetches the IdP's JWKS on a cache miss / unknown `kid`, the same pattern RFC-003 uses for Google. |
| Expiry monitoring | a job warns admins (and emits an audit/ops signal) before an IdP cert expires, so a connection does not silently break. With the provider path the provider owns expiry tracking; Pulse surfaces the provider's warning in the admin UI. |

---

## 4. Domain capture and login routing

### 4.1 The problem

An enterprise user lands on the Pulse login page and types `alice@acme.com`. Pulse must route her to Acme's IdP, not show her the Google/GitHub buttons (or, if SSO is enforced for Acme, hide those buttons entirely). That routing is driven by **verified email-domain claims** owned by the org.

### 4.2 Domain verification (DNS TXT)

An org claims one or more email domains. A claim is only honored once verified, so one org cannot capture another org's domain (a serious takeover risk if unverified).

| Step | Behavior |
|------|----------|
| Claim | an owner/admin of an Enterprise org enters a domain (`acme.com`) in SSO settings. A row is created in `org_domains` with `status='pending'` and a random `verification_token`. |
| Prove | the admin adds a DNS TXT record (`pulse-verification=<token>`) to the domain. Pulse periodically resolves the TXT record; on a match the domain flips to `status='verified'`. |
| Use | only `verified` domains drive login routing (4.3) and only verified domains may be used as the JIT auto-link basis (section 5). |
| Conflict | a domain can be verified by at most one org (unique on the normalized domain among `verified` rows). A second org claiming an already-verified domain stays `pending` and cannot verify, which prevents domain hijack. |

### 4.3 Login routing (SP-initiated discovery) and enforced SSO

```
user types alice@acme.com on the login page
        |
        v
lookup verified org_domains for "acme.com"
        |
  +-----+------------------------------+
  | found: org=Acme, active connection |   not found
  v                                    v
route to Acme's IdP                  show normal social login (RFC-003)
(SP-initiated SAML / OIDC, section 5)
        |
  enforced SSO for Acme?
   yes -> social/password buttons are not offered to acme.com users;
          they must come through the IdP
   no  -> SSO is available, but the user may also use social login
          for their personal identity
```

| Aspect | Behavior |
|--------|----------|
| Discovery | typing an enterprise email (or visiting an org's SSO login URL) routes to that org's active connection. This is SP-initiated login (section 5). |
| Optional enforced SSO | an org may turn on `enforced` (section 3.1). When enforced, members of that org must authenticate through the IdP; social/password login is not offered for that org's verified domains. This is what an enterprise security team expects ("everyone goes through Okta"). |
| Reconcile with the personal-org model | enforced SSO is scoped to the **enterprise org**, not to the human globally. A person can still have a personal org (PRD-001 section 4.2) reached by social login with their personal email. Enforcement says "to act inside Acme's org you came through Acme's IdP," not "this human may never use Google again." A user whose work email is under an enforced domain and who also has a personal Google account keeps both: the enforced rule governs access to the Acme org; their personal org is untouched. If an org wants to forbid its members from also holding social logins on the work identity, that is an attribute/SCIM concern, not a login-routing one, and is out of scope for v1. |

### 4.4 IdP-initiated login

SAML also allows IdP-initiated login (the user clicks the Pulse tile in their Okta dashboard and arrives at Pulse's ACS with an unsolicited assertion). Pulse supports it but treats it as the higher-risk path (section 9): there is no Pulse-generated `RelayState`/request to bind against, so replay and CSRF defenses lean entirely on assertion-id replay caching, audience/recipient checks, and `NotOnOrAfter`. OIDC is SP-initiated only; there is no IdP-initiated OIDC in this design.

---

## 5. Authentication flow

After the IdP authenticates the user, Pulse validates the assertion/token, maps it to a Pulse user (link by verified email per RFC-003 section 2.4, or JIT-create if no user yet), and issues the **normal** Pulse session and access JWT (RFC-003 sections 3, 4). The validation in 5.1 and 5.2 is the security-load-bearing part; it is what section 9 hardens.

### 5.1 SAML SP-initiated login

```
Browser           api (SP)                         IdP                    Postgres/Redis
  |                  |                               |                          |
  | GET /sso/saml/{conn}/login  (or via domain discovery, 4.3)                  |
  |----------------->|                               |                          |
  |                  | make AuthnRequest id + RelayState; store {id,RelayState, |
  |                  |   return_to} keyed by RelayState ----------------------->| Redis (TTL ~10m)
  |                  | 302 to IdP SSO URL with SAMLRequest + RelayState         |
  |<-----------------|                               |                          |
  | follow 302 ------------------------------------->| user authenticates       |
  |                  |                               |                          |
  | POST /sso/saml/{conn}/acs  (SAMLResponse + RelayState)                      |
  |----------------->|                               |                          |
  |                  | RelayState exists in Redis? single-use; delete -------->| Redis
  |                  | VALIDATE the assertion (5.3):                            |
  |                  |   - XML signature valid against the connection's IdP cert(s)
  |                  |   - signature covers the assertion (anti-XSW, section 9) |
  |                  |   - Issuer == connection idp_entity_id                   |
  |                  |   - Audience == Pulse SP entityID for this connection    |
  |                  |   - Recipient == this ACS URL                            |
  |                  |   - SubjectConfirmation NotBefore/NotOnOrAfter within skew
  |                  |   - InResponseTo == our stored AuthnRequest id          |
  |                  |   - assertion ID not seen before -------------------->| Redis replay cache
  |                  |   - decrypt EncryptedAssertion if present               |
  |                  | extract verified email + name + groups (attr mapping)   |
  |                  | refuse if no verified email (RFC-003 rule)              |
  |                  | map to Pulse user (5.3) ----------------------------->| Postgres
  |                  |   identity hit  -> resume user                          |
  |                  |   miss + verified-email match -> link (RFC-003 2.4)     |
  |                  |   miss + no match -> JIT create user+identity+membership| (atomic txn)
  |                  | apply group->role mapping (section 8)                   |
  |                  | issue refresh token + sign access JWT (RFC-003 3,4) -->| Postgres
  |                  | Set-Cookie pulse_at + pulse_rt (httpOnly,Secure)        |
  |                  | 302 to return_to (allowlisted) or app home              |
  |<-----------------|                               |                          |
```

### 5.2 OIDC login

```
Browser           api (RP)                         IdP                    Postgres/Redis
  |                  |                               |                          |
  | GET /sso/oidc/{conn}/login                       |                          |
  |----------------->|                               |                          |
  |                  | make state + nonce + PKCE verifier; store keyed by state>| Redis (TTL ~10m)
  |                  | 302 to IdP authorize?client_id&code_challenge&state&nonce&scope=openid email profile groups
  |<-----------------|                               |                          |
  | follow 302 ------------------------------------->| user authenticates       |
  |                  |                               |                          |
  | GET /sso/oidc/{conn}/callback?code&state         |                          |
  |----------------->|                               |                          |
  |                  | load {nonce,verifier} by state; single-use delete ----->| Redis
  |                  | POST token exchange (code + verifier + client secret) -->|
  |                  |<-- id_token + access_token --------------------------- |
  |                  | VALIDATE id_token (5.3):                                 |
  |                  |   - signature vs IdP JWKS (fetch by issuer, cache by kid)|
  |                  |   - iss == connection oidc_issuer                       |
  |                  |   - aud == connection oidc_client_id                    |
  |                  |   - exp / iat within skew; nonce == stored nonce        |
  |                  | extract verified email (email_verified) + name + groups |
  |                  | refuse if no verified email                             |
  |                  | map to Pulse user / JIT (5.3) ----------------------->| Postgres
  |                  | apply group->role mapping (section 8)                   |
  |                  | issue refresh + access JWT (RFC-003 3,4) ------------->| Postgres
  |                  | 302 to return_to or app home                           |
  |<-----------------|                               |                          |
```

### 5.3 Validation checklist (both protocols)

| Check | SAML | OIDC | Why |
|-------|------|------|-----|
| Signature | XML-DSig valid against the connection's IdP cert(s) | id_token JWS valid against the IdP JWKS | the assertion/token must come from the configured IdP |
| Signature scope | the signature must cover the assertion that supplies the subject; reject if the signed element is not the one we read (anti-XSW, section 9) | n/a (the JWT is one signed object) | XSW is the headline SAML attack |
| Issuer | `Issuer` == connection `idp_entity_id` | `iss` == connection `oidc_issuer` | bind the assertion to this connection's IdP |
| Audience | `Audience` == Pulse SP entityID for this connection | `aud` == connection `oidc_client_id` | the assertion was minted for Pulse, not another SP (audience confusion, section 9) |
| Recipient / ACS | `Recipient` == this ACS URL | redirect URI matches | stop assertion redirection to a different endpoint |
| Time window | `NotBefore` / `NotOnOrAfter` within a small clock skew (~2-3 min) | `exp` / `iat` within skew | bound the validity window |
| Replay | assertion `ID` cached until `NotOnOrAfter`; reject a repeat | `jti`/`nonce` single-use; reject a repeat | stop assertion/token replay |
| Request binding | `InResponseTo` == our stored AuthnRequest id (SP-initiated) | `state` and `nonce` == stored values | bind the response to the request the browser started |
| Encryption | decrypt `EncryptedAssertion` if the connection negotiated encrypted assertions | TLS protects the token in transit | confidentiality of attributes at rest in transit |

After validation, the mapping to a Pulse user reuses RFC-003 section 2.4 exactly: an existing SSO identity resumes its user; a verified-email match to an existing user auto-links the SSO identity to that user; no match triggers JIT (section 6). Then the normal session/JWT issuance runs (RFC-003 sections 3, 4) with nothing SSO-specific in the token (the access JWT is still identity-only, org resolved per request, RFC-003 section 3.2).

When the provider path (section 2) is used, steps "VALIDATE the assertion/token" are performed by the provider and Pulse receives the already-verified normalized profile over the provider callback (itself authenticated). Pulse still owns everything from "map to Pulse user" onward. The validation table stays the spec Pulse holds the provider to and the spec the in-house path implements directly.

---

## 6. JIT provisioning vs SCIM

Two ways a user/membership comes into existence for an enterprise org. They are not mutually exclusive; a connection may use one or both.

| | JIT provisioning | SCIM provisioning |
|--|------------------|-------------------|
| Trigger | first successful SSO login by a user who has no Pulse user yet (section 5) | the IdP pushes a create/update/deactivate to Pulse's SCIM API (section 7), independent of any login |
| Creates | `User` + `user_identities(provider='sso')` + `Membership` in the org, in one transaction (mirrors RFC-003 section 2.3) | same rows, driven by the IdP's directory |
| Default role | `member` (section 8), overridden by group mapping if groups are present in the assertion | the role from the group/attribute mapping at provisioning time, default `member` |
| Deprovision | JIT alone has no deprovision signal; access ends only when the membership is removed in Pulse or the IdP stops asserting the user (they can no longer log in, but a stale membership lingers) | deactivate is an explicit signal: the membership is disabled and sessions/refresh tokens revoked immediately (section 7.3) |
| When it applies | the lightweight option: an org turns on SSO, and users get a Pulse account the first time they log in. Good for smaller enterprise orgs that do not run SCIM | the full lifecycle option: the IdP is the source of truth, users exist in Pulse before they ever log in, and offboarding in the IdP removes Pulse access automatically. Required for orgs that need fast, auditable deprovisioning |
| Combined | JIT can create on first login while SCIM keeps the directory in sync afterward; SCIM deactivate is still the authoritative offboarding path | |

The decision rule: **JIT is the minimum for SSO to work; SCIM is what makes deprovisioning fast and auditable.** An enterprise security review almost always asks for SCIM precisely because JIT-only leaves deprovisioning to manual membership removal, which is the gap SOC 2 reviewers flag (section 12).

---

## 7. SCIM 2.0 provisioning

### 7.1 The SCIM server surface

Per connection, Pulse (or the provider on Pulse's behalf, section 2) exposes a SCIM 2.0 server the IdP calls.

| Endpoint | Methods | Behavior |
|----------|---------|----------|
| `/scim/v2/Users` | POST (create), GET (list/filter), GET `/{id}`, PUT/PATCH (update), DELETE or PATCH `active=false` (deactivate) | maps a SCIM user to a Pulse `User` + `user_identities(provider='sso')` + `Membership` in the connection's org |
| `/scim/v2/Groups` | POST, GET, PATCH (membership add/remove), DELETE | maps a SCIM group to the connection's `group_role_mappings`; group membership drives the user's role (section 8) |
| `/scim/v2/ServiceProviderConfig`, `/Schemas`, `/ResourceTypes` | GET | the SCIM discovery documents IdPs read to learn what Pulse supports |

### 7.2 SCIM auth and idempotency

| Concern | Handling |
|---------|----------|
| Auth | each connection has a SCIM **bearer token** the IdP sends as `Authorization: Bearer <token>`. The token is generated once, shown once, and stored hashed (SHA-256, same discipline as API keys in RFC-003 section 5.2 and `api_keys` in RFC-001). A SCIM request authenticates the connection (and therefore its org) and can only touch that org's users (section 9). |
| Token model | the SCIM token is per-connection, not per-user; it authenticates the IdP's directory sync, not a person. It is revocable (rotate generates a new token, hashes it, and the old hash stops matching), and its use is audited (section 12). |
| Idempotency | SCIM creates are idempotent on `(connection_id, external_id)` (the IdP's stable user id) and on verified email: a repeated create returns the existing user rather than duplicating. Updates are idempotent by nature (PATCH/PUT to a known id). This matches the at-least-once posture elsewhere in Pulse (RFC-000 / ADR-0009): the IdP may retry, and a retry must not create a second user or a second membership (the `uniq_membership_user_org` invariant, RFC-001, backs this). |

### 7.3 Deprovisioning (the fast-revoke path)

Deprovisioning is the reason enterprises want SCIM, so it must be immediate.

```
IdP: PATCH /scim/v2/Users/{id} {active:false}   (or DELETE)
        |
        v
api: authenticate SCIM bearer token -> connection -> org
        |
   in one transaction:
   - disable the Membership in this org (end the membership; free the seat per PRD-001 5.1)
   - revoke ALL of the user's refresh tokens scoped to this org's access
        (RFC-003 section 4.3 revocation: log-out path)
   - invalidate the membership cache member:{user_id}:{org_id}  (RFC-003 6.3)
        |
        v
effect: the next request the user makes resolves "no membership in this org" -> 403,
        and they cannot refresh into a new access token for this org.
        Their other orgs (e.g. their personal org) are untouched.
```

| Tie-in to RFC-003 | How |
|-------------------|-----|
| Session revocation | deactivate runs the same refresh-token revocation RFC-003 section 4.3 already defines for removal; the user's session for this org cannot be refreshed |
| Membership-cache bust | the same event-driven invalidation as a role change (RFC-003 section 6.3); the next request re-reads "no membership" |
| Access token window | the short-lived access JWT (RFC-003 section 3.3, ~15 min) is the only residual window, and it is bounded by the membership cache bust catching the next request, exactly as for any other removal |

A SCIM deactivate does not delete the `User` row globally (the person may exist in other orgs and owns their personal org); it ends their membership in the connection's org and kills that org's session, consistent with PRD-001's "removal frees the seat, other orgs untouched" (section 5.3).

### 7.4 Group membership and role

SCIM `Groups` carry membership; a user's Pulse role in the org is derived from the IdP groups they belong to, through the connection's `group_role_mappings` (section 8). A SCIM group-membership change (PATCH add/remove member) re-evaluates that user's role and busts the membership cache, so a group change in the IdP takes effect on the user's next request, the same timeliness as a role change made inside Pulse.

---

## 8. Role mapping

### 8.1 IdP groups / attributes -> Pulse roles

Pulse keeps the four roles (owner, admin, member, viewer; PRD-001 section 7, no custom roles in v1). A connection maps IdP groups (or a role attribute) onto those four.

| Source | Mapping | Default |
|--------|---------|---------|
| IdP group name (from the SAML assertion, OIDC `groups` claim, or SCIM Group) | `group_role_mappings(connection_id, idp_group, pulse_role)` | a user in no mapped group gets the connection's default JIT role |
| Default JIT role | `member` | the safe default: an operator, not an admin, never an owner |
| Multiple groups | if a user matches several mapped groups, the **highest** mapped role wins (owner > admin > member > viewer) | |

### 8.2 The owner invariant and SSO

SSO and SCIM **cannot create or remove owners freely**, because owner is the existential role (billing, deletion, ownership transfer; PRD-001 Appendix A) and those are deliberately human, UI-only actions.

| Rule | Reason |
|------|--------|
| JIT and SCIM never auto-assign `owner` | owner is existential; an IdP group should not be able to mint a billing-capable owner. The highest role SSO/SCIM grants is `admin`. This mirrors the API-key ceiling (RFC-003 section 5.4: keys max at admin). |
| SSO/SCIM must not remove the last owner | the at-least-one-owner invariant (PRD-001 I1, RFC-003 section 7.5) is enforced in the same `authz` guard plus the DB-level check. A SCIM deactivate or a group change that would drop the org's last owner is refused at the membership write, exactly as a UI attempt would be. The IdP cannot strand an org. |
| Owner is granted in-product | an enterprise org gets its first owner through the normal flow (the human who set up the org, or an ownership transfer), not through the IdP. SSO/SCIM manage admin-and-below. |

If a mapping would set someone to owner, it is clamped to admin and the attempt is audited (section 12), so the mapping config cannot silently exceed the ceiling.

---

## 9. Multi-tenancy and security

### 9.1 Per-org isolation of SSO

| Invariant | Enforcement |
|-----------|-------------|
| A connection authenticates only into its own org | the `org_id` on the connection is the only org an assertion through that connection can resolve into; the ACS/callback URL carries `{connection_id}`, and the resulting membership/JIT is created with that connection's `org_id`, never an org from the assertion. An assertion cannot name a different org. |
| Domain capture is verified and unique | `org_domains` are DNS-TXT verified and unique among verified rows (section 4.2), so one org cannot route another org's users or claim their domain. |
| SCIM token scopes to one org | a SCIM bearer token authenticates one connection -> one org; every SCIM write is org-scoped through the same RLS path as all tenant writes (RFC-001 section 5, `app.current_org` set from the connection's org). A leaked SCIM token cannot touch another org's users. |
| RLS backstop | SSO/SCIM-created users and memberships go through the same repository + RLS layer (RFC-001 section 5); even a bug in the SSO mapping cannot write a membership into the wrong org because the `WITH CHECK` policy rejects a cross-org insert. |

### 9.2 SAML / OIDC / SCIM threats and mitigations

| Threat | Vector | Mitigation |
|--------|--------|------------|
| XML Signature Wrapping (XSW) | attacker wraps a forged assertion around a legitimately-signed one so the parser reads the forged subject while signature validation passes on the real element | validate that the signature covers the exact element whose subject we consume; use a hardened SAML library that resolves the signed element by reference and rejects multiple assertions / moved elements; never select the assertion by position. With the provider path, this is the provider's vetted parser (a core reason to buy, section 2.2). |
| Signature stripping / unsigned assertions | attacker presents an assertion with no signature, or strips it, hoping Pulse accepts it | require a valid signature on every assertion; reject unsigned or `SignatureValue`-absent responses outright. There is no "optional signature" mode. |
| Assertion / token replay | resend a captured assertion or id_token | cache the assertion `ID` (SAML) / `jti`+`nonce` (OIDC) until `NotOnOrAfter`/`exp`; reject any repeat (section 5.3). `InResponseTo` / `state` bind SP-initiated responses to a request. |
| IdP-initiated CSRF / replay | unsolicited assertion at the ACS (no Pulse request to bind) | IdP-initiated login has no `InResponseTo` to check, so it leans on audience + recipient + `NotOnOrAfter` + assertion-id replay cache; the assertion is still single-use. Orgs that want the stricter posture can disable IdP-initiated and use SP-initiated only (section 4.4). |
| Audience confusion | an assertion minted for another SP is replayed at Pulse | require `Audience` (SAML) / `aud` (OIDC) to equal Pulse's SP entityID / client id for this connection; reject otherwise (section 5.3). |
| Open redirect on ACS / callback | attacker sets `return_to` / `RelayState` to an external URL to phish post-login | `return_to` is read from the server-side record bound to `RelayState`/`state`, never raw from the request, and must pass the internal-path allowlist (RFC-003 section 2.2). No external origin is honored. |
| SCIM token leakage | a leaked per-connection SCIM bearer token | the token is stored hashed (never recoverable), is per-connection (one org), is revocable by rotation, and every SCIM action is audited (section 12). RLS confines a leaked token to its own org's users (9.1). |
| Privilege escalation via group mapping | a manipulated `groups` attribute or a mapping that grants too much | groups come only from a signature-validated assertion / authenticated SCIM call, so they cannot be forged in transit; the mapping clamps to admin (never owner, section 8.2); the last-owner invariant cannot be broken by SSO. |
| Cert / metadata tampering | attacker swaps the IdP cert or SSO URL | connection config is owner/admin-only and audited; cert/metadata changes are audit events (section 12); the provider path tracks rotation centrally (section 3.3). |
| Encrypted-assertion downgrade | attacker strips encryption from an assertion the connection expects encrypted | if a connection negotiated `EncryptedAssertion`, a plaintext assertion is rejected, not silently accepted. |

These extend the RFC-003 section 10 threat model rather than replacing it; the access-token, refresh-token, CSRF, and cross-org escalation mitigations there all still apply because SSO feeds the same session and the same request -> `(org, role)` pipeline.

---

## 10. Data model additions (for RFC-001)

Conceptual columns; RFC-001 owns the DDL, indexes, constraints, and RLS. All org-owned tables here get `org_id` and an RLS policy (RFC-001 section 5). These are additive: nothing in the existing schema changes shape, which is what makes designing SSO in now cheap (section 13).

| Table | Tenancy | Conceptual columns |
|-------|---------|--------------------|
| `sso_connections` | org | `id`, `org_id`, `protocol` (saml/oidc), `status` (draft/active), `display_name`; SAML: `idp_entity_id`, `idp_sso_url`, `idp_x509_certs` (set, 1..2 for rotation), `sp_cert_encrypted`; OIDC: `oidc_issuer`, `oidc_client_id`, `oidc_client_secret` (ENCRYPTED); `provider_connection_id` (the WorkOS/provider id, null on the in-house path); `attribute_mapping` (jsonb: email/name/groups source); `enforced` (bool); `created_at`, `updated_at` |
| `scim_tokens` | org | `id`, `org_id`, `connection_id`, `token_hash` (HASHED, SHA-256), `prefix` (non-secret, for the list view), `created_by`, `created_at`, `revoked_at` |
| `org_domains` | org | `id`, `org_id`, `domain` (normalized lowercase), `status` (pending/verified), `verification_token`, `verified_at`, `created_at`. Unique on `domain` among `status='verified'` (one org per verified domain, section 4.2) |
| `group_role_mappings` | org | `id`, `org_id`, `connection_id`, `idp_group` (the IdP group name / id), `pulse_role` (admin/member/viewer; never owner per section 8.2), `created_at` |
| SSO identity link | global (FK to user) | extend `user_identities` (RFC-003 / RFC-001) with `provider='sso'` plus a nullable `sso_connection_id`, so an SSO identity is the same row shape as a Google/GitHub identity. `provider_user_id` is the IdP subject / SCIM external id; `provider_email` and `email_verified` carry as today. Uniqueness rules I4/I5 (RFC-001 section 4.1) hold: one SSO identity per (user, connection), one IdP subject maps to one user |

Notes for RFC-001:

- `user_identities.provider` CHECK currently allows `('google','github')`; it gains `'sso'`. The SSO identity additionally carries `sso_connection_id` so the same IdP subject under two different org connections is two distinct identities (a contractor in two customer orgs is two memberships and two SSO identities on one user, linked by verified email per RFC-003 section 2.4).
- `scim_tokens` reuses the `api_keys` hashing discipline exactly (SHA-256, prefix shown, secret shown once); the verify path can reuse the same Redis-cached lookup shape (RFC-003 section 5.3) since a SCIM call is high-rate during a directory sync.
- All four new org-owned tables fall under the section 5 RLS policy and the section 5.4 cross-tenant test suite must gain SSO cases (a connection / SCIM token / domain / mapping from org A is invisible and unusable from org B).

---

## 11. Entitlement gating

SSO and SCIM are an **Enterprise-tier entitlement** (PRD-006 section 1.3, section 3 Enterprise note; master section 11 / 15). Configuration is gated behind it, the same two-gate way every other plan-limited feature is (RFC-003 section 7.4, RFC-009 / PRD-006 section 5).

| Gated action | Entitlement | Behavior when not entitled |
|--------------|-------------|----------------------------|
| Create / activate an `sso_connection` | `sso_enabled` (Enterprise) | rejected with an upsell `code` (PRD-006 envelope), the same shape as `seat_limit_reached` etc. |
| Generate a SCIM token / call the SCIM API | `scim_enabled` (Enterprise) | config rejected; SCIM endpoints return not-entitled for a non-Enterprise org |
| Claim/verify an `org_domain` for SSO routing | `sso_enabled` | rejected if the org is not Enterprise |

| Detail | Handling |
|--------|----------|
| New entitlement fields | `entitlements` (RFC-001 section 4.2, PRD-006 section 2.2) gains `sso_enabled BOOLEAN` and `scim_enabled BOOLEAN`, defaulted from the plan (Enterprise = true, all others = false). RFC-009 owns how this is cached and invalidated; this RFC only says SSO/SCIM config sits behind these flags. |
| Where the gate runs | on the config write path (creating/activating a connection, generating a SCIM token, claiming a domain), and on the SCIM API auth path, using the same cached entitlement read as every other gate (PRD-006 section 5.3). Login through an already-active connection is on the auth hot path and reads the same cache. |
| Downgrade | if an Enterprise org downgrades, SSO/SCIM config is not silently deleted (PRD-006's no-silent-delete rule, section 6.2). The connection is deactivated (login routing stops, enforced SSO lifts so users are not locked out), and the owner is prompted; the config rows are preserved for a re-upgrade. Locking enterprise users out on a billing event would be the worst possible failure, so deactivate-not-delete is the rule. |

---

## 12. Compliance

SSO and SCIM are standard SOC 2 / enterprise-security requirements (master section 13: SOC 2 path designed in; the SSO/SCIM, audit-log, RBAC requirements come from the enterprise persona, master section 2 persona C). This RFC keeps the path open (RFC-010 section 10.4, RFC-015 owns the compliance program) by making SSO config changes and SCIM events auditable.

| Audited event | When | Actor |
|---------------|------|-------|
| `sso.connection_created` / `_activated` / `_deactivated` / `_updated` | admin changes a connection (incl. cert/metadata/enforced changes) | human (owner/admin) |
| `sso.domain_claimed` / `_verified` | a domain is claimed and later verifies | human / system |
| `sso.login` | a successful SSO login worth auditing (high volume; same retention stance as `auth.login`, PRD-001 D5) | the SSO user |
| `scim.token_created` / `_revoked` | a SCIM token is generated or rotated | human |
| `scim.user_provisioned` / `_updated` / `_deactivated` | the IdP creates/updates/deactivates a user | system (the SCIM connection), with the connection id as the actor |
| `scim.group_membership_changed` / `role_mapped` | a group change or a role re-evaluation | system |
| `sso.role_clamped_to_admin` | a mapping tried to grant owner and was clamped (section 8.2) | system, security-relevant |

These reuse the existing `audit_events` table and the `actor_type` split (RFC-001 section 4.6, which already has `human`/`api_key`/`system`); a SCIM-driven event is a `system` actor carrying the connection id, which is exactly the seam RFC-001 left for non-human actors. Audit storage, retention by plan, and the owner/admin view are owned by RFC-002 / RFC-010 / the audit subsystem, not by this RFC; this RFC only names the events it emits. The provider sub-processor (section 2.3) goes into the vendor-management and sub-processor list that RFC-015 / the SOC 2 program tracks.

---

## 13. Phasing

| When | What ships |
|------|-----------|
| Now (phase 1/2, this RFC) | the identity model is designed so SSO is additive: `user_identities` already exists and gains a `provider='sso'` value (section 10); the request -> `(org, role)` pipeline (RFC-003 section 6) already resolves a membership regardless of how the user authenticated; domain routing, per-org connection, and the entitlement flags are reserved seams. No SSO code ships, but nothing has to be retrofitted. |
| Phase 3, milestone A (SSO login) | stand up the provider integration (section 2), the `sso_connections` / `org_domains` / `group_role_mappings` tables, the SAML and OIDC login flows (section 5) with JIT provisioning (section 6), domain verification and routing (section 4), and the Enterprise entitlement gate (section 11). This is the deal-closing "log in with Okta" checkbox. |
| Phase 3, milestone B (SCIM + enforced SSO) | add the SCIM server (via the provider), the `scim_tokens` table, the deprovision-fast path (section 7.3), group-driven role mapping (section 8), and optional enforced SSO (section 4.3). This is the "automatic offboarding" that the security review asks for. |

Because the section 1.1 abstraction is fixed now, the phase-3 work is "fill in the adapter and the tables," not "redesign identity." If the provider path is taken (the recommendation), milestone A is mostly wiring the provider SDK behind the `ssoidentity` interface; the in-house path (section 2.4) is the same milestones with Pulse's own SAML/SCIM code, which is why it is a multi-quarter alternative rather than a drop-in.

---

## 14. Touchpoints to reconcile

These docs interact with SSO/SCIM. This RFC edits only RFC-003 (the auth seam, below). The rest are listed so their owners can reconcile in their own docs; this RFC does not edit them, to avoid concurrent-edit conflicts.

| Doc | What reconciles | Edited here? |
|-----|-----------------|--------------|
| RFC-003 (authn/authz) | the SSO/SCIM authentication + provisioning seam: SSO is an additional auth path feeding the same user/membership model and session issuance; SCIM-driven removal reuses the section 4.3 revocation and section 6.3 cache bust | YES, one surgical edit (section 14.1) |
| RFC-001 (data model) | the new tables `sso_connections`, `scim_tokens`, `org_domains`, `group_role_mappings`, the `user_identities.provider='sso'` value and `sso_connection_id`, the `entitlements.sso_enabled`/`scim_enabled` flags, and the SSO cases in the cross-tenant test suite (section 10) | no (listed) |
| PRD-001 (identity & tenancy) | enterprise identity, domain capture, enforced SSO, and reconciling enforced SSO with the personal-org model (section 4.3); N2 currently lists SSO/SCIM as phase 3 non-goal, which this RFC realizes | no (listed) |
| RFC-009 / PRD-006 (entitlements / billing) | SSO + SCIM as an Enterprise-tier entitlement, the `sso_enabled`/`scim_enabled` flags, and the deactivate-not-delete downgrade behavior (section 11) | no (listed) |
| RFC-015 (audit / compliance) | the SSO/SCIM audit events, the provider sub-processor in vendor management, and the SOC 2 evidence that SSO/SCIM provide (section 12) | no (listed; RFC-015 is the planned compliance RFC) |
| RFC-013 (frontend) | the SSO login button / domain-discovery login UX and the admin SSO-connection + SCIM-token + domain-verification config UI | no (listed) |
| PLANNING.md | phase-3 sequencing of the two milestones (section 13) | no (listed) |

### 14.1 The RFC-003 edit (applied)

A short subsection was added to RFC-003's authentication section noting that enterprise SSO (SAML/OIDC) and SCIM are an additional authentication + provisioning path, defined in this RFC, that feeds the same user/membership model and the same session issuance. The exact text is in RFC-003 section 2.7.

---

## 15. ADRs to capture

| ADR | Decision | From |
|-----|----------|------|
| ADR-0018 Build vs buy enterprise SSO | use a provider (WorkOS) for the SAML/OIDC/SCIM normalization layer, Pulse owns the user/membership mapping; in-house is the documented alternative behind the `ssoidentity` abstraction | section 2 |
| ADR-0019 SSO protocol support set | support SAML 2.0 (SP-initiated and IdP-initiated) and OIDC; no IdP-initiated OIDC; four-role mapping only, no custom roles in v1 | sections 3, 4, 8 |
| ADR-0020 SCIM provisioning model | SCIM 2.0 server per connection, per-connection hashed bearer token, deactivate revokes membership + session immediately, idempotent on external id / verified email | sections 6, 7 |

---

## 16. Decisions summary

| # | Decision | Rejected alternative |
|---|----------|----------------------|
| D1 | Buy the SAML/OIDC/SCIM normalization layer from a provider (WorkOS); Pulse owns the identity mapping behind a `ssoidentity` abstraction | build SAML SP + multi-IdP + SCIM server in-house (security-critical, quirk-heavy, never-finished; the provider sees only auth metadata, accepted as a sub-processor) |
| D2 | SSO is an additional auth path that feeds the same user/membership model and the same session/JWT issuance; the access token stays identity-only and org is resolved per request | a separate SSO session model or org/role baked into the SSO token (would fork the pipeline and break RFC-003 revocation timeliness) |
| D3 | Per-org connections (SAML + OIDC), per-org verified email domains (DNS-TXT), SP-initiated discovery, optional enforced SSO scoped to the enterprise org | a global IdP, unverified domain capture (takeover risk), or enforcing SSO on the human globally (would break the personal-org model) |
| D4 | JIT provisioning is the minimum; SCIM is the authoritative lifecycle and fast-deprovision path (deactivate -> disable membership + revoke session immediately) | JIT-only (leaves deprovisioning manual, the SOC 2 gap) |
| D5 | IdP groups map onto the four roles, default `member`, highest-role-wins; SSO/SCIM clamp at admin and can never create or remove the last owner | letting an IdP group mint an owner or strand an org by removing the last owner |
| D6 | Hardened assertion/token validation: signature must cover the consumed element (anti-XSW), no unsigned assertions, audience/recipient/time/replay/request-binding checks | trusting position-selected assertions, optional signatures, or skipping replay/audience checks |
| D7 | SSO/SCIM are Enterprise-tier entitlements; on downgrade the config is deactivated, not deleted, so users are not locked out | gating off by deleting config (would lock enterprise users out on a billing event) |
