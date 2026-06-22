import { expect, fixture, html } from "@open-wc/testing";
import "./confirm-dialog.js";
import type { ConfirmDialog } from "./confirm-dialog.js";

// confirm-dialog focus behavior (RFC-013 section 9.1). Components render in light
// DOM, so we query the element's own subtree (not a shadow root).
function tab(shiftKey = false): void {
  window.dispatchEvent(
    new KeyboardEvent("keydown", { key: "Tab", shiftKey, bubbles: true }),
  );
}

function buttons(el: ConfirmDialog): HTMLElement[] {
  return Array.from(el.querySelectorAll<HTMLElement>(".modal-box button"));
}

describe("confirm-dialog focus management", () => {
  it("moves focus to the first (cancel) button on open", async () => {
    const el = await fixture<ConfirmDialog>(
      html`<confirm-dialog></confirm-dialog>`,
    );
    el.open();
    await el.updateComplete;
    expect(document.activeElement).to.equal(buttons(el)[0]);
  });

  it("traps Tab within the dialog (last wraps to first)", async () => {
    const el = await fixture<ConfirmDialog>(
      html`<confirm-dialog></confirm-dialog>`,
    );
    el.open();
    await el.updateComplete;
    const bs = buttons(el);
    bs[bs.length - 1].focus();
    tab();
    expect(document.activeElement).to.equal(bs[0]);
  });

  it("traps Shift+Tab within the dialog (first wraps to last)", async () => {
    const el = await fixture<ConfirmDialog>(
      html`<confirm-dialog></confirm-dialog>`,
    );
    el.open();
    await el.updateComplete;
    const bs = buttons(el);
    bs[0].focus();
    tab(true);
    expect(document.activeElement).to.equal(bs[bs.length - 1]);
  });

  it("restores focus to the trigger on close", async () => {
    const wrap = await fixture(html`
      <div>
        <button id="trigger">open</button>
        <confirm-dialog></confirm-dialog>
      </div>
    `);
    const trigger = wrap.querySelector<HTMLButtonElement>("#trigger")!;
    const dialog = wrap.querySelector<ConfirmDialog>("confirm-dialog")!;
    trigger.focus();
    expect(document.activeElement).to.equal(trigger);

    dialog.open();
    await dialog.updateComplete;
    expect(document.activeElement).to.not.equal(trigger);

    dialog.close();
    await dialog.updateComplete;
    expect(document.activeElement).to.equal(trigger);
  });
});
