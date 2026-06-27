import { expect, fixture, html } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./app-nav.js";
import type { AppNav } from "./app-nav.js";
import { appContext, type AppContext } from "../state/context.js";
import type { OrgMembership } from "../api/types.js";
import { t } from "../i18n.js";

const ORG: OrgMembership = {
  org_id: "o1",
  name: "Org One",
  slug: "org-one",
  role: "owner",
  plan: "tier3",
};

// Mount app-nav under a context provider with an active org, at the given path.
async function mountAtPath(path: string): Promise<AppNav> {
  history.pushState({}, "", path);
  const host = document.createElement("div");
  const ctx: AppContext = {
    me: { user_id: "u", email: "e", name: "n", avatar_url: null, orgs: [ORG] },
    activeOrg: ORG,
    role: "owner",
    entitlements: null,
    refreshMe: async () => {},
  };
  new ContextProvider(host, { context: appContext, initialValue: ctx });
  document.body.appendChild(host);
  host.innerHTML = "<app-nav></app-nav>";
  const nav = host.querySelector<AppNav>("app-nav")!;
  await nav.updateComplete;
  return nav;
}

// Active links carry aria-current="page"; the label is the link's first span (the
// trailing span is the index chip), so read that rather than the whole textContent.
function activeLabels(nav: AppNav): string[] {
  return Array.from(nav.querySelectorAll('a[aria-current="page"]')).map(
    (a) => a.querySelector("span")?.textContent?.trim() ?? a.textContent?.trim() ?? "",
  );
}

describe("app-nav i18n rendering", () => {
  it("renders translated copy, not the raw keys", async () => {
    const el = await fixture<AppNav>(html`<app-nav></app-nav>`);
    await el.updateComplete;
    const text = el.textContent ?? "";
    expect(text).to.contain(t("nav.account"));
    // logout is an icon button; its accessible name carries the translated copy.
    // Match the label exactly: the footer also holds the theme-toggle button.
    const logout = el.querySelector(`button[aria-label="${t("nav.logout")}"]`);
    expect(logout, "logout button present with translated aria-label").to.not.be
      .null;
    expect(text).to.not.contain("nav.account");
    expect(text).to.not.contain("nav.logout");
  });
});

describe("app-nav active-link highlighting", () => {
  it("on a section page, only that section is active (not Monitors/home)", async () => {
    const nav = await mountAtPath("/orgs/o1/channels");
    expect(activeLabels(nav)).to.deep.equal([t("nav.channels")]);
  });

  it("on the org home, Monitors is the only active link", async () => {
    const nav = await mountAtPath("/orgs/o1");
    expect(activeLabels(nav)).to.deep.equal([t("nav.monitors")]);
  });

  it("on a monitor sub-path, Monitors stays active", async () => {
    const nav = await mountAtPath("/orgs/o1/monitors/m1");
    expect(activeLabels(nav)).to.deep.equal([t("nav.monitors")]);
  });

  it("on settings, only Settings is active", async () => {
    const nav = await mountAtPath("/orgs/o1/settings");
    expect(activeLabels(nav)).to.deep.equal([t("nav.settings")]);
  });
});
