# RFC-019 - Transactional Email Delivery and Outbound Mail Consolidation

Status: Accepted, implemented 2026-06-24
Author: Platform Engineering (delivery/notifications)
Audience: notifier authors, api authors who today send mail inline (invite, magic-link), authn owners, anyone touching outbound email or email reputation
Parent: `docs/rfc/RFC-007-notifier.md` (the notifier is the single outbound-mail sender), `docs/rfc/RFC-000-architecture-overview.md` (section 2.5 notifier, section 5 eventing)
Depends on: RFC-002 (a new `email.events` topic and its envelope), RFC-001 (the `invitations.token_hash` becomes nullable; the magic-link Redis record contract), RFC-003 (auth-token minting moves to the sender; `Verify` stays in the api), RFC-014 (per-locale copy), RFC-017 (From display name and branding)
Product source of truth: `docs/prd/PRD-003-notifications.md` (test-send), `docs/prd/PRD-001-*` (invites/seats), RFC-003 (magic-link, invitations)

House style: no em-dashes. Tables and diagrams over prose. Every load-bearing choice states the decision, the reasoning, and the rejected alternatives.

Amends: RFC-007 section 9 (test-send was "api calls the library directly, synchronously, never through the notifier") and the RFC-003 implicit model where the api sends invite and magic-link email inline. Both are replaced by "the notifier is the only thing that sends mail." See section 12 for what changes in those RFCs.

---

## 0. As built (where the code refined this draft)

The implementation kept the shape of this RFC and refined a few details. These notes win where the body below still reads the original way.

| Topic | This draft said | As built | Why |
|-------|-----------------|----------|-----|
| Invite intents | two intents, `InvitationCreated` and `InvitationResent` | one `InvitationRequested` for both | the notifier action is identical (mint, set-hash-on-pending, send); the only difference is which api handler publishes it |
| Locale | per-intent field | one `locale` on the envelope | every email type renders in some locale; no reason to repeat it per payload |
| Invite idempotency | notifier skips if the row already has a token | notifier re-mints every delivery and `SetInvitationToken` overwrites (guarded only by `state = pending`) | "skip if set" loses the email when the set succeeds but the send then fails; re-mint is true at-least-once (late, never lost) and a resend needs no extra "null the token" step |
| Channel test scope | all channel tests route through the notifier | only the Team-email test (the one that uses the platform mailer) goes async through the notifier; slack/webhook/BYO-SMTP test synchronously in the api against their own destination | a non-email test has no platform-mailer dependency and keeps its useful instant pass/fail; this also resolves the synchronous-test-send open question |
| From routing | category->From map in the notifier | same, as a per-message `Mail.From` override plus a default of the alerts address; account mail overrides per message | the Team-email alert path sets no From and so uses the default (alerts); only account mail needs the override |
| Magic-link minting helper (Q1) | new shared package | `internal/maglink` owns the record contract (key, fields, TTL, Mint/Consume); `authn` delegates to it and keeps `Verify` | keeps `authn` from being imported by the notifier for one function |
| Channel-test recipient | (n/a) | the clicker is the signed-in user's email from the session; an API-key actor (no human inbox) gets a 422 | an API-key principal carries no user or email, so there is no one to send a test to |

---

## 1. Overview, scope, owned contracts

### 1.1 The problem

Today outbound mail is sent from two places:

| Sender | Mail | How |
|--------|------|-----|
| api (in the request handler) | invitation, magic-link sign-in | renders with `internal/notify`, then `s.mailer.Send(...)` inline; SMTP error is swallowed (`_ =`) (`internal/api/members.go`, `internal/api/maglink.go`) |
| notifier (bus consumer group) | monitor down/recovery, Team-email channel | consumes `notify.events`, delivers via `internal/notify` |

Two problems follow from the api sending its own mail:

1. There is no single place to put sending policy. Reputation segmentation (per-subdomain From), queueing, per-recipient rate-limiting, and suppression all have to be repeated in every sender or they do not exist.
2. Reliability is uneven: the api's inline sends drop the message on an SMTP error; the notifier path has at-least-once retry.

### 1.2 The decision

All outbound email goes through the notifier. Services do not send mail; they publish a high-level intent and the notifier owns everything below it: rendering, localization, the From address (reputation segmentation), token creation, retries, and the future policy layer (rate-limit, dedup, suppression). The api stops calling `s.mailer.Send` entirely.

| Decision | Reasoning | Rejected alternative |
|----------|-----------|----------------------|
| One sender (the notifier), fed by the bus | one place for reputation/queueing/anti-spam policy; uniform at-least-once delivery; the notifier already owns `internal/notify` and the mailer | keep per-service inline sends (the status quo): policy has nowhere to live and auth mail stays fire-and-forget |
| Events are semantic intents, not rendered email | the publisher says what happened (`MagicLinkRequested`), the notifier decides template, locale, From, recipient; rendering/policy stay in one place | publish rendered subject/body/html: re-spreads rendering and From choice back into every publisher |
| No usable credential ever rides the bus | a magic-link / invite token is a bearer login credential; it must not sit in a Kafka/Redis-Streams log for the retention window | put the token in the event (even encrypted): a working credential persisted in the bus log, wider exposure than today |

