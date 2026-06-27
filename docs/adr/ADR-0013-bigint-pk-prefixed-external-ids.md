# ADR-0013: BIGINT primary keys with prefixed external ids

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-001 section 3, RFC-012 section 3, ADR-0007

## Context
Internal primary keys are written at high volume, most of all on the check_results firehose (~10k inserts/sec). External clients need stable, type-namespaced ids in URLs and webhook payloads, and the PRD payload examples are literal forms like mon_123 and inc_456. We separate the storage surrogate from the public id so the public id is never coupled to the storage representation.

## Options considered
- BIGINT GENERATED ALWAYS AS IDENTITY internally, with a prefixed external id computed at the API serialization boundary - 8-byte keys, no write amplification on the firehose, public id namespaced by type.
- UUID or ULID primary keys - globally unique without a sequence, but 16 bytes per key plus index write-amplification on the firehose, which the high insert rate makes expensive.

## Decision
Internal primary keys are BIGINT identity columns. External-facing ids are a prefixed string (mon_, inc_, chn_, org_, usr_, key_, wh_, ...) produced by a codec at the API boundary and never stored. For v1 the encoding is the decimal string of the bigint, so external("mon", 123) is "mon_123", which matches the PRD examples verbatim. The codec is swappable to a keyed encoding (sqids/hashids or base62 of an encrypted bigint) with zero schema change because nothing is stored; only the serialization boundary would change. Internal-only tables (memberships, seats, joins, rollups, audit, idempotency) carry no external id.

## Consequences
The firehose stays cheap (8-byte keys, no UUID/ULID write amplification) and external ids stay stable for the life of a row, so bookmarks, CI configs, and webhook payloads keep working. The decimal id leaks aggregate counts and ordering, which we accept for v1 because access enumeration is already blocked by authz plus RLS (org A asking for org B's mon_2 gets a 404, never the row) and counts are low-value to a competitor. The codec is not a security boundary. If product later wants opacity or compactness, swap to a keyed or base62 encoding at the boundary with no migration. Note: the task brief described this ADR as base62-at-the-boundary; RFC-012 section 3 chose decimal for v1 and deferred base62, and this record follows the RFC.

## Revisit
Switch the codec to a keyed/base62 encoding when product wants id opacity or shorter strings; the bigint PK, foreign keys, RLS, and event keys are untouched by that change.
