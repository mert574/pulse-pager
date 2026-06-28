# RFC-015 - Compliance and Data Governance

Status: DRAFT
Author: Principal Architecture
Audience: every service author who stores or moves customer data, the identity/api owners (RFC-003, PRD-001), the SRE/observability owners (RFC-010), the infra owners (RFC-011), the billing owner (PRD-006), and the legal/compliance counsel who own the steps engineering cannot
Parent: `docs/rfc/RFC-000-architecture-overview.md`
Depends on: RFC-001 (PII columns, encryption, retention, partitioning, RLS), RFC-003 (identity, sessions, cookies, key auth), RFC-008 (control-plane vs regional-data-plane split, cross-region flows), RFC-010 (logging hygiene, audit, breach detection, incident response), RFC-011 (KMS, encryption at rest, backups, network), RFC-012 (error envelope and API conventions)
Product source of truth: PRD master section 13, PRD-001 (deletion/export, the 14-day grace), PRD-006 (Paddle holds card data)

House style: no em-dashes. Tables and concrete cites over prose. Where a control is legal, not technical, this RFC says so and stops there.

---

## 1. Overview, scope, and the honest framing

### 1.1 What this RFC fixes

This RFC is the detailed compliance and data-governance design for Pulse. It deepens PRD master section 13 into concrete technical controls: a data inventory and classification, the GDPR data-subject-rights mechanics (export, erasure, rectification, restriction), privacy by design, data residency and international transfers, audit logging, breach-notification readiness, the SOC 2 / ISO 27001 controls matrix, cookies, retention enforcement, and the PCI boundary. It enumerates the touchpoints in other RFCs and PRDs so a later reconciliation pass can make those edits.

The goal is GDPR compliance and SOC 2 Type II / ISO 27001 audit-readiness, with the technical controls designed in from the start rather than retrofitted.

### 1.2 The honest framing (what engineering owns and what it does not)

Engineering owns the technical controls and the design-for-certification. Engineering does NOT own, and this RFC does NOT claim to satisfy, the steps that require legal counsel and an external audit firm. Stating this plainly so no reader mistakes a designed control for a granted certification.

| Owned by engineering (this RFC) | Owned outside engineering (legal / auditor / vendor) |
|---|---|
| Encryption at rest and in transit, key management | The data processing agreement (DPA) wording |
| Export and erasure mechanics, the deletion cascade | The public privacy policy and cookie policy text |
| RLS tenant isolation, RBAC least privilege | The public subprocessor list and notice-of-change process |
| No-PII-in-logs filters, audit immutability | Each subprocessor's signed DPA |
| Retention jobs and partition drop | The SOC 2 Type II audit itself and the auditor's opinion |
| Breach detection signals and the incident runbook | ISO 27001 certification and the ISMS policy set |
| The controls matrix and the evidence each control produces | The independent penetration test |
| The PCI boundary (keeping card data in Paddle) | Standard Contractual Clauses (SCCs) execution, transfer-impact assessment |

The rule of this document: a control is "designed" or "built" when engineering can point to it in the design or the code. A control is "external" when a human outside engineering must sign, write, test, or certify it. The matrix in section 12 carries that status per control.

### 1.3 Controller vs processor (the role model)

Pulse plays two GDPR roles at once, and the role decides who owns the lawful basis and the data-subject relationship.

| Role | Data | Why |
|---|---|---|
| **Controller** | Account and identity data: user email, name, avatar, the org graph, memberships, invitations, billing identity, audit logs, status-page subscriber emails (phased) | Pulse decides why and how this data is processed (running the account, billing, security). The data subject is the Pulse user. |
| **Processor** | Customer-monitored data: monitor config and target URLs, check results, incidents, latencies, the last-failure snapshot | The customer org decides what to monitor and why. Pulse processes it on the customer's instruction under the DPA. The customer is the controller for this data. |

Consequence: a DPA between Pulse and each paying org is required (legal), and a public subprocessor list is required (legal), because Pulse-as-processor uses subprocessors (cloud, Paddle, email, OAuth, error tooling) to process customer data. Section 5 lists the subprocessor categories.

