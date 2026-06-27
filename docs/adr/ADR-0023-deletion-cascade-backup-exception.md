# ADR-0023: Account/org deletion cascade plus the backup-retention exception

Status: Accepted
Date: 2026-06-22
Deciders: Architecture
Related: RFC-015 sections 3.2, PRD-001 sections 4.5, 10.1, RFC-001 section 9.4, RFC-011 section 12.2

## Context
GDPR Article 17 (right to erasure) has to run through Pulse's existing deletion flows for accounts and orgs. Erasure must actually remove the data from live systems, but deleted data unavoidably lingers in encrypted backups until they age out, and that backup window needs a documented, lawful position rather than an unstated gap.

## Options considered
- Hard-delete through `ON DELETE CASCADE` plus a scoped partition delete after a 14-day grace, with a documented backup-retention exception capped at 30 days (chosen). Erasure removes the data from live systems immediately at grace end; backups keep it only for integrity and recovery and are never restored into production.
- Anonymize / keep a pseudonymized tombstone instead of hard-deleting - rejected for v1. v1 hard-deletes; there is no retained pseudonymized identity record. Keeping an anonymized incident record for aggregate uptime stats after org deletion would be a deliberate later decision, not v1 behavior.
- No documented backup exception (treat backups as out of scope) - rejected. Backups demonstrably contain deleted PII until they age out; leaving that unstated is a compliance gap. The lawful position (integrity/recovery, capped window, no restore of deleted-subject data) must be written down.

## Decision
Account deletion moves the account to `deletion-pending`, revokes all sessions immediately, handles per-org membership (a sole owner of a team org with other members is blocked until ownership transfers, a sole-owned personal or empty team org is deleted with the account), and after the grace window hard-deletes the `users` row and all `user_identities`. Org deletion has a 14-day grace; at grace end a hard delete runs the RFC-001 9.4 cascade: `ON DELETE CASCADE` from `organizations` removes memberships, monitors, monitor headers, channels, incidents, status pages, api keys, outbound webhooks, audit events, idempotency keys, and rollups. Raw `check_results` is partitioned and not FK-cascaded, so the hard delete also issues a scoped delete of that org's rows in the live partitions and the rest age out by partition drop. The backup exception is documented: deleted data persists only in encrypted Postgres backups for a maximum of 30 days (with a 7-day PITR window inside it), this is a recognized GDPR position (backups kept for integrity and recovery), restoring a backup does not resurrect a deleted account into live service (the deletion status and the re-run cascade prevent that), and we commit to not restoring deleted-subject data into production. The wording and its lawful basis are confirmed by counsel (legal step).

## Consequences
Erasure is concrete and verifiable: the cascade is enumerated, the partitioned table has a scoped delete path, and the other PII carriers (Kafka, logs, traces, Stripe) are accounted for (Kafka carries integer ids not user PII, logs/traces are PII-scrubbed at source, Stripe erasure is requested under its own process). The 30-day backup window is the honest upper bound stated as a lawful exception rather than an unstated gap. The cost is that erasure is not instantaneous in backups (up to 30 days of residual encrypted copy) and the lawful-exception wording depends on a counsel confirmation outside engineering. Stripe customer erasure on org deletion and audit tamper-evidence hardening are tracked as open questions, not v1 blockers.
