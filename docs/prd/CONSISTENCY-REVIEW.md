# Cross-PRD Consistency Review - Pulse

Status: IN REVIEW (per PLANNING.md)
Reviewer: Principal PM / editor
Scope: master `PRD.md` plus PRD-001 through PRD-007, checked against `PLANNING.md`.

This review checks the seams between the sub-PRDs for contradictions, gaps, and broken cross-references. Mechanical errors were fixed in place. Substantive conflicts are flagged for a lead decision. Each item is tagged [FIXED], [CONFLICT], or [GAP].

Summary counts: 6 FIXED, 3 CONFLICT, 4 GAP. The rest of the seams are a clean bill (listed at the end).

---

## Seam 1 - Seats model (PRD-001 vs PRD-006 vs master 3 / 11)

Clean. All three agree:

- Pending invitations reserve a seat (master decision 16.1, master 3, PRD-001 I6 / section 5.1 / 6.4, PRD-006 4.2 and open decision 11.1).
- Seat count = accepted members + reserved pending invites (master 11 seats meter, PRD-001 5.1, PRD-006 4.2).
- Seat caps 1 / 3 / 10 / 25 match across master 11, PRD-001 5.2, PRD-006 section 3.
- Canonical ownership is clean: PRD-001 says it only consumes the entitlement and PRD-006 owns the numbers and the meter. No double-source-of-truth.

No issues.

---

## Seam 2 - RBAC permission matrix (master 4, PRD-001 7, and every role gate)

Mostly clean. The matrices line up across master 4, PRD-001 7.2, PRD-003 9, PRD-004 10, PRD-005 4, PRD-006 9:

- Manual close incident: owner/admin only everywhere (master 4, PRD-001 7.2, PRD-002 6.4, PRD-005 4.3 "admin", master 16.8). Consistent.
- Manage billing: owner only; view billing: owner+admin. Consistent across master 4, PRD-001 7.2, PRD-005 4.7, PRD-006 9.
- Send test message / create channels: member+ everywhere (master 4, PRD-003 6 and 9, PRD-005 4.2). Consistent.
- Create/edit/publish status pages and post incident update: member+ (master 4, PRD-004 10, PRD-005 4.4). Consistent.
- Configure custom domain: owner/admin only (master 4, PRD-004 9.1 and 10). Consistent.
- API keys never owner-equivalent: stated consistently in master 5 / 16.5, PRD-001 7.3 and appendix A, PRD-005 1.3 / 2.1 / 4.7, PRD-006 9. Consistent.

[GAP] G-RBAC-1: "Create a new organization" is in PRD-001 7.2 (marked +, Y for all roles) but it is not in the master 4 matrix and there is no API for it in PRD-005. This is fine for a UI-only self-service action, but the master matrix should probably acknowledge it so PRD-001's added row has a clear parent. Low severity. Recommendation: add a one-line note to master 4 that org creation is open to any signed-in user (already the signup behavior) so the (+) row in PRD-001 traces to the master.

No conflicts on who-can-do-what.

---

## Seam 3 - Plan tier numbers (PRD-006 vs master 11 vs PRD-007 vs PRD-002)

Mostly clean and now fully consistent after one fix.

Verified identical across docs:
- Free anchor: 2 monitors / 120-min interval / 1 region (master 11, PRD-006 section 3, PRD-007 section 3, PRD-002 2.3 effective floor 7200s).
- Business anchor: 500 monitors / 1-min / up to 6 regions incl. premium (master 11, PRD-006 section 3, PRD-007 section 3).
- Monitors 2 / 25 / 100 / 500 (master 11, PRD-006 3).
- Regions per monitor 1 / 2 / 4 / 6 (master 11, PRD-006 3, PRD-007 section 3).
- Seats 1 / 3 / 10 / 25 (master 11, PRD-001 5.2, PRD-006 3).
- Retention 7 / 30 / 90 / 180 days, matching master section 12 (master 11 and 12, PRD-002 8.1, PRD-006 3).
- Status pages 1 / 1 / 3 / 10 (master 11, PRD-004 1.1 and 2.3, PRD-006 3).
- API rate 30 / 120 / 300 / 600 req/min (PRD-005 2.4, PRD-006 3). Master 9/11 only names the shape (low/standard/higher/highest); PRD-005 and PRD-006 agree on the concrete numbers.

[FIXED] F-TIER-1: PRD-006 section 10.2 AC1 heading read "6th monitor on Free is blocked" while the body and the locked anchor both say the cap is 2 and the 3rd is the one over the cap. Master 11 and PRD-006 5.1 both say "the 3rd monitor on Free is blocked." Fixed the heading to "3rd monitor on Free is blocked" and removed the confusing "the 6th generalizes" parenthetical, replacing it with "the 3rd is the one over the cap."

No remaining tier-number conflicts.

---