A subtlety worth stating: a monitored target URL can itself contain personal data (for example a URL with a customer's own user id in the path). Pulse treats target URLs as customer-controlled operational data (PRD master 13: "what it returns is the customer's responsibility"), processed under the DPA, not as Pulse-controller PII.

---

## 2. Data inventory and classification

Every data category Pulse holds, with its class, store, encryption at rest, retention, who can read it, whether it is PII, and the controller/processor role. Stores and column names cite RFC-001 unless noted.

| # | Data category | Class | Store | Encryption at rest | Retention | Who can access | PII | Role |
|---|---|---|---|---|---|---|---|---|
| 1 | User email / name / avatar (`users.primary_email`, `display_name`, `avatar_url`) | PII | Postgres `users` | Managed disk/db encryption (RDS/Cloud SQL, RFC-011 6) | Life of account; hard-deleted at grace end | The user (self-scoped); owner/admin see org members' emails | Yes | Controller |
| 2 | Provider identity (`user_identities.provider_email`, `provider_user_id`) | PII | Postgres `user_identities` | Managed disk/db encryption | Life of account | The user (self-scoped) | Yes | Controller |
| 3 | Invitation email (`invitations.invited_email`) | PII | Postgres `invitations` | Managed disk/db encryption | Until accepted/revoked/expired; org-cascade on delete | Owner/admin of the inviting org | Yes | Controller |
| 4 | Status-page subscriber email (phased, PRD-004) | PII | Postgres (subscriber table, phased; not in RFC-001 v1) | Managed disk/db encryption | Until unsubscribe or page/org delete | Owner/admin of the org | Yes | Controller |
| 5 | Channel credentials (`channels.config` secret fields: Slack/Discord/webhook URL, SMTP password) | Secret | Postgres `channels` (JSONB, per-value encrypted) | App-level AES-256-GCM per value via `internal/crypto` (RFC-001 1.3, 4.3) | Life of channel; org-cascade on delete | Never returned over API; redacted on read; owner/admin/member manage | No | Processor |
| 6 | Monitor secret headers (`monitor_headers.value` when `is_secret`) | Secret | Postgres `monitor_headers` | App-level AES-256-GCM per value (RFC-001 4.3) | Life of monitor; org-cascade on delete | Write-only over API; never returned; never logged | No | Processor |
| 7 | Outbound-webhook signing secret (`outbound_webhooks.signing_secret`) | Secret | Postgres `outbound_webhooks` | App-level AES-256-GCM (RFC-001 4.6) | Life of webhook; org-cascade on delete | Never returned after creation | No | Processor |
| 8 | API key material (`api_keys.key_hash`) | Secret | Postgres `api_keys` | SHA-256 hash only, not reversible (RFC-001 4.6, RFC-003 5.2) | Until revoked; org-cascade on delete | Secret shown once at creation; only prefix shown after | No | Controller |
| 9 | Refresh tokens (`refresh_tokens.token_hash`) | Secret | Postgres `refresh_tokens` | SHA-256 hash only (RFC-003 4.1) | ~30 days sliding; revoked on logout/delete | Server only; never returned | No | Controller |
| 10 | JWT signing key (RS256 private key) | Secret | KMS-backed k8s secret, mounted only into api (RFC-003 3.4, RFC-011 8) | Cloud KMS, never in git or image | Rotated with `kid` overlap (RFC-003 3.4) | api process only | No | Controller |
| 11 | Monitor config incl. target URL (`monitors.url`, headers, body, assertions) | Operational (customer-controlled) | Postgres `monitors`, `monitor_headers` | Managed disk/db encryption; secret headers also AES-256-GCM | Life of monitor; org-cascade on delete | Org-scoped by RLS; friendly name only on public status pages | Possible (customer's choice) | Processor |
| 12 | Check results / latencies (`check_results`) | Operational | Postgres `check_results` (partitioned by month) | Managed disk/db encryption | Per plan: 7/30/90/180 days (RFC-001 6.2) | Org-scoped by RLS | No | Processor |
| 13 | Rollups (`check_rollups`) | Operational | Postgres `check_rollups` | Managed disk/db encryption | Longest uptime window per plan | Org-scoped by RLS | No | Processor |
| 14 | Incidents and the last-failure snapshot (headers + capped body, PRD-002 3.8) | Operational | Postgres `incidents` | Managed disk/db encryption | Life of org (not subject to raw-result cleanup) | Org-scoped by RLS | No (treated as operational) | Processor |
| 15 | Billing identity (`subscriptions.provider`, `provider_customer_id`, `provider_subscription_id`, `provider_price_id`, `plan`, `status`) | Billing | Postgres `subscriptions` | Managed disk/db encryption | Life of org; cascade on delete | Owner views/manages; admin views | No (no card data) | Controller |
| 16 | Card data | Billing | **Paddle only, never Pulse** (PRD-006 8.1) | Paddle (PCI) | Paddle's policy | Paddle only | N/A to Pulse | N/A (Paddle is Merchant of Record) |
| 17 | Audit log (`audit_events`: actor, action, target, `ip_address`, `user_agent`) | Audit | Postgres `audit_events` (append-only) | Managed disk/db encryption | Per plan: Professional 30d, Custom 365d; Free/Hobby none (RFC-001 4.2) | Owner/admin only (RFC-003 7.2) | Yes (contains IP/UA and actor) | Controller |
| 18 | Operational logs | Telemetry | Loki (RFC-010 3.6) | Managed disk encryption | 30d hot, 90d for error level (RFC-010 3.6) | Access-controlled Grafana/Loki; SRE only | No (PII scrubbed, RFC-010 3.4) | Controller |
| 19 | Traces | Telemetry | Tempo (RFC-010 4.5) | Managed disk encryption | Tail-sampled; window cost-driven (RFC-010 open Q) | SRE only | No (only `org_id`/`monitor_id` ids on spans) | Controller |
| 20 | Metrics | Telemetry | Prometheus (RFC-010 2) | Managed disk encryption | Prometheus window (RFC-011) | SRE only | No (no `org_id` at full cardinality, RFC-010 2.2) | Controller |
| 21 | Backups / PITR | Mixed (mirrors all of Postgres) | Managed Postgres backups, cross-region copy (RFC-011 12.2) | Cloud KMS at rest (RFC-001 9, RFC-011 12.2) | Daily, 30-day retention; 7-day PITR | DBA/infra break-glass only | Yes (contains PII) | Both |

Headline: PII is concentrated in a handful of identity tables (`users`, `user_identities`, `invitations`, plus the phased subscriber table) plus the audit log and the backups that mirror them. The secret class is encrypted at the application layer with AES-256-GCM and never leaves the control plane in plaintext. Operational and telemetry data carry no customer PII by design.

---

## 3. GDPR data-subject rights, concretely implemented

### 3.1 Right of access and portability (Articles 15, 20)

Two scopes, because Pulse is controller for account data and processor for org data.

| Scope | Who triggers | What is included | Excluded | Format |
|---|---|---|---|---|
| User-level (personal data export) | Any user, self-scoped, role-independent (RFC-003 7.2) | Profile (name, email, avatar URL), linked providers and their reported emails, orgs + role in each, account timestamps, the user's own audit entries (login, identity link/unlink, logout-all) (PRD-001 10.2) | Secrets, other users' personal data, customer-monitored data the user does not control | Machine-readable (JSON), one archive |
| Org-level (org data export) | Owner of the org (PRD-001 10.2) | Monitors, incidents, members (with emails and roles, because the owner is controller for the org), settings, pending invitations, ownership history | Channel/header/webhook secrets (write-only, never exported), API key material | Machine-readable (JSON) |

Endpoint: served by the api service under `/api/v1` per RFC-012 conventions. The exact path is the api owner's call during reconciliation (section 12), but the contract is: authenticated, role-checked (self for user-level, owner for org-level), returns a machine-readable archive. Time SLA: a target of within 30 days of request (the GDPR Article 12 outer bound), with self-serve export expected to return synchronously or near-real-time for the common case; the 30-day window is the legal commitment, not the engineering target.

### 3.2 Right to erasure (Article 17)

Erasure runs through the existing deletion flows (PRD-001 10.1, 4.5), with a documented backup exception.

Account deletion (user-initiated, PRD-001 10.1):
- Account moves to `deletion-pending`; all sessions are revoked immediately (logout-all, RFC-003 4.3).
- Per org: a non-owner membership ends and frees the seat; a sole owner of a team org with other members is **blocked** until ownership transfers (no silent promotion); a sole-owned personal or empty team org is deleted with the account.
- After the grace window: `users` row and all `user_identities` are hard-deleted (RFC-001 9.4 cascade rules apply to owned orgs).

Org deletion (owner-initiated, PRD-001 4.5), 14-day grace:
- Org moves to `deletion-pending`. Monitoring stops, the org is hidden, status pages go offline, data is recoverable by support/owner during grace.
- At grace end, a hard delete runs. The cascade is concrete (RFC-001 9.4): `ON DELETE CASCADE` from `organizations` removes memberships, monitors, monitor headers, channels, incidents, status pages, api keys, outbound webhooks, audit events, idempotency keys, and rollups. Raw `check_results` is partitioned and not FK-cascaded, so the hard delete also issues a scoped delete of that org's rows in the live partitions, and the rest age out by partition drop.

What is hard-deleted vs anonymized: v1 hard-deletes. RFC-001 9.4 describes a cascade and scoped partition delete, not anonymization. There is no pseudonymized-tombstone retained for identity data in v1. If a future need arises to keep an anonymized incident record for aggregate uptime stats after org deletion, that is a deliberate later decision, not the v1 behavior.

The cascade across the other PII carriers:
- Kafka event data carrying PII: by design the eventing bus carries `org_id`/`monitor_id` integer ids and operational payloads, not user PII (RFC-010 3.4, RFC-002). `check.jobs` carries decrypted secrets but stays in-region and is short-lived (RFC-008 3.4); it is not a durable PII store. So there is no PII to erase from Kafka beyond letting short-retention topics age out.
- Logs and traces: PII is scrubbed at the source (RFC-010 3.4), so operational logs and traces hold no user PII to erase; they age out on their own windows (30/90 days logs, tail-sampled traces).
- Paddle: org deletion cancels the Paddle subscription (PRD-006). Paddle is the controller-appointed processor (Merchant of Record) for billing; erasure of the Paddle customer record is requested from Paddle under its own data-deletion process (legal/ops step, executed via the Paddle API/console).

The backup exception (documented lawful exception with a maximum window):
- Deleted data persists in encrypted Postgres backups until those backups age out. The maximum window is the backup retention, **30 days** (RFC-011 12.2), with a 7-day PITR window inside it.
- This is a recognized GDPR position: backups are kept for integrity and recovery (a legitimate interest / legal obligation around resilience), restoring a backup does not resurrect a deleted account into live service (the `deletion-pending`/`deleted` status and the re-run cascade prevent that), and the data is removed from live systems immediately at grace end. We commit to the 30-day maximum and to not restoring deleted-subject data into production. This wording and its lawful basis must be confirmed by counsel (legal step).

### 3.3 Right to rectification (Article 16)

Profile edit: display name is an editable override; avatar follows the provider; primary email is the verified provider email and changes only by linking/unlinking a provider (PRD-001 2.1, RFC-003 2.1). All self-scoped and role-independent.

### 3.4 Right to restriction and objection (Articles 18, 21)

Restriction maps to suspension: an account or org in `deletion-pending` is locked (no logins act on it, monitoring stops) while data still exists, which satisfies "stop processing but do not delete yet." Objection to processing for an org's monitored data is handled by the customer (controller) disabling or deleting monitors; Pulse-as-processor stops on instruction.

---

## 4. Privacy by design and by default (Article 25)

| Principle | How Pulse does it | Cite |
|---|---|---|
| Data minimization | Only the verified email, name, and avatar are taken from OAuth; sign-in is refused with no user created if no verified email is available. No other profile data is collected. | RFC-003 2.1, 2.3 |
| Encryption at rest (secret class) | App-level AES-256-GCM per value via `internal/crypto`, stored as `base64(nonce\|\|ciphertext\|\|tag)`. The master `PULSE_SECRET_KEY` is sourced from cloud KMS via external-secrets, key-versioned for rotation (active + previous), never in git or an image. | RFC-001 1.3, RFC-011 8.2 |
| Encryption at rest (PII / everything else) | Managed disk/db encryption on Postgres/Redis/Kafka and on backups (cloud KMS). | RFC-011 6, 12.2 |
| Encryption in transit | TLS everywhere: client to api, status pages, and to every infra endpoint (`sslmode=verify-full` Postgres, TLS Redis, TLS Kafka). Mesh mTLS is deferred to the SOC 2/scale trigger. | RFC-011 7.1 |
| Pseudonymization where feasible | Telemetry keys on integer `org_id`/`monitor_id`, not on user PII (RFC-010 3.4, 2.2). Tokens and keys are stored as hashes, not reversible values. | RFC-010, RFC-003 4.1/5.2 |
| Access control | RBAC least privilege (owner/admin/member/viewer, audit log owner/admin only) plus RLS tenant isolation enforced at the data layer with `FORCE ROW LEVEL SECURITY` and a transaction-scoped `app.current_org`. | RFC-001 5, RFC-003 7.2 |
| No PII in logs (default) | Secret-class values are never logged; user emails/names/invitation emails are not logged; `org_id`/`monitor_id` integer ids are allowed. Enforced by CI check and code review; a leaked secret in a log is a security incident. | RFC-010 3.4 |

The no-PII-in-logs rule is the privacy-by-default cornerstone for telemetry and ties directly to RFC-010 section 3.4. Section 12 lists it as a touchpoint so the filters are verified in code.

---

## 5. Data residency and international transfers

### 5.1 Where PII lives vs where checks run

| Data | Location | Carries customer PII | Cite |
|---|---|---|---|
| Account/identity PII, billing identity, audit log | Control plane, single home region (Postgres) | Yes | RFC-008 2, RFC-011 3.1 |
| `check.jobs` (config + a secret header) | Produced into the target region's Kafka, never leaves it, short-lived | No user PII (carries the monitor secret header, which stays in-region) | RFC-008 3.4 |
| `check.results` (mirrored home) | Regional then mirrored to home Postgres | No: `monitor_id`, `region`, `checked_at`, `healthy`, `failure_reason` only | RFC-008 3.4, RFC-002 4.4 |
| `region.health` (mirrored home) | Regional heartbeat to home | No: status/liveness only | RFC-008 3.4 |

Verified against RFC-008's mirror design (section 3.4): only `check.results` and `region.health` cross regions, and neither carries customer PII. The larger job payload that carries the monitor's secret header is produced into the region and never leaves it. Regional data planes hold no durable product state. So multi-region check execution does not move customer PII across borders today: PII stays in the control-plane home region.

### 5.2 EU data residency (phased)

v1/GA does not enforce data residency: results flow home to the central Postgres for aggregation regardless of customer (RFC-008 11). An EU data-residency option for account data is phased (PRD master 15, phase 3, alongside enterprise). The substrate is already in place: the `region` attribution on every result and the control-plane / regional-data-plane split are what a residency variant builds on, by changing what mirrors home rather than re-architecting (RFC-008 11).

### 5.3 International transfers and subprocessors

Where personal data is transferred outside the EEA (for example to a US cloud region or to a US subprocessor), Standard Contractual Clauses (SCCs) plus a transfer-impact assessment are required (legal step). Engineering's part is to keep the data map accurate so counsel knows what crosses where.

Subprocessor categories and the DPA requirement (a signed DPA with each is a legal step):

| Subprocessor category | Example | Processes | DPA required |
|---|---|---|---|
| Cloud provider | AWS or GCP (RFC-011) | All hosting, storage, backups | Yes (legal) |
| Payments | Paddle, Merchant of Record (PRD-006) | Billing identity and card data | Yes (legal) |
| Transactional email | email provider (invitations, alerts, subscriber notices) | Recipient emails and message bodies | Yes (legal) |
| OAuth providers | Google, GitHub (RFC-003) | Sign-in, verified email | Yes (legal) |
| Error / trace tooling | error and trace backend (RFC-010) | Telemetry (PII-scrubbed) | Yes (legal) |

The public subprocessor list and a notice-of-change process for it are required and are legal artifacts, not engineering ones.

---

## 6. Audit logging for compliance

The immutable audit trail is a product/compliance artifact, deliberately separate from operational logs (RFC-010 3.6, 10.4).

| Property | Design | Cite |
|---|---|---|
| Store | Postgres `audit_events`, append-only, not a log aggregator | RFC-001 4.6, RFC-010 3.6 |
| Fields (who/what/when/where) | `actor_type` (human/api_key/system), `actor_id`, `action`, `target`, `changes` (JSONB), `ip_address` (INET), `user_agent`, `created_at` | RFC-001 4.6 |
| Covered actions | member invited/joined/removed/role-changed, ownership transfer, invitation lifecycle, API key created/revoked, billing/plan change, channel created/deleted, monitor deleted, status page published, org settings changed, manual incident close, org deletion requested/done, auth login, logout-all, identity link/unlink | PRD master 13, PRD-001 9 |
| Retention | Per plan: Professional 30 days, Custom 365 days; Free/Hobby none | RFC-001 4.2 |
| Tamper-evidence | Append-only by convention (no update/delete path in the repository), separated from ops logs so it cannot be edited via log tooling. A stronger hash-chain or WORM is an open hardening item (RFC-001 11.2 flags partitioning; tamper-evidence beyond append-only is not yet specified). | RFC-001 4.6, 11.2 |
| Who can read | Owner/admin only (and admin-scoped API keys) | RFC-003 7.2 |

Gap to close for audit-readiness: v1 audit is append-only by repository discipline, not cryptographically tamper-evident. A SOC 2 auditor may accept append-only Postgres with restricted write paths, but a hash-chain or an external WORM sink is the stronger control. Listed as a touchpoint for RFC-010/RFC-001 (section 12).

---

## 7. Breach notification readiness

| Stage | Design | Cite |
|---|---|---|
| Detection | Security monitoring on the signals we have: SSRF block-rate spike as a security signal (a sustained spike is suspicious), plus the general SRE alerting. Anomaly detection on access patterns is a hardening item, not v1. | RFC-010 2.5.3, 7.2 |
| Triage and severity | The SRE severity model (page / ticket / info) and the single on-call rotation with primary + secondary escalation classify a suspected breach as a page. | RFC-010 7.1, 10.2 |
| The 72-hour workflow | On a confirmed personal-data breach, GDPR Article 33 requires notice to the supervisory authority within 72 hours of becoming aware, and Article 34 requires notice to affected data subjects when the risk is high. The runbook step: contain, assess scope using audit logs and traces, classify whether personal data was exposed, then hand to legal/DPO who own the regulator and subject notifications (legal step). | this RFC + RFC-010 10 |
| Incident-response runbook | Tie to the RFC-010 blameless incident review: timeline reconstructed from traces/logs/alerts, impact, contributing causes, tracked follow-ups. A `data-breach` runbook is added to the RFC-010 runbook set (touchpoint). | RFC-010 10.1, 10.3 |
| Breach register | A durable register of incidents that touched personal data (what, when discovered, scope, notifications sent, remediation) is required for accountability (Article 5(2)). Engineering provides the data (audit/incident records); maintaining the register and the regulator relationship is a legal/DPO step. | this RFC |

The 72-hour clock, the regulator notification, and the subject notification wording are legal steps. Engineering's job is fast, accurate detection and scoping so the clock can start informed.

---

## 8. SOC 2 (and ISO 27001) readiness

Build toward SOC 2 Type II from the start (PRD master 13): the controls are designed in. The formal audit is phase 3. Below is the controls matrix mapping the Trust Services Criteria to controls already in the design, plus the external steps.

### 8.1 Trust Services Criteria mapped to designed controls

| TSC | Control in the design | Evidence it produces | Status |
|---|---|---|---|
| Security (CC) | RLS tenant isolation (`FORCE RLS`, transaction-scoped `app.current_org`); cross-tenant test suite blocks release | The T1-T10 isolation suite results in CI; the RLS policies in migrations | Built (design + CI) |
| Security (CC) | RBAC least privilege (four roles; audit log owner/admin only; API keys capped at admin) | The permission matrix; role checks in api | Designed/Built |
| Security (CC) | SSRF always-on, resolution-time validation + network egress deny for RFC1918/link-local/metadata | SSRF block metric; NetworkPolicy manifests | Built |
| Security (CC) | Image supply chain: SBOM, CVE scan blocking HIGH/CRITICAL, cosign signing + admission verify | CI logs, signatures, admission policy | Built |
| Confidentiality | AES-256-GCM for the secret class; KMS-sourced keys; TLS to all infra | Encrypted columns flagged in DDL; KMS config; `sslmode=verify-full` | Built |
| Confidentiality | No-PII-in-logs + secrets-never-logged, CI-enforced | The CI lint; redaction-at-source on Kafka records | Designed/Built |
| Availability | 99.9% control-plane SLA; Multi-AZ Postgres, HA Redis, multi-broker Kafka; SLOs and error budgets | SLO dashboards; uptime reports (RFC-010 5) | Designed |
| Availability | Backups daily/30-day, 7-day PITR, cross-region copy, quarterly restore drills | Backup config; restore-drill records | Designed |
| Processing Integrity | At-least-once idempotent consumers; exactly-once-in-effect alerting dedup; the alerting table test | Idempotency keys; the alerting acceptance test in CI | Built |
| Processing Integrity | Forward-only migrations run as a discrete Job; entitlement enforcement at api and scheduler | Migration history; entitlement checks | Built |
| Privacy | The GDPR control set in sections 3-6 (export, erasure, minimization, consent capture, audit) | Export/erasure endpoints; audit log; this RFC | Designed/partly built |
| Change management | Argo CD pull-based GitOps; reviewed PRs; CI gates; readiness-controlled rollout with rollback | Git history; Argo sync records; CI runs | Built |
| Vendor management | Subprocessor inventory (section 5); DPA per vendor | The subprocessor list; signed DPAs | Designed (list) / External (DPAs) |
| Incident response | SRE severity model, on-call, blameless review, runbooks incl. data-breach | Incident reviews; runbooks | Designed |

### 8.2 External steps (out of engineering scope)

These are required for an actual SOC 2 Type II report or ISO 27001 certificate and engineering does not own them:
- The written policy set (information security policy, access control policy, data classification policy, incident response policy, vendor management policy, business continuity/DR policy). ISO 27001 needs a full ISMS.
- A formal risk assessment and a Statement of Applicability (ISO 27001).
- An independent penetration test.
- The auditor engagement and the Type II observation period (evidence collected over time, typically 6-12 months).
- Signed DPAs with each subprocessor and the executed SCCs.

ISO 27001 overlaps heavily with SOC 2 on the technical controls above; the difference is mostly the management-system documentation and the certification body, which are external.

---

## 9. Cookies and tracking

| Cookie | Purpose | Class | Consent needed | Cite |
|---|---|---|---|---|
| `pulse_at` (access JWT) | Auth, HttpOnly/Secure/SameSite=Lax | Strictly necessary | No | RFC-003 4.4 |
| `pulse_rt` (refresh token) | Auth, HttpOnly/Secure/SameSite=Lax, path-scoped to `/auth` | Strictly necessary | No | RFC-003 4.4 |
| `pulse_csrf` (CSRF token) | CSRF defense, Secure/SameSite=Lax, readable by JS | Strictly necessary | No | RFC-003 4.4 |

v1 sets only these three essential auth cookies. No third-party trackers ship by default. Any analytics added later requires consent (a consent banner and a stored consent record) before any non-essential cookie or tracker loads; no third-party tracker without consent. The cookie policy text itself is a legal artifact.

---

## 10. Data retention enforcement

Per-category retention and the mechanism that enforces each. Retention is not a promise on paper; each row below has a job or a structural mechanism behind it.

| Category | Retention | Enforcement mechanism | Cite |
|---|---|---|---|
| Raw check results | 7/30/90/180 days per plan | Monthly RANGE partitions; `DROP TABLE` once a partition is older than the longest tier (180d); per-org early prune in the rollup job pass | RFC-001 6.2, 6.4 |
| Rollups | Longest uptime window per plan | Produced hourly; raw rows past the org's `retention_days` deleted in the same pass | RFC-001 6.3 |
| Audit log | Professional 30d / Custom 365d / none on Free/Hobby | Per-plan `audit_log_retention_days`; deletion job (partitioning flagged as the next step if volume dominates) | RFC-001 4.2, 11.2 |
| Operational logs | 30d hot, 90d error level | Loki retention policy | RFC-010 3.6 |
| Traces | Tail-sampled; window cost-driven | Tempo retention + tail sampling | RFC-010 4.5 |
| Backups / PITR | 30-day backups, 7-day PITR | Managed Postgres backup config | RFC-011 12.2 |
| Deleted account/org | 14-day grace, then hard delete | `deletion-pending` status + grace-end cascade and scoped partition delete | PRD-001 4.5, RFC-001 9.4 |

The leader-elected runner that owns rollups and partition pre-create/drop (RFC-001 6.4) is the single mechanism that enforces the check-result and rollup retention; the deletion cascade enforces account/org erasure; managed backup config enforces the backup window.

---

## 11. PCI scope

Card data is captured, stored, and charged by Paddle; it is never held by Pulse (PRD-006 8.1: "card capture, PCI scope, charging" are the provider's). Pulse stores only billing identity references (`provider_customer_id`, `provider_subscription_id`, `provider_price_id`, `plan`, `status`, `current_period_end`) and a lightweight invoice reference (id, amount, period, status, hosted PDF URL). Pulse does not compute or store card numbers, CVCs, or full PANs.

The boundary: because no cardholder data ever enters Pulse systems, Pulse stays out of PCI-DSS scope for cardholder-data storage and processing. Payment forms use Paddle-hosted checkout so card data goes browser-to-Paddle, not through Pulse. Confirming the exact PCI SAQ type (typically SAQ A for a fully provider-hosted integration) is a legal/compliance step, but the engineering boundary is clear and must be preserved: never accept, log, or proxy raw card data.

---

## 12. Implementation requirements and touchpoints

Concrete edits/tasks for a later reconciliation. This RFC does NOT edit these docs now (to avoid concurrent-edit conflicts); it lists what each needs.

| Doc | What it needs | Status target |
|---|---|---|
| RFC-001 | A PII-classification annotation on the identity columns; the export queries (user-level and org-level); the scoped-partition-delete query for org erasure; confirm residency-relevant columns (`region` attribution) are sufficient for a future residency variant; decide audit tamper-evidence (hash-chain vs append-only) | designed -> built |
| RFC-003 / PRD-001 | The export and erasure flow endpoints under `/api/v1`; consent capture at signup (terms + privacy acceptance, currently undocumented in RFC-003 and PRD-001); logout-all on deletion already specified | designed -> built |
| RFC-010 | The no-PII logging filters verified in code (not just convention); a `data-breach` runbook; audit immutability hardening (hash-chain or WORM sink); access-pattern anomaly detection as a later signal | designed -> built |
| RFC-011 | Confirm KMS key rotation cadence for `PULSE_SECRET_KEY`; confirm disk/db and backup encryption are on for all managed stores; the worker egress deny-list for RFC1918/link-local/metadata is specified, verify it is applied | built (verify) |
| RFC-007 / PRD-003 | PII minimization in notification bodies and transactional emails (do not embed more PII than needed; subject/body already operational) | designed |
| PRD-004 | Status-page subscriber email handling (phased): a subscriber table, consent on subscribe, an unsubscribe flow, and inclusion in the erasure cascade | external/phased |
| PRD-006 | Keep the Paddle/PCI boundary explicit (no raw card data in Pulse); cancel-subscription on org deletion already specified; Paddle customer erasure step on org deletion | built (verify) |

### 12.1 Compliance controls matrix

| Control | Requirement it satisfies | Where implemented | Status |
|---|---|---|---|
| RLS tenant isolation | Confidentiality, Security (CC), GDPR access control | RFC-001 5; api `withOrg` | Built |
| RBAC least privilege | Security (CC), GDPR Art 25 | RFC-003 7.2; permission matrix | Built |
| AES-256-GCM secret class | Confidentiality, GDPR Art 32 | RFC-001 1.3/4.3; `internal/crypto` | Built |
| KMS-sourced keys + rotation | Confidentiality, Security | RFC-011 8.2 | Built |
| TLS everywhere | Confidentiality, GDPR Art 32 | RFC-011 7.1 | Built |
| Managed disk/db + backup encryption | Confidentiality, Availability | RFC-011 6, 12.2 | Designed |
| No-PII-in-logs + secrets-never-logged | GDPR Art 25, Confidentiality | RFC-010 3.4 | Designed/Built |
| Data minimization at OAuth | GDPR Art 5(1)(c), Art 25 | RFC-003 2.1/2.3 | Built |
| Export (user + org) | GDPR Art 15, 20 | RFC-003/PRD-001 (endpoint TBD) | Designed |
| Erasure cascade + grace | GDPR Art 17 | PRD-001 4.5/10.1; RFC-001 9.4 | Designed/partly built |
| Backup-exception policy | GDPR Art 17 lawful exception | this RFC 3.2; RFC-011 12.2 window | Designed (wording: external) |
| Audit trail (immutable, who/what/when/where) | SOC 2 CC, GDPR accountability | RFC-001 4.6; RFC-010 3.6 | Built (tamper-evidence: hardening) |
| Retention enforcement jobs | GDPR storage limitation, Availability | RFC-001 6.2-6.4 | Built |
| Breach detection + runbook | GDPR Art 33/34, SOC 2 incident response | RFC-010 2.5.3/7/10 | Designed |
| Change management (GitOps + CI gates) | SOC 2 change management | RFC-011 9 | Built |
| SSRF always-on + egress deny | Security (CC) | RFC-011 7.3; ADR-0016 | Built |
| Image signing + CVE block | SOC 2 supply chain | RFC-011 9.1/2.4 | Built |
| PCI boundary (Paddle-only card data) | PCI-DSS scope reduction | PRD-006 8.1 | Built |
| Cookie consent for analytics | ePrivacy, GDPR | RFC-013/legal | External/phased |
| DPA + subprocessor list + SCCs | GDPR Art 28, transfers | legal | External |
| Privacy policy + cookie policy text | GDPR transparency | legal | External |
| Penetration test | SOC 2 / ISO control | external firm | External |
| SOC 2 Type II audit / ISO 27001 cert | Certification | auditor / cert body | External |

---

## 13. Phasing

| Phase | Compliance work |
|---|---|
| Now (built into identity/data features, required as soon as there are EU users) | Export, erasure, the 14-day grace cascade, consent capture, encryption (at rest + in transit), audit log, retention enforcement, no-PII-in-logs. These are GDPR-required from the first EU user and must ship with the identity/data features, not later. |
| Phase 2 (GA) | Paddle/PCI boundary enforced in code; subprocessor inventory kept current as vendors are added; cookie consent if any analytics is introduced. |
| Phase 3 | SOC 2 Type II audit (controls are designed-in now, the audit and observation period are phase 3 alongside enterprise); ISO 27001 if pursued; EU data residency option for account data; status-page subscriber email handling; audit tamper-evidence hardening. |

The stance matches multi-region (RFC-000): the contract and controls are designed in from day one; the certification and the residency variant land later without re-architecture.

---

## 14. ADRs to capture

- **ADR-0018: Controller/processor model and the GDPR control set.** Pulse is controller for account/identity data and processor for customer-monitored data; the control set (export, erasure, minimization, encryption, audit, retention, no-PII-logs) is designed in from day one and required as soon as there are EU users.
- **ADR-0019: Data residency approach.** v1 keeps all account PII in the control-plane home region and moves no customer PII across regions (only `check.results` and `region.health` mirror home, neither carries PII); EU residency for account data is a phased additive variant that changes what mirrors home, not the topology.
- **ADR-0020: Deletion-cascade and backup-exception policy.** Account/org deletion hard-deletes through the `ON DELETE CASCADE` plus scoped partition delete after a 14-day grace; deleted data persists only in encrypted backups for a documented maximum of 30 days and is never restored into production, as the lawful backup exception under GDPR Art 17.

---

## 15. Open questions

1. Audit tamper-evidence: append-only Postgres vs a hash-chain vs an external WORM sink. v1 is append-only by repository discipline; decide the stronger control before the SOC 2 observation period.
2. Export delivery: synchronous archive vs an async job with a signed download link, for large org exports. Contract is machine-readable JSON either way.
3. Consent capture mechanics: where the terms/privacy acceptance is recorded (a `users` column vs an audit event) and whether re-consent is needed on policy changes. Currently undocumented in RFC-003/PRD-001.
4. Paddle customer erasure on org deletion: automate via the Paddle API at grace end vs a manual ops step. Affects how clean the erasure is for billing identity.

---

Status: DRAFT. This RFC designs the technical controls and names every step that is legal or auditor work rather than engineering work. It does not claim any certification.
