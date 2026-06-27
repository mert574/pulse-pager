# ADR-0018: ICU MessageFormat plus the localizable-string API convention

Status: Accepted
Date: 2026-06-22
Deciders: Architecture
Related: RFC-014 sections 2, 3, 4, 10.1, RFC-012 section 2.4

## Context
Pulse returns user-facing strings from many surfaces: the error envelope, per-field validation, the channel catalog, notification bodies, status pages. To localize any of them later without re-shaping the API, every user-facing string needs one stable shape, and the interpolation/plural/select rules need a standard that works on both the Go server and the JS frontend so one source pattern compiles in both catalogs.

## Options considered
- One localizable-string shape `{code, params?, message}` everywhere, with ICU MessageFormat as the interpolation standard (chosen). `code` is the contract a client localizes against its own catalog, `params` carries machine-shaped values (numbers, ids, enum tokens, RFC3339 timestamps) so the client can interpolate and re-localize embedded tokens, `message` is the already-interpolated English fallback. ICU handles plurals, gender/select, and nesting and has mature implementations on both stacks.
- Go `text/template` for interpolation - rejected. Server-only, no plural or gender model, so a translator cannot express "1 region / 2 regions" without code, and the FE cannot share the pattern.
- printf-style (`%s`, `%d`) - rejected. Positional args break under translation, no plural/select, no named args.

## Decision
Every user-facing string the API returns uses the shape `{code, params?, message}`: `code` is the contract (a stable dotted key, never a free-text branch target), `params` is present only when the message has variables and holds machine values, `message` is the interpolated English source and the fallback for any client without a catalog for `code`. A client with a catalog renders from `code` + `params` and ignores `message`; a client without one shows `message`. Both always work. Interpolation, pluralization, and select/gender use ICU MessageFormat: the English source for a code is an ICU pattern and `params` are the ICU arguments, so one source pattern compiles in both the server catalog and the FE catalog with identical semantics. This convention amends the RFC-012 error envelope (the top-level message and each per-field validation message become localizable, additive and backward-safe), and the channel catalog already returns this shape.

## Consequences
The API surface is localization-ready from day one: adding a locale is a catalog plus a translation, not a handler, endpoint, or spec change. Every client always renders something because `message` is always present. The cost is discipline: every new user-facing string must go through a code in the namespace (section 4) rather than a bare string, and a CI check asserts every emitted code resolves in both catalogs or falls back to `en`. `params` values must stay machine-shaped (a plan token, not "Starter plan"; an RFC3339 UTC timestamp) so the client can re-localize them. v1 resolves everything to `en`; a new locale just starts winning negotiation.