### 1.3 Contracts this RFC owns

| Contract | Decision |
|----------|----------|
| `email.events` topic + envelope | a new topic carrying semantic intents, no secrets in the body (section 4) |
| Intent catalog | `MagicLinkRequested`, `InvitationCreated`, `InvitationResent`, `ChannelTestRequested`; monitor alerts stay on `notify.events` unchanged (section 3) |
| Token ownership | the notifier mints the magic-link and invite tokens; tokens never ride the bus (section 5) |
| From routing / reputation | the notifier picks the From by intent category, account subdomain vs alerts subdomain (section 6) |
| Rendering + policy home | `internal/notify` rendering moves to notifier-only use; the api no longer renders or sends (section 7) |

---

## 2. Architecture

```
                       publish intent (no secret)
  api  ───────────────────────────────────────────────►  email.events  ─────►  notifier
  (mints nothing for email,        MagicLinkRequested                          (single sender)
   sends nothing)                  InvitationCreated                            - render (internal/notify)
                                   InvitationResent                             - locale (RFC-014)
                                   ChannelTestRequested                         - From by category (section 6)
                                                                                - mint token if needed (section 5)
  alerting ──► notify.events ─────────────────────────────────────────────►    - policy: retry, rate-limit,
              (monitor down/recovery, unchanged)                                  dedup, suppression (section 8)
                                                                                       │
                                                                                       ▼
                                                                                  platform SMTP (Resend)
```

The notifier gains a second consume path (`email.events`) next to its existing `notify.events` / `webhook.delivery` paths (RFC-007 section 3). Monitor-alert mail is unchanged: it still arrives as `notify.events` and the Team-email channel renders it. The new path handles the transactional intents the api used to send inline.

Reuse vs new service: the notifier is the sender, not a new `mailer` service. It already consumes the bus, holds the platform mailer, and owns `internal/notify`. A separate deployable buys isolation we do not need yet and one more thing to run.

---

## 3. Intent catalog

Each intent is a thing that happened or is wanted, named in the publisher's terms. The body carries only what the notifier needs to render and route, never a token.

| Intent | Published by | Body (non-secret) | Notifier action |
|--------|--------------|-------------------|-----------------|
| `MagicLinkRequested` | api (magic-link start handler) | `email`, `locale` | mint token, store its hash in the shared magic-link Redis record, render, send from the account subdomain |
| `InvitationCreated` | api (CreateInvitation, after the row + seat are committed) | `invitation_id`, `org_id`, `org_name`, `inviter` (display string), `role`, `email`, `locale` | mint token, write its hash to the invitation row (`WithOrg`), render, send from the account subdomain |
| `InvitationResent` | api (ResendInvitation, after expiry bump) | `invitation_id`, `org_id`, `email`, `locale` | mint a fresh token, rewrite the row hash, render, send |
| `ChannelTestRequested` | api (POST /channels/{id}/test) | `channel_id`, `org_id`, `requested_by_email` | render the channel test, send to `requested_by_email` only (not the whole channel), from the alerts subdomain |
| monitor down/recovery | alerting (unchanged) | `notify.events` (RFC-002 / RFC-007) | unchanged; the Team-email channel renders + sends from the alerts subdomain |

Note on `inviter`: a display string like `Jane Doe (jane@acme.com)`, resolved by the api from the actor (it already has it). It is not secret, so it rides in the body and the notifier does not have to look the user up.

Note on `ChannelTestRequested`: this fixes the current behavior where a Team-email test would fan out to every configured member. A test is sent only to the person who clicked, which is also why `requested_by_email` is in the intent.

---

## 4. Eventing: the `email.events` topic

| Aspect | Decision | Reasoning |
|--------|----------|-----------|
| New topic, not `notify.events` | `email.events` | `notify.events` is the monitor-alert verdict stream keyed by `monitor_id`; transactional intents are a different concern with a different key and no incident/dedup-id model |
| Partition key | `org_id` where present, else `email` | keeps a single org's / address's intents ordered; spreads load |
| Envelope | the standard RFC-002 envelope (id, type, occurred-at, trace headers) with a `type` discriminator for the intent | matches the existing bus contract; the notifier switches on `type` |
| Secrets in body | none, ever | tokens are minted by the notifier (section 5); the body is safe to retain |
| Delivery | at-least-once, commit-after-process, same spine as RFC-007 section 3.2 | a crash redelivers; section 5/8 cover idempotency |
| DLQ | unparseable message to `email.events.dlq` via `bus.Poison` | same poison handling as the other topics |

