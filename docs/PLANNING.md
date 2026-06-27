# Pulse - Planning Index

This is the map of Pulse's planning documents and the order they are produced in. Pulse is a multi-tenant SaaS uptime monitoring platform (distributed Go services, Postgres/Redis/Kafka, Kubernetes, Lit SPA). The planning is layered on purpose: product first (PRDs), then technical design (RFCs), then decisions (ADRs), then the delivery plan.

## Document layers

1. **Master PRD** (`PRD.md`) - product vision, tenancy, RBAC, scope, NFR targets, roadmap. The single product source of truth.
2. **Sub-PRDs** (`prd/PRD-NNN-*.md`) - one per product domain. Each fully specifies that domain's product behavior, derived from the master PRD.
3. **Master RFC** (`rfc/RFC-000-architecture-overview.md`) - the overall technical architecture: services, data flow, the seams between systems, cross-cutting concerns, and the RFC index. The single technical source of truth.
4. **Sub-RFCs** (`rfc/RFC-NNN-*.md`) - one per system/service. Each pins down the technical design that implements the relevant PRDs, with interface contracts, schemas, and failure modes.
5. **ADRs** (`adr/ADR-NNNN-*.md`) - short decision records for load-bearing choices (each: context, options, decision, consequences).
6. **Roadmap** (`ROADMAP.md`) - phased delivery, milestones, workstreams, risk register.

Status legend: TODO / DRAFTING / IN REVIEW / DONE.

## Master PRD
- `PRD.md` - Master product requirements - DONE (v2.1: multi-tenant, RBAC, multi-region, entitlements, API docs)

## Sub-PRDs (`prd/`)
- PRD-001 Identity & Tenancy - users, orgs, memberships, seats, invitations, RBAC, account model - DONE
- PRD-002 Monitoring Engine - monitors, checks, assertions, alerting state machine, incidents, status - DONE
- PRD-003 Notifications - channels (Slack/Discord/webhook/email), delivery, test-send, phased channels - DONE
- PRD-004 Status Pages - public pages, branding, incidents display, custom domain (phased) - DONE
- PRD-005 Public API & Webhooks - REST API, API keys, OpenAPI/Swagger UI, outbound webhooks, docs site - DONE
- PRD-006 Billing & Entitlements - plans, tiers, seats, per-API metering, quota enforcement, Stripe (phased) - DONE
- PRD-007 Multi-Region - region selection, per-region results, quorum down-policy, probe-fleet health, region cost - DONE
- Cross-PRD consistency review - DONE (`prd/CONSISTENCY-REVIEW.md`; 6 mechanical fixes applied, conflicts resolved to defaults)

Lead decisions from the review (locked): 14-day Team trial (PRD-006 canonical), 14-day org-deletion grace, incident-duration-based uptime (PRD-002 canonical). Remaining minor nits (glossary check/probe, create-org row in master matrix, PRD-004 uptime citation) folded into the final doc-polish pass; not blocking.

**Product layer LOCKED.** Architecture layer drafted (RFCs below).

## Master RFC (`rfc/`)
- RFC-000 Architecture Overview - services, topology, data flow, cross-cutting concerns, RFC index - DONE

