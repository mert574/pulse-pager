import { html, nothing, type TemplateResult } from "lit";

// Small presentational helpers shared by the migrated views (Swiss design overhaul).
// Pure templates, no i18n: callers pass already-localized strings. These keep the
// page scaffold (full-bleed broadsheet header band + padded content) and the
// loading/empty/error states consistent across every view without a base class.

// The full-bleed masthead band: a big Archivo title on the left, optional actions on
// the right, a hairline rule underneath. Used on its own by views that put custom
// content (a hero) between the header and the body.
export function pageHeader(title: string, actions?: unknown): TemplateResult {
  return html`<div
    class="flex flex-wrap items-end justify-between gap-5 px-6 lg:px-10 pt-7 lg:pt-[30px] pb-6 lg:pb-[26px] border-b border-line"
  >
    <h1
      class="m-0 font-disp font-black uppercase tracking-[-0.045em] leading-[0.82] text-[40px] lg:text-[52px]"
    >
      ${title}
    </h1>
    ${actions ? html`<div class="flex items-center gap-3">${actions}</div>` : nothing}
  </div>`;
}

// Full page scaffold: header band + padded body, breaking out of the shell's padded
// content column (-mx/-my match app-root's px-6 lg:px-10 / py-7) so the header meets
// the folio edge to edge.
export function pageShell(
  title: string,
  actions: unknown,
  body: unknown,
): TemplateResult {
  return html`<div class="-mx-6 lg:-mx-10 -my-7">
    ${pageHeader(title, actions)}
    <div class="px-6 lg:px-10 py-7">${body}</div>
  </div>`;
}

// Error banner with a retry action. Red outline, reserved for failures.
export function errorBox(
  message: string,
  onRetry: () => void,
  retryLabel: string,
): TemplateResult {
  return html`<div
    role="alert"
    class="flex items-center justify-between gap-3 border border-down px-4 py-3 text-down"
  >
    <span>${message}</span>
    <button class="pulse-btn pulse-btn-ghost pulse-btn-sm" @click=${onRetry}>
      ${retryLabel}
    </button>
  </div>`;
}

// Empty state: a dashed-hairline panel with an icon, a display title and a hint, plus
// an optional primary action.
export function emptyState(
  iconTpl: TemplateResult | string,
  title: string,
  hint: string,
  action?: TemplateResult | typeof nothing,
): TemplateResult {
  return html`<div
    class="border border-dashed border-hair p-12 flex flex-col items-center text-center gap-3"
  >
    <span class="text-brand">${iconTpl}</span>
    <div>
      <p class="font-disp font-extrabold text-lg uppercase tracking-[-0.02em]">
        ${title}
      </p>
      <p class="text-ink3 mt-1">${hint}</p>
    </div>
    ${action ?? nothing}
  </div>`;
}

// Skeleton rows for a loading list.
export function skeletonRows(count = 6): TemplateResult {
  return html`<div class="flex flex-col gap-px" aria-busy="true">
    ${Array.from({ length: count }).map(
      () => html`<div class="h-12 w-full bg-paper animate-pulse"></div>`,
    )}
  </div>`;
}

// A small spinner for inline busy states (e.g. "load more").
export function spinner(): TemplateResult {
  return html`<span
    class="inline-block size-3 animate-spin rounded-full border border-current border-t-transparent"
  ></span>`;
}
