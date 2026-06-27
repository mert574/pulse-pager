# ADR-0021: Data controller/processor model and the GDPR technical control set

Status: Accepted
Date: 2026-06-22
Deciders: Architecture
Related: RFC-015 sections 1.3, 2, 3, 4, 12, ADR-0001

## Context
Pulse holds two kinds of personal and tenant data with different GDPR roles, and which role applies decides who owns the lawful basis and the data-subject relationship. The compliance controls (export, erasure, encryption, audit, retention, minimization, no-PII-in-logs) are GDPR-required from the first EU user, so they must be designed into the identity and data features rather than bolted on for an audit.

## Options considered
- Split the data model into a controller role for account/identity data and a processor role for customer-monitored data, with one designed-in control set covering both (chosen). Pulse decides why/how account data is processed (the user is the data subject); the customer org is the controller for what it monitors and Pulse processes it on instruction under a DPA. Concentrates PII in a handful of identity tables and keeps operational and telemetry data PII-free by design.
- Treat all data as one controller relationship - rejected. It misstates the legal role for customer-monitored data (target URLs, results, incidents), where the customer is the controller, and would put the lawful-basis and data-subject obligations on Pulse for data it only processes on instruction.
- Defer the control set to the SOC 2 phase and build features without it - rejected. The GDPR controls are required as soon as there are EU users, not at certification time; retrofitting export/erasure/minimization into shipped identity features is far costlier and risks a compliance gap in between.

## Decision
Pulse is the controller for account/identity data (user email/name/avatar, the org graph, memberships, invitations, billing identity, audit logs, phased status-page subscriber emails) and the processor for customer-monitored data (monitor config and target URLs, check results, incidents, latencies, the last-failure snapshot). A monitored target URL is customer-controlled operational data processed under the DPA, not Pulse-controller PII. The technical control set is designed in from day one and ships with the identity/data features: user-level and org-level export (Articles 15, 20), erasure through the deletion flows (Article 17), data minimization at OAuth (Article 5/25), AES-256-GCM secret encryption plus managed at-rest and TLS in-transit encryption (Article 32), the append-only audit log, retention-enforcement jobs (storage limitation), and no-PII-in-logs with secrets never logged. A DPA per paying org and a public subprocessor list are required (legal steps, named here as out of engineering scope).

## Consequences
Compliance obligations land on the correct party: Pulse-as-controller owns export/erasure/minimization for identity data, and Pulse-as-processor stops on the customer's instruction for monitored data. Because the controls are designed in, the SOC 2 and ISO work later is the audit and observation period, not re-architecture. PII stays concentrated in `users`, `user_identities`, `invitations` (plus the phased subscriber table), the audit log, and the backups that mirror them, which keeps the surface to protect small. The cost is that some steps are explicitly legal/auditor work (the DPA wording, the subprocessor list, the SCCs, the certification) that engineering names but does not own, and the no-PII-in-logs rule must be verified in code (the logging filters), not just by convention.