Because the body has no secret, the earlier idea of encrypting the payload with the platform cipher is dropped. There is nothing on the bus worth encrypting.

---

## 5. Token ownership: the notifier mints

Decision: the notifier mints both the magic-link and the invitation token. The token is created at send time, inside the only service that sends, so it never has to be handed across the bus.

| Decision | Reasoning | Rejected alternative |
|----------|-----------|----------------------|
| Notifier mints both tokens | the token is part of "send the link", which is the notifier's job; nothing usable transits the bus; no claim-check store, no payload encryption | api mints and hands the raw token to the notifier by reference (a short-lived Redis id): keeps minting in the api but adds a handoff store and a second moving part |
| Magic-link: write the hash to the shared Redis record | `Verify` (api) already reads `magiclink:<hash>` from the shared Redis; the notifier writes the same record, so verify is unchanged | mint in the api: re-introduces an api sender |
| Invite: write the hash to `invitations.token_hash` under `WithOrg` | the accept path (`GetInvitationByToken`) already looks the invite up by hash; the notifier sets that column as the delivery step | api mints at create and hands a reference: see rejected row above |

### 5.1 Magic-link flow

```
api POST /auth/email/start:
  rate-limit the request (unchanged, api keeps the per-IP / per-email limiter)
  publish MagicLinkRequested{ email, locale }      -- no token
  return 200 "check your email"

notifier on MagicLinkRequested:
  raw = newOpaqueToken()
  SET magiclink:<sha256(raw)> = {email, created_at} EX 15m   -- same record shape api's Verify reads
  url = appBaseURL + "/auth/email/verify?token=" + raw
  render (locale), send from the account subdomain

api GET /auth/email/verify?token=raw  (unchanged):
  GETDEL magiclink:<sha256(raw)>, resolve/create user
```

The minting helper (opaque token + the Redis record contract) is the one piece of `internal/authn` that becomes shared, since both the notifier (mint) and the api (`Verify`) use it. `Verify` and user resolution stay api-only. This is the RFC-003 boundary change called out in section 12.

### 5.2 Invitation flow (async token, the synchronous part stays synchronous)

The parts of `CreateInvitation` that can reject the request or hold state the user sees stay synchronous; only the token and the email are deferred.

```
api POST /orgs/{org}/invitations:
  authz + entitlement (seat cap) check        -- can reject (402/403): MUST be sync
  duplicate check                              -- can 409: MUST be sync
  INSERT invitation row (state=pending, token_hash NULL, seat reserved)   -- sync, shows "invited"
  publish InvitationCreated{ invitation_id, org_id, org_name, inviter, role, email, locale }
  return 201 (the invitation DTO; it never contained the token anyway)

notifier on InvitationCreated:
  WithOrg(org_id):
    load the invitation row; if not still pending (revoked/accepted) -> commit and stop
    raw = newOpaqueToken()
    UPDATE invitations SET token_hash = sha256(raw) WHERE id = invitation_id
  url = appBaseURL + "/invitations/" + raw
  render (locale, inviter), send from the account subdomain
```

`token_hash` becomes nullable (RFC-001 migration): the row exists briefly with no token between the INSERT and the notifier's update. That window is invisible to the invitee, who has no link until the email lands. `ResendInvitation` follows the same shape via `InvitationResent` (mint fresh, rewrite hash, send).

Two-writer note: the api writes the invitation's identity/state (create, accept, revoke); the notifier writes only `token_hash`. The api's accept path reads that column. This split is deliberate (the token is the delivery credential, owned by the sender) and is the reason `token_hash` is nullable. The notifier reads-then-checks-pending before minting so a revoke that landed first wins.

---

## 6. From routing and reputation segmentation

This is the task that started this RFC: do not send everything from one address, so a burst of alert mail cannot drag down login-link deliverability, and neither rides the root domain.

| Category | Intents | From (recommended) |
|----------|---------|--------------------|
| account | `MagicLinkRequested`, `InvitationCreated`, `InvitationResent` | `Pulse Pager <login@account.pulsepager.com>` |
| alerts | monitor down/recovery, Team-email channel, `ChannelTestRequested` | `Pulse Pager <alerts@alerts.pulsepager.com>` |

| Aspect | Decision | Reasoning |
|--------|----------|-----------|
| Route by intent category, in the notifier | the notifier maps each intent to a category and the category to a From | one place owns it; correct no matter which service published the intent |
| Same SMTP creds, different From | Resend signs DKIM by the From domain; only the From address differs per subdomain | no second SMTP account; verify each subdomain in Resend (DNS) |
| Config | `PULSE_SMTP_FROM_ACCOUNT`, `PULSE_SMTP_FROM_ALERTS`, fallback `PULSE_SMTP_FROM` | unset specifics fall back to the single From, so a small deploy runs on one subdomain and the split is opt-in |
| Never the root domain | both subdomains live under `pulsepager.com`, not the apex | isolates transactional reputation from the website / corporate mail |

