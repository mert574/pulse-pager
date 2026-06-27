# ADR-0020: Frontend i18n library is @lit/localize

Status: Accepted
Date: 2026-06-22
Deciders: Architecture
Related: RFC-014 sections 10.3, 10.4, 8, RFC-013 section 9.2, ADR-0017

## Context
The SPA is built on Lit (ADR-0017). It currently uses an interim flat `t(key)` map (RFC-013 section 9.2) as a v1 placeholder, which has no plurals, no ICU, no runtime locale switching, and no translation-tool format. To localize the SPA properly it needs a Lit-native i18n library that integrates with Lit templates, switches locale at runtime without a full reload, and uses a catalog format the standard translation tools speak.

## Options considered
- `@lit/localize` (chosen). First-party for Lit, integrates with Lit templates, supports runtime locale switching, and uses XLIFF catalogs that fit the standard translation workflow. Pairs with `Intl.*` (`DateTimeFormat`, `NumberFormat`, `RelativeTimeFormat`) for date/number formatting, which the SPA already uses. Supersedes the interim `t(key)` map.
- FormatJS / `@formatjs/intl` - rejected. Excellent ICU support and the React-world standard, but not Lit-native, so it needs glue to drive Lit re-renders on locale change and carries a heavier runtime for ICU richness `@lit/localize` plus `Intl.*` already covers for our string set.
- Hand-rolled `t(key)` map (the current interim) - rejected as the final answer. Fine as the v1 placeholder, but no plurals, no ICU, no runtime switching, no translation-tool format. It is exactly what this decision replaces once real locales come.

## Decision
The SPA localizes with `@lit/localize`. FE catalogs are XLIFF, generated from the source strings in the Lit templates and keyed by the same code namespace as the server (ADR-0018, ADR-0019). The interim flat `t(key)` map (RFC-013 section 9.2) is the migration source: it is the placeholder this library replaces. The active locale `@lit/localize` carries also drives the RTL direction hook (set `dir="rtl"` on the document) with logical CSS (Tailwind logical utilities), so an RTL locale is a catalog plus a translation, not a layout rewrite. No RTL locale ships in v1; the direction hook and logical-property discipline are in place.

## Consequences
The SPA gets runtime locale switching, ICU plurals/select, and a standard translation format, evolving the existing Lit foundation rather than adding a non-native runtime. The migration cost is replacing the `t(key)` map call sites with `@lit/localize` and wiring the `{code, params, message}` consumption so the SPA renders client-side from `code` + `params`. The FE XLIFF catalog and the server TOML catalog must stay in step; the shared code namespace and a CI check that every emitted code resolves on both sides (or falls back to `en`) keep them aligned. Adding a FE locale is an XLIFF translation and a shipped bundle, no code or API change.