## Sub-RFCs (`rfc/`)
- RFC-001 Data Model & Multi-Tenancy - Postgres schema, tenant isolation, partitioning, migrations, backups - DONE
- RFC-002 Eventing & Kafka Contracts - topics, partitioning, event schemas, ordering, idempotency, delivery semantics - DONE
- RFC-003 AuthN & AuthZ - OAuth/OIDC, JWT/JWKS, refresh, API keys, RBAC enforcement seam - DONE
- RFC-004 Scheduler - distributed scheduling, leader election, fan-out, region dispatch, entitlement floors - DONE
- RFC-005 Worker / Checker - check execution at scale, SSRF, regional workers, result emission - DONE
- RFC-006 Alerting - state machine at scale, per-monitor ordering, multi-region aggregation, incident lifecycle - DONE
- RFC-007 Notifier - channel delivery, retry/backoff, idempotency, dedup - DONE
- RFC-008 Multi-Region & Probe Fleet - control/data-plane split, region health, failover, cost-aware scheduling - DONE
- RFC-009 Entitlements Enforcement - plan limits model, enforcement points, Redis caching - DONE
- RFC-010 Observability & SRE - metrics/logs/traces, SLIs/SLOs/error budgets, alerting on ourselves, capacity & cost - DONE
- RFC-011 Deployment & Infra - Kubernetes topology, Docker, Helm/IaC, CI/CD, environments, DR - DONE
- RFC-012 API Design & OpenAPI - REST conventions, versioning, pagination, errors, rate limits, the OpenAPI spec - DONE
- RFC-013 Frontend - Lit SPA architecture, routing, state, nginx serving, auth handling - DONE
- RFC-014 Internationalization (i18n/l10n) - localizable-string contract, locale negotiation, ICU + go-i18n + @lit/localize, server-rendered content, data-model fields - DONE
- RFC-015 Compliance & Data Governance - GDPR data-subject rights, data inventory/classification, residency, retention, SOC 2 control matrix - DONE
- RFC-016 Enterprise SSO & SCIM - SAML/OIDC per-org connections, domain routing, JIT + SCIM, role mapping, build-vs-buy (provider) - DONE
- RFC-017 Product Naming & Branding - rename "Pulse" -> "Pulse Pager", usage/wordmark/title/footer/email rules, alert-prefix decision, change-vs-keep scope (display changes, internal identifiers stay), full rename checklist - DONE
- Cross-RFC consistency review - DONE / RESOLVED (`rfc/CONSISTENCY-REVIEW.md`; 5 mechanical fixes applied, the flagged seams C-1/C-2/C-3/G-1/G-2/G-3 resolved by lead decision and applied across the docs)

**Architecture layer LOCKED.** All 13 sub-RFCs written against RFC-000's seams; the consistency review applied the mechanical fixes and the flagged seams (worker result-persistence path, `scheduled_at` on `check.results`, bootstrap endpoint path, check-now job id, derived channel types, audit buffer vs store) are now resolved by lead decision and applied across the docs. RFC-000 and RFC-001..013 are DONE. ADRs and the adversarial principal review are next.

**Pending reconciliation - i18n touchpoints (RFC-014).** RFC-014 defines the API-wide localizable-string contract and applied the RFC-012 error-envelope edit directly. The following docs still need threading to stay consistent (listed in RFC-014 section 13, deliberately deferred to avoid concurrent-edit conflicts): RFC-007/PRD-003 (localized notification bodies, webhook envelope stays English), RFC-013 (replace the interim `t(key)` map with `@lit/localize` + RTL hook), RFC-003/PRD-001 (user/org locale+timezone preference + negotiation), RFC-001 (add `users.locale`/`timezone`, `organizations.default_locale`/`default_timezone`, `invitations.locale`, `status_pages.default_locale`, defaults `en`/`UTC`), PRD-004 (status-page localization).

## ADRs (`adr/`)
Extracted from the RFCs for choices that carry weight. **DONE** - 26 ADRs written, indexed in `adr/README.md`. Each is short and decision-focused (context, options, decision, consequences). The set:

- ADR-0001 Shared-DB row-level tenancy with Postgres RLS - DONE
- ADR-0002 PostgreSQL via pgx + golang-migrate - DONE
- ADR-0003 Kafka client is franz-go - DONE
- ADR-0004 Scheduler singleton via Kubernetes Lease - DONE
- ADR-0005 RS256 JWT + JWKS, identity-only token - DONE
- ADR-0006 Multi-region messaging shape (regional Kafka, results mirrored home) - DONE
- ADR-0007 check_results range partitioning with rollups - DONE
- ADR-0008 Entitlements as a library + Redis cache, not a service - DONE
- ADR-0009 At-least-once delivery with idempotent consumers - DONE
- ADR-0010 NetworkPolicy + TLS-to-infra for service trust, mesh deferred - DONE
- ADR-0011 Worker emits results only; control-plane consumer persists (ratifies CONSISTENCY-REVIEW C-1) - DONE
- ADR-0012 API key auth via SHA-256 + Redis cache - DONE
- ADR-0013 BIGINT primary keys with prefixed external ids - DONE
- ADR-0014 Single leader-elected scheduler heap for v1, with shard trigger - DONE
- ADR-0015 JSON event serialization with a versioned envelope - DONE
- ADR-0016 SSRF always-on, not customer-disableable - DONE
- ADR-0017 Frontend stack (Lit light DOM + Tailwind + daisyUI + uPlot + TanStack Table) - DONE
- ADR-0018 ICU MessageFormat + localizable-string API convention - DONE
- ADR-0019 Server-side i18n library for Go (go-i18n v2) - DONE
- ADR-0020 Frontend i18n library (@lit/localize) - DONE
- ADR-0021 Data controller/processor model + GDPR control set - DONE
- ADR-0022 Data residency approach (account PII in the home region) - DONE
- ADR-0023 Deletion cascade + backup-retention exception - DONE
- ADR-0024 Build-vs-buy enterprise SSO is buy, with in-house escape hatch - DONE
- ADR-0025 Enterprise SSO support set (SAML 2.0 + OIDC) - DONE
- ADR-0026 SCIM 2.0 provisioning (JIT + SCIM, group->role, fast deprovision) - DONE