DKIM/SPF/DMARC verification per subdomain in Resend is the ops half and is not code. The code half is the category->From map plus the two config values.

---

## 7. Rendering and policy live in the notifier

`internal/notify` rendering (the `html/template` shell, `InviteEmail`, `MagicLinkEmail`, alert/test builders) is used only by the notifier after this change. The api drops its dependency on rendering, From, and SMTP. Localization (RFC-014) is applied in the notifier from the intent's `locale`. The notifier is the natural and only home for the policy layer this whole change exists to enable: per-recipient rate-limiting, send dedup, and per-user suppression / unsubscribe.

---

## 8. Idempotency and failure modes

| Case | Behavior |
|------|----------|
| `email.events` redelivered | magic-link: a second mint overwrites the Redis record (still single-use on verify); invite: the row check sees a token already set / not-pending and skips. Acceptable at-least-once: a rare duplicate email beats a missed login link |
| Notifier down | intents hold in the topic (retention) and send on catch-up. Login email is late, not lost. Strictly better than today's swallowed inline send |
| Invite revoked before the notifier runs | the `WithOrg` load sees state != pending and stops before minting/sending |
| Send fails (SMTP) | at-least-once retry on the bus (the notifier's existing spine), unlike today's fire-and-forget |
| Token never on the bus | guaranteed by section 5: the notifier mints; the body has no secret |

---

## 9. Migration / rollout

| Step | Note |
|------|------|
| 1. Add `email.events` + the notifier consume path; keep the api inline sends | new path dark, no behavior change |
| 2. RFC-001 migration: `invitations.token_hash` nullable | forward-only; existing rows keep their hash |
| 3. Move magic-link minting helper + Redis record contract to a shared package | api `Verify` unchanged |
| 4. Switch the api handlers to publish intents and stop calling `s.mailer.Send` | cutover per intent type; can ship invite and magic-link independently |
| 5. Wire `PULSE_SMTP_FROM_ACCOUNT` / `_ALERTS`, verify subdomains in Resend | reputation split goes live |
| 6. Fix `ChannelTestRequested` to send to the clicker only | folds the test-recipient fix in |

Each step is shippable on its own; the From split (step 5) works the moment one sender exists.

---

## 10. What this does NOT change

- Monitor alert delivery (`notify.events`, the Team-email channel, the locked payloads) is untouched.
- The BYO-SMTP channel keeps sending from the user's own server / address (their reputation, not ours).
- The api keeps `Verify` (magic-link) and invite accept/revoke; only minting and sending move.

---

## 11. Open questions

| # | Question | Lean |
|---|----------|------|
| Q1 | Does the magic-link minting helper live in a new shared `internal/mailtoken` package, or does the notifier import a slimmed `internal/authn` piece? | new small shared package; keep `authn` api-only otherwise |
| Q2 | Per-recipient rate-limit / suppression: ship in this RFC or a follow-up once one sender exists? | follow-up; this RFC only makes it possible by consolidating |
| Q3 | Different From display names per category (`Pulse Pager` vs `Pulse Pager Alerts`)? | keep `Pulse Pager` for both unless product wants otherwise (RFC-017) |
| Q4 | Should `InvitationCreated` carry the render fields (org_name, inviter, role) or have the notifier read them under `WithOrg`? | carry them in the body (non-secret); the notifier still reads the row to check still-pending |

---

## 12. Dependencies and amendments

| This RFC depends on | For |
|---------------------|-----|
| RFC-002 | the new `email.events` topic, envelope, partition key, DLQ |
| RFC-001 | `invitations.token_hash` nullable; the magic-link Redis record contract |
| RFC-003 | minting moves to the sender; `Verify` and accept stay in the api |
| RFC-014 | per-locale copy applied in the notifier |
| RFC-017 | From display name / branding |
| `internal/notify`, `internal/bus`, `internal/crypto`, `internal/kv` | rendering, consume loop, shared key, Redis |

Amendments this RFC makes:

| RFC | Was | Now |
|-----|-----|-----|
| RFC-007 section 9 | test-send is synchronous, api calls the library directly, never through the notifier | test-send is a `ChannelTestRequested` intent the notifier delivers, to the clicker only. (The synchronous pass/fail UX is reconsidered in Q open: if the UI needs an instant result, it can poll the delivery outcome; otherwise the test is async like the rest) |
| RFC-003 | api sends invite / magic-link inline and mints their tokens | api publishes intents; the notifier mints and sends; `Verify` stays in the api |

| Depends on this RFC | For |
|---------------------|-----|
| the subdomain reputation work | it is section 6 of this RFC, not a standalone task |
```