## Seam 4 - Multi-region verdict flow (PRD-007 vs PRD-002 vs PRD-004 vs PRD-003)

Clean. The four docs agree:

- Verdict reduces per-region results to one monitor-level healthy/unhealthy input, then the unchanged state machine runs (master 6.4 / 6.7, PRD-002 4.9, PRD-007 section 5).
- down_policy any / quorum / all with quorum default everywhere (master 6.7, PRD-002 2.2 / 4.9, PRD-007 section 5, PRD-005 8).
- Quorum denominator = healthy reporting regions R, excluding heartbeat-unhealthy regions D. PRD-002 4.9 summarizes "majority of healthy-region results"; PRD-007 section 5 gives the precise |U| > |R|/2 definition. Consistent (PRD-002 correctly defers the full definition to PRD-007).
- Coverage-degraded is an orthogonal signal, not a 5th status, everywhere (master decision 16.7, PRD-002 5.1, PRD-004 section 7, PRD-007 section 6).
- Status pages hide per-region and coverage-degraded detail in v1 (PRD-004 7 / 12.2 AC9 / 13, PRD-007 9 and decision 13.3). Consistent.
- Notifications fire on the monitor-level verdict, never per region; region detail only as an additive human-readable line (PRD-003 7, PRD-002 4.9, master 6.4). Consistent.

No issues.

---

## Seam 5 - Entitlement enforcement (PRD-006 vs PRD-002 vs PRD-005 vs PRD-007)

Clean. The two-point enforcement model is stated identically:

- API on write + scheduler on dispatch, neither trusts the other (master 11, PRD-002 2.4, PRD-006 5.1 / 5.2, PRD-007 section 3 enforcement).
- Downgrades cannot be bypassed; scheduler clamps interval and region on every dispatch (PRD-006 5.2, PRD-002 2.4, PRD-007 section 3).
- Cached entitlements in Redis, invalidate on plan change (master 11, PRD-002 2.4, PRD-006 5.3, PRD-005 2.4).
- API has two gates (role + entitlement), Free read-only (PRD-005 9.1 / 9.2, PRD-006 5.1). Consistent.
- Region entitlement is owned by PRD-006 numbers and PRD-007 mechanics; both say so. Consistent.

No issues.

---

## Seam 6 - Notification payloads and contract (PRD-003 vs master appendix B vs PRD-002)

Clean. The locked payloads in PRD-003 4.3 are byte-for-byte the same as master appendix B (generic-webhook down/recovery JSON, Slack text, Discord content, SMTP subject/body). The one-down / one-up, no-re-notify-in-v1 contract is identical across master 6.4, PRD-002 4.7, PRD-003 4.1.

One thing to keep an eye on (not a defect): PRD-003 7 and its open decision 11.1 add an optional human-readable `Regions:` line to Slack/Discord/email bodies. This is correctly scoped as additive and explicitly does NOT touch the locked generic-webhook envelope, so it does not break the appendix B contract. Flagged here only so a reviewer confirms the additive line is acceptable; no fix needed.

No issues.

---

## Seam 7 - Cross-reference integrity

[FIXED] F-REF-1 through F-REF-5: PRD-001 referenced a non-existent "PRD-009" in five places. Per PLANNING.md, the public API / API-keys product domain is PRD-005 (entitlements enforcement is the RFC layer RFC-009, and billing/entitlements product scope is PRD-006). All five were about API keys / public API, so they map to PRD-005. Fixed:
- Line 7 header "Depends on: ... PRD-009 Public API (key auth)" -> PRD-005.
- Line 347 matrix subheading "API keys (detail in PRD-009)" -> PRD-005.
- Line 365 "PRD-009 owns key mechanics" -> PRD-005.
- Line 420 "owned by PRD-009 and PRD-006" -> PRD-005 and PRD-006.
- Line 562 dependencies row "PRD-009 Public API ... PRD-009 owns key creation..." -> PRD-005.

Verified clean after the fix:
- No other PRD-008 / PRD-009 (or any PRD-NNN above 007) references remain anywhere in `prd/`, `PRD.md`, or `PLANNING.md`.
- PRD-006's "RFC-009 Entitlements Enforcement" reference is correct (matches PLANNING.md line 41). RFC references in PRD-003 (RFC-002, RFC-007) and PRD-007 (RFC-004, RFC-005, RFC-008, RFC-000) all match PLANNING.md.
- All "master N" section references resolve (master has sections 1-16).
- All "master 16.N" decision references are within 16.1-16.8 (master section 16 has exactly 8 decisions).
- All "master 10 screen N" references are within 1-14 (master section 10 lists 14 screens); PRD-007's "master 10.5 / 10.6" map to screens 5 and 6. All valid.

No remaining broken references.

---

## Seam 8 - Open decisions (conflicting defaults across docs)

