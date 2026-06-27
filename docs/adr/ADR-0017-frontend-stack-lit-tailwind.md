# ADR-0017: Frontend stack (Lit light DOM, Tailwind, daisyUI, uPlot, TanStack Table)

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-013 sections 2.1, 2.5, 5, and 6, RFC-000 section 14, ADR-0005

## Context
The SaaS SPA is far larger than the v1 four-view app: social login, org switcher and org-scoped routing, role- and entitlement-aware UI, many feature views, charts, tables, and a public status page. The existing web/ foundation is deliberately zero-dependency (hand-rolled History router, custom SVG, hand-rolled tokens.css, shadow-DOM components). Hand-rolling and maintaining a palette, spacing scale, dark theme, charts, and every control's CSS at this app size is slow and drifts.

## Options considered
- Lit 3 in light DOM plus Tailwind v4, daisyUI v5, uPlot, and headless TanStack Table - a themed component layer and utilities with small footprint, charts and table behavior without a heavy framework, building on the existing Lit foundation.
- Stay fully hand-rolled (keep tokens.css, no Tailwind/daisyUI, hand-built charts and tables) - zero added deps, but slow to extend and drift-prone at the now-larger app size.
- Shadow DOM plus a shared compiled Tailwind/daisyUI sheet adopted per component - keeps encapsulation but adds a base class and a build step in every component for encapsulation this first-party app does not need.

## Decision
Build the SPA with Lit 3 rendering in light DOM (createRenderRoot returns this), styled with Tailwind CSS v4 plus daisyUI v5, charts with uPlot, and tables with daisyUI markup plus headless TanStack Table (@tanstack/table-core). Keep and extend the existing hand-rolled History router and the per-view state plus a small @lit/context for session/org/entitlements; no Redux, no router library, no JS component library (daisyUI ships CSS only). Light DOM is required so global Tailwind/daisyUI classes reach component markup.

## Consequences
The team gets a consistent themed component layer and utilities, charts, and table behavior with a small runtime, evolving the existing Lit foundation rather than starting over. The cost is added dependencies and a migration of the current foundation: tokens.css and per-component shadow-DOM CSS are replaced by one Tailwind/daisyUI stylesheet, components move to light DOM (form-field renders its control directly instead of using a slot, confirm-dialog's focus trap queries this instead of shadowRoot), and the build adds Tailwind plus daisyUI. This reverses the foundation's earlier no-component-library, encapsulated-shadow-DOM choice; encapsulation buys little in a single first-party SPA. Route-level code-splitting and per-chunk size budgets in CI keep the larger app from bloating. Revisit if the app ever ships reusable widgets where style encapsulation matters again.
