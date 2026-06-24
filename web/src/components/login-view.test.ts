import { expect, fixture, html, waitUntil } from "@open-wc/testing";
import "./login-view.js";
import type { LoginView } from "./login-view.js";

// Mock fetch so the email-login submit hits a recorded handler instead of the
// network, mirroring the account-view test's helper.
interface Call {
  url: string;
  method: string;
}

function installFetch(handler: (call: Call) => Response): {
  calls: Call[];
  restore: () => void;
} {
  const calls: Call[] = [];
  const original = globalThis.fetch;
  globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    const call: Call = { url: String(input), method: init?.method ?? "GET" };
    calls.push(call);
    return Promise.resolve(handler(call));
  }) as typeof fetch;
  return { calls, restore: () => (globalThis.fetch = original) };
}

function json(status: number, body: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

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

// Tick the Terms + Privacy consent box, which gates every sign-in method.
async function agree(el: LoginView): Promise<void> {
  const box = el.querySelector<HTMLInputElement>("[data-agree]")!;
  box.checked = true;
  box.dispatchEvent(new Event("change"));
  await el.updateComplete;
}

describe("login-view", () => {
  it("renders the GitHub button and no Google button (Google is not configured)", async () => {
    const el = await fixture<LoginView>(html`<login-view></login-view>`);
    const labels = [...el.querySelectorAll("button")].map((b) =>
      b.textContent?.trim(),
    );
    expect(labels.some((l) => l?.includes("GitHub"))).to.be.true;
    expect(labels.some((l) => l?.includes("Google"))).to.be.false;
  });

  it("gates sign-in on the consent box, then navigates with return_to", async () => {
    const el = await fixture<LoginView>(html`<login-view></login-view>`);
    const calls = spyRedirect(el);
    const github = el.querySelector<HTMLButtonElement>(
      'button[data-provider="github"]',
    )!;
    // disabled until consent: a click does nothing
    expect(github.disabled).to.be.true;
    github.click();
    expect(calls.length).to.equal(0);
    // after consent the button enables and navigates to the GitHub auth redirect
    await agree(el);
    expect(github.disabled).to.be.false;
    github.click();
    expect(calls.length).to.equal(1);
    expect(calls[0]).to.contain("/auth/github/login");
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

  it("has no password field (passwordless login)", async () => {
    const el = await fixture<LoginView>(html`<login-view></login-view>`);
    expect(el.querySelector('input[type="password"]')).to.be.null;
    // the email input is present instead
    expect(el.querySelector('input[type="email"]')).to.not.be.null;
  });

  it("shows the neutral confirmation after submitting an email", async () => {
    const { calls, restore } = installFetch(() => json(200, { ok: true }));
    try {
      const el = await fixture<LoginView>(html`<login-view></login-view>`);
      await agree(el);
      const input = el.querySelector<HTMLInputElement>('input[type="email"]')!;
      input.value = "person@example.com";
      input.dispatchEvent(new Event("input"));
      const form = el.querySelector("form")!;
      form.dispatchEvent(new Event("submit"));

      await waitUntil(
        () => el.querySelector('[role="status"]') !== null,
        "neutral confirmation shown",
      );
      const status = el.querySelector('[role="status"]')!;
      expect(status.textContent).to.contain("sign-in link");
      // it POSTed to the start endpoint with the typed email
      const start = calls.find((c) => c.url.includes("/auth/email/start"));
      expect(start).to.not.be.undefined;
      expect(start?.method).to.equal("POST");
      // the form is gone once confirmed
      expect(el.querySelector("form")).to.be.null;
    } finally {
      restore();
    }
  });

  it("treats a rate-limit (429) the same as success, so the limit doesn't leak", async () => {
    const { restore } = installFetch(() =>
      json(429, { error: { code: "rate_limited", message: "" } }),
    );
    try {
      const el = await fixture<LoginView>(html`<login-view></login-view>`);
      await agree(el);
      const input = el.querySelector<HTMLInputElement>('input[type="email"]')!;
      input.value = "person@example.com";
      input.dispatchEvent(new Event("input"));
      el.querySelector("form")!.dispatchEvent(new Event("submit"));
      await waitUntil(
        () => el.querySelector('[role="status"]') !== null,
        "neutral confirmation shown on 429",
      );
      expect(el.querySelector('[role="status"]')).to.not.be.null;
    } finally {
      restore();
    }
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
