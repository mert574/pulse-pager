import { expect } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./org-switcher.js";
import type { OrgSwitcher } from "./org-switcher.js";
import { appContext, type AppContext } from "../state/context.js";
import type { OrgMembership } from "../api/types.js";

// org-switcher: a dropdown listing the session orgs. Each item is rich and
// multiline (name, role + plan badges, org id); the active org's role/plan also
// shows under the trigger. Picking an org navigates to its path, and the
// "create" item navigates to /orgs/new.

const ORGS: OrgMembership[] = [
  { org_id: "o1", name: "Org One", slug: "org-one", role: "owner", plan: "team" },
  { org_id: "o2", name: "Org Two", slug: "org-two", role: "member", plan: "free" },
];

async function mount(activeId: string): Promise<OrgSwitcher> {
  const active = ORGS.find((o) => o.org_id === activeId) ?? null;
  const ctx: AppContext = {
    me: {
      user_id: "u",
      email: "e",
      name: "n",
      avatar_url: null,
      locale: "en",
      timezone: "UTC",
      orgs: ORGS,
    },
    activeOrg: active,
    role: active?.role ?? null,
    entitlements: null,
    refreshMe: async () => {},
  };
  const host = document.createElement("div");
  new ContextProvider(host, { context: appContext, initialValue: ctx });
  document.body.appendChild(host);
  host.innerHTML = "<org-switcher></org-switcher>";
  const el = host.querySelector<OrgSwitcher>("org-switcher")!;
  await el.updateComplete;
  return el;
}

function items(el: OrgSwitcher): HTMLAnchorElement[] {
  return [...el.querySelectorAll<HTMLAnchorElement>(".dropdown-content a")];
}

describe("org-switcher", () => {
  it("lists every org plus the create option", async () => {
    const el = await mount("o1");
    const text = items(el).map((a) => a.textContent ?? "");
    expect(text.some((t) => t.includes("Org One"))).to.be.true;
    expect(text.some((t) => t.includes("Org Two"))).to.be.true;
    expect(text.some((t) => t.includes("Create"))).to.be.true;
  });

  it("each org item shows its role, plan, and id", async () => {
    const el = await mount("o1");
    const ownerItem = items(el).find((a) => a.textContent?.includes("Org One"))!;
    const text = ownerItem.textContent ?? "";
    expect(text).to.contain("Owner");
    expect(text).to.contain("Team");
    expect(text).to.contain("o1");
  });

  it("the active org's role and plan show on the trigger", async () => {
    const el = await mount("o2");
    const trigger = el.querySelector('[role="button"]')!;
    expect(trigger.textContent).to.contain("Org Two");
    expect(trigger.textContent).to.contain("Member");
    expect(trigger.textContent).to.contain("Free");
  });

  it("picking an org navigates to its path", async () => {
    const el = await mount("o1");
    const other = items(el).find((a) => a.textContent?.includes("Org Two"))!;
    other.click();
    expect(window.location.pathname).to.contain("/orgs/o2");
  });

  it("the create item navigates to /orgs/new", async () => {
    const el = await mount("o1");
    const create = items(el).find((a) => a.textContent?.includes("Create"))!;
    create.click();
    expect(window.location.pathname).to.contain("/orgs/new");
  });
});