**Branding note (RFC-017) - alert prefix change to locked appendix B.** The product rename changes the email/SMS alert subject prefix from `[Pulse]` to `[Pulse Pager]`. This is the one branding change that touches the otherwise-locked appendix B payloads. It is intentional and additive: payload structure and field keys are unchanged, only the human-readable prefix text. `docs/PRD.md` appendix B is updated to match. This is ADR-worthy if we later formalize it (context: rename clarity vs subject/SMS length; decision: full name in the prefix because the alert is the "pager" moment and the six extra characters do not change SMS segmenting for normal alerts; consequence: notifier email/SMS strings and their tests update, all other payloads unchanged). Captured here rather than as a standalone ADR for now since RFC-017 already records the full reasoning.

**Pending reconciliation - compliance touchpoints (RFC-015).** Apply as the data/identity features are built: RFC-001 (PII annotation, export/erasure queries, audit tamper-evidence), RFC-003/PRD-001 (export/erasure endpoints + consent capture), RFC-010 (no-PII log filters, breach runbook, audit immutability), RFC-011 (KMS rotation, disk/backup encryption, egress deny-list), RFC-007/PRD-003 (PII minimization), PRD-004 (subscriber email handling), PRD-006 (PCI boundary + Stripe customer erasure).

**Pending reconciliation - SSO touchpoints (RFC-016).** Apply in the enterprise phase: RFC-001 (sso_connections, scim_tokens, org_domains, group_role_mappings, user_identities sso link, entitlements sso/scim flags), PRD-001 (enterprise identity, domain capture, enforced SSO), RFC-009/PRD-006 (Enterprise-tier entitlement), RFC-015 (audit/compliance), RFC-013 (SSO login + admin config UI). RFC-003 already carries the SSO seam.

## Production order (dependencies)
1. Master PRD (revision) -> review.
2. Sub-PRDs (parallel) -> review for consistency against master.
3. Master RFC -> review.
4. Sub-RFCs (parallel) -> review.
5. ADRs captured alongside RFCs.
6. Adversarial principal review across the RFC set (scale, failure modes, isolation, cost, security). Fold findings.
7. Roadmap.
8. Only then resume implementation, reusing the already-built and tested Go packages (domain, crypto, checker, alerting, notify; auth to be extended).

## Reuse note
These Go packages are already implemented and tested and carry over: `internal/domain`, `internal/crypto`, `internal/checker`, `internal/alerting`, `internal/notify`. `internal/store` (SQLite) is replaced by Postgres. `internal/auth` is extended for the new identity model. The Lit frontend foundation under `web/` carries over (served by nginx instead of embedded).

## Implementation status
Build has started (ahead of ADRs/Roadmap, by choice). Done so far: the multi-service foundation (five services + `cmd/schema`, Postgres/Redis/Kafka wiring, RLS tenancy, obs) and Phase-0 slice step 1 (monitors/check_results schema, scheduler -> worker pipeline reusing `internal/checker`). The v1 spine (SQLite store, single-admin auth, embedded web, `cmd/pulse`) has been removed; the leaf packages carry forward as planned. Schema is reset-based (`schema.sql` + `ApplySchema`), not versioned migrations yet. See `README.md`, `IMPLEMENTATION-PLAN-foundation.md`, and `CODE-VS-RFC-GAP.md` (now a historical baseline) for detail.
