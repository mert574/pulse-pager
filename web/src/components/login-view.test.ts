import { expect, fixture, html } from "@open-wc/testing";
import "./login-view.js";
import type { LoginView } from "./login-view.js";

// login-view: renders the two provider buttons, navigates the browser to the
// auth-plane redirect with return_to, and surfaces a callback error param.
//
// window.location.assign is replaced with a recorder so the test never actually
// navigates. The dev button only renders under import.meta.env.DEV, which is true
// in the test runner (esbuild dev build), so we assert it is present here.

// Override the instance's redirect() so the test records the URL instead of
// actually navigating the browser (window.location.assign is read-only).
function spyRedirect(el: LoginView): string[] {
  const calls: string[] = [];
  (el as unknown as { redirect: (u: string) => void }).redirect = (u) =>
    calls.push(u);
  return calls;
}

describe("login-view", () => {
  it("renders Google and GitHub sign-in buttons", async () => {
    const el = await fixture<LoginView>(html`<login-view></login-view>`);
    const buttons = [...el.querySelectorAll("button")];
    const labels = buttons.map((b) => b.textContent?.trim());
    expect(labels.some((l) => l?.includes("Google"))).to.be.true;
    expect(labels.some((l) => l?.includes("GitHub"))).to.be.true;
  });

  it("navigates to the provider login with return_to", async () => {
    const el = await fixture<LoginView>(html`<login-view></login-view>`);
    const calls = spyRedirect(el);
    const google = [...el.querySelectorAll("button")].find((b) =>
      b.textContent?.includes("Google"),
    )!;
    google.click();
    expect(calls.length).to.equal(1);
    expect(calls[0]).to.contain("/auth/google/login");
    expect(calls[0]).to.contain("return_to=");
  });

  it("renders the dev sign-in affordance under DEV", async () => {
    const el = await fixture<LoginView>(html`<login-view></login-view>`);
    const dev = [...el.querySelectorAll("button")].find((b) =>
      b.textContent?.includes("Dev"),
    );
    // the test runner builds with import.meta.env.DEV true
    expect(dev).to.not.be.undefined;
  });

  it("shows an error message when the URL carries ?error", async () => {
    const url = new URL(window.location.href);
    url.searchParams.set("error", "access_denied");
    window.history.replaceState({}, "", url.toString());
    try {
      const el = await fixture<LoginView>(html`<login-view></login-view>`);
      const alert = el.querySelector('[role="alert"]');
      expect(alert).to.not.be.null;
      expect(alert?.textContent).to.contain("cancelled");
    } finally {
      const clean = new URL(window.location.href);
      clean.searchParams.delete("error");
      window.history.replaceState({}, "", clean.toString());
    }
  });
});
