import { expect, fixture, html } from "@open-wc/testing";
import "./toast-host.js";
import type { ToastHost } from "./toast-host.js";
import { toast, dismissToast, subscribeToasts, type ToastItem } from "../toast.js";

// Long ttl so the auto-dismiss timer never fires mid-test; we dismiss manually.
const TTL = 1_000_000;

describe("toast service", () => {
  it("notifies subscribers on toast and dismiss", () => {
    let latest: ToastItem[] = [];
    const off = subscribeToasts((items) => (latest = items));
    expect(latest.length).to.equal(0);

    toast("saved", "success", TTL);
    expect(latest.length).to.equal(1);
    expect(latest[0].message).to.equal("saved");
    expect(latest[0].type).to.equal("success");

    dismissToast(latest[0].id);
    expect(latest.length).to.equal(0);
    off();
  });
});

describe("toast-host", () => {
  it("renders active toasts and clears them on dismiss", async () => {
    const el = await fixture<ToastHost>(html`<toast-host></toast-host>`);
    toast("hello world", "info", TTL);
    await el.updateComplete;
    const alert = el.querySelector('[role="status"]');
    expect(alert).to.not.be.null;
    expect(el.textContent).to.contain("hello world");

    // clicking a toast dismisses it
    (alert as HTMLElement).click();
    await el.updateComplete;
    expect(el.querySelector('[role="status"]')).to.be.null;
  });
});