No two docs recommend conflicting defaults for the same question. Cross-checked:
- Pending invites reserve a seat: yes, everywhere.
- Re-notify while down: off, everywhere (master 16-area / 6.4, PRD-002 11.2, PRD-003 11.2).
- Per-region public detail: hidden in v1, agreed by PRD-004 13.2 and PRD-007 13.3 (jointly owned, same recommendation).
- Quorum denominator: majority of healthy reporting regions, only decided in PRD-007 (5 and 13.1); PRD-002 defers to it. No conflict.
- Email-must-match-on-accept, org-deletion grace: PRD-001 D1/D3, consistent with master 16.2.

[CONFLICT] C-DECISION-1 (low severity, ownership): Trial length. PRD-006 8.4 and decision 11.4 recommend a 14-day Team trial, no card. The master section 16 open-decisions list does not include a trial decision at all, and master 14 lists conversion triggers without a trial. This is not a contradiction of a master default (there is none), but a sub-PRD is introducing a customer-facing default (trial existence + length) that the master never mentions. Options: (a) accept PRD-006 as the owner of the trial decision and add a one-line pointer in master 11/16; (b) leave trial entirely out of v1 since GA self-serve billing is Phase 2 and a trial adds Stripe-trial complexity. Recommendation: (a). The trial is a GTM lever that belongs in PRD-006, but the master should acknowledge it exists so the doc set has one obvious home for "is there a trial." Lead to confirm 14 days vs none.

[CONFLICT] C-DECISION-2 (low severity): Org-deletion / account-deletion grace window. PRD-001 D3 recommends a 14-day org-deletion grace, with "shorter (7 days) if legal prefers." Master 13 GDPR only commits to "a short grace window" and "deletions honored within a committed window" without a number. No other doc contradicts 14 days, but the number is invented at the sub-PRD level for an existential, legally-sensitive action. Recommendation: lead/legal pins the master number (14 vs 7) so PRD-001 cites a master value rather than choosing it. Not a doc-vs-doc contradiction today, flagged because the canonical value should live in the master.

---

## Seam 9 - Terminology

[CONFLICT] C-TERM-1 (low severity, intentional drift the lead should ratify): organization / workspace / account. The master 3 explicitly standardizes on "organization" in the API and allows "workspace" in some UI copy and "account" loosely. The sub-PRDs mostly follow "organization/org", but "workspace" still appears in user-facing strings (e.g. PRD-001 example "Dev's workspace"). This is the master's stated policy, so it is consistent by design, not a bug. Recommendation: no change, but confirm the API-vs-UI split is what we want and that no API field is named `workspace`.

[GAP] G-TERM-1: "check" vs "probe." The docs use "check" for the act of testing a customer endpoint and "probe" / "probe fleet" / "probe region" for Pulse's own regional workers (PRD-007, master 6.7). This is a useful distinction but it is never defined in one place. A reader could read "probe" as a synonym for "check." Recommendation: add a one-line glossary note (master 6.7 or PRD-007 section 1) that a "probe region / probe fleet" is our infrastructure and a "check" is one test of a customer endpoint. Low severity.

Otherwise terminology is consistent: "membership" (the link) vs "member" (the person) is used cleanly; "incident", "monitor", "channel", "verdict", "coverage-degraded" are used the same way everywhere.

---

## Additional gaps found

[GAP] G-MISC-1: PLANNING.md (line 27) lists the consistency review as a deliverable ("Cross-PRD consistency review - IN REVIEW") but the index of sub-PRDs (lines 19-26) does not mention this file's path. Minor: once this review is accepted, PLANNING.md should point at `prd/CONSISTENCY-REVIEW.md`. Not fixed here because it is an editorial choice for the lead.

[GAP] G-MISC-2: Uptime formula is an open decision in PRD-002 (11.3, incident-duration based) and PRD-004 consumes uptime (3.3, 12.2 AC2) without restating which formula. PRD-002 flags it as "open until status-page work confirms," and PRD-004 is the status-page work but does not close it. Recommendation: PRD-004 should explicitly adopt PRD-002 11.3 (incident-duration based) so the open decision is closed in one place rather than left dangling between the two docs.

---

## Clean bill (seams verified consistent, no action needed)

- Seam 1 Seats model: fully consistent.
- Seam 4 Multi-region verdict flow (quorum, coverage-degraded, per-region public visibility): fully consistent.
- Seam 5 Entitlement enforcement (api-on-write + scheduler-on-dispatch, no-bypass, caching): fully consistent.
- Seam 6 Notification payloads and one-down/one-up contract: byte-for-byte consistent with master appendix B.
- Plan tier numbers (after F-TIER-1): all anchors and middle tiers consistent across master, PRD-002, PRD-006, PRD-007.
- Cross-reference integrity (after F-REF-1..5): no broken PRD/RFC/master/section references remain.
- RBAC who-can-do-what: consistent across all six docs that state a role gate.
