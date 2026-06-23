import { expect, waitUntil } from "@open-wc/testing";
import { ContextProvider } from "@lit/context";
import "./channel-form-view.js";
import type { ChannelFormView } from "./channel-form-view.js";
import { appContext, type AppContext } from "../state/context.js";
import type {
  Channel,
  ChannelTypeCatalog,
  OrgMembership,
} from "../api/types.js";

const ORG: OrgMembership = {
  org_id: "o1",
  name: "Org One",
  slug: "org-one",
  role: "owner",
  plan: "tier3",
};

// A catalog with one available type carrying a string, a secret string, an enum,
// and a bool field, plus one plan-gated (unavailable) type with a reason.
const CATALOG: ChannelTypeCatalog = {
  channel_types: [
    {
      type: "webhook",
      display_name: "Webhook",
      available: true,
      config_fields: [
        {
          key: "url",
          type: "string",
          required: true,
          secret: false,
          label: { code: "channel.webhook.url.label", message: "Endpoint URL" },
          help: { code: "channel.webhook.url.help", message: "Where to POST." },
        },
        {
          key: "token",
          type: "string",
          required: false,
          secret: true,
          label: { code: "channel.webhook.token.label", message: "Auth token" },
        },
        {
          key: "format",
          type: "enum",
          required: false,
          secret: false,
          enum: ["json", "form"],
          label: { code: "channel.webhook.format.label", message: "Payload format" },
        },
      ],
    },
    {
      type: "pagerduty",
      display_name: "PagerDuty",
      available: false,
      required_plan: "tierCustom",
      // an unseeded code, so tDynamic falls back to the API-provided message
      unavailable_reason: {
        code: "channel.unavailable.pagerduty_business",
        message: "Upgrade to use PagerDuty.",
      },
      config_fields: [],
    },
  ],
};

const EDIT_CHANNEL: Channel = {
  id: "ch_1",
  org_id: "o1",
  name: "On-call webhook",
  type: "webhook",
  enabled: true,
  config: {
    url: "https://hooks.example.com/x",
    // the secret comes back redacted via a "_set" marker, not the value
    token_set: true,
    format: "json",
  },
};

interface Call {
  url: string;
  method: string;
  body: string | null;
}

function installFetch(handler: (c: Call) => Response): {
  calls: Call[];
  restore: () => void;
} {
  const calls: Call[] = [];
  const original = globalThis.fetch;
  globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    const call: Call = {
      url: String(input).split("?")[0],
      method: init?.method ?? "GET",
      body: (init?.body as string) ?? null,
    };
    calls.push(call);
    return Promise.resolve(handler(call));
  }) as typeof fetch;
  return { calls, restore: () => (globalThis.fetch = original) };
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

// Default route: catalog on GET /channel-types, channel list on GET /channels,
// 201/200 on create/update.
function route(c: Call): Response {
  if (c.url.endsWith("/channel-types")) return json(200, CATALOG);
  if (c.url.endsWith("/channels") && c.method === "GET")
    return json(200, [EDIT_CHANNEL]);
  if (c.url.endsWith("/channels") && c.method === "POST")
    return json(201, { id: "ch_new", org_id: "o1" });
  if (c.method === "PUT") return json(200, { id: "ch_1", org_id: "o1" });
  return json(200, {});
}

async function mount(channelId = ""): Promise<{
  el: ChannelFormView;
  calls: Call[];
  restore: () => void;
}> {
  const { calls, restore } = installFetch(route);
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
  const el = document.createElement("channel-form-view") as ChannelFormView;
  if (channelId) el.channelId = channelId;
  host.appendChild(el);
  await el.updateComplete;
  return { el, calls, restore };
}

function setInput(el: ChannelFormView, selector: string, value: string): void {
  const input = el.querySelector<HTMLInputElement>(selector)!;
  input.value = value;
  input.dispatchEvent(new Event("input", { bubbles: true }));
}

describe("channel-form-view (create)", () => {
  it("renders the type picker and an upsell for a plan-gated type", async () => {
    const { el, restore } = await mount();
    try {
      await waitUntil(
        () => el.textContent?.includes("Webhook") ?? false,
        "catalog renders",
      );
      // available type is a selectable button; gated types collapse into a single
      // upsell that names them (not one banner per type).
      expect(el.textContent).to.contain("PagerDuty");
      expect(el.querySelectorAll("upsell-banner").length).to.equal(1);
      expect(el.textContent).to.contain("PagerDuty are available on a higher plan");
    } finally {
      restore();
    }
  });

  it("renders config fields dynamically once a type is picked", async () => {
    const { el, restore } = await mount();
    try {
      await waitUntil(() => el.textContent?.includes("Webhook") ?? false);
      const typeBtn = Array.from(el.querySelectorAll("button")).find(
        (b) => b.textContent?.trim() === "Webhook",
      )!;
      typeBtn.click();
      await waitUntil(() => el.querySelector("#url") !== null, "config renders");
      // string, secret (password), and enum (select) all rendered from the catalog
      expect(el.querySelector("#url")).to.not.be.null;
      expect(el.querySelector<HTMLInputElement>("#token")?.type).to.equal("password");
      expect(el.querySelector("select#format")).to.not.be.null;
      // localized label uses the API message fallback (no seeded key for these)
      expect(el.textContent).to.contain("Endpoint URL");
    } finally {
      restore();
    }
  });

  it("creates the channel with the entered config on submit", async () => {
    const { el, calls, restore } = await mount();
    try {
      await waitUntil(() => el.textContent?.includes("Webhook") ?? false);
      const typeBtn = Array.from(el.querySelectorAll("button")).find(
        (b) => b.textContent?.trim() === "Webhook",
      )!;
      typeBtn.click();
      await waitUntil(() => el.querySelector("#url") !== null);
      setInput(el, "#name", "My webhook");
      setInput(el, "#url", "https://hooks.example.com/y");
      await el.updateComplete;
      el.querySelector("form")!.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      await waitUntil(
        () => calls.some((c) => c.method === "POST"),
        "create POST fired",
      );
      const post = calls.find((c) => c.method === "POST")!;
      expect(post.url).to.contain("/orgs/o1/channels");
      const sent = JSON.parse(post.body ?? "{}");
      expect(sent.name).to.equal("My webhook");
      expect(sent.type).to.equal("webhook");
      expect(sent.config.url).to.equal("https://hooks.example.com/y");
    } finally {
      restore();
    }
  });

  it("blocks submit and flags a required field left blank", async () => {
    const { el, calls, restore } = await mount();
    try {
      await waitUntil(() => el.textContent?.includes("Webhook") ?? false);
      const typeBtn = Array.from(el.querySelectorAll("button")).find(
        (b) => b.textContent?.trim() === "Webhook",
      )!;
      typeBtn.click();
      await waitUntil(() => el.querySelector("#url") !== null);
      setInput(el, "#name", "No url channel");
      await el.updateComplete;
      el.querySelector("form")!.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      await el.updateComplete;
      expect(el.textContent).to.contain("This field is required");
      expect(calls.some((c) => c.method === "POST")).to.be.false;
    } finally {
      restore();
    }
  });
});

describe("channel-form-view (edit)", () => {
  it("loads the channel, marks the secret configured, and keeps it when blank", async () => {
    const { el, calls, restore } = await mount("ch_1");
    try {
      await waitUntil(() => el.querySelector("#url") !== null, "form loads");
      // non-secret value is filled, secret stays blank and shows "Configured"
      expect(el.querySelector<HTMLInputElement>("#url")?.value).to.equal(
        "https://hooks.example.com/x",
      );
      expect(el.querySelector<HTMLInputElement>("#token")?.value).to.equal("");
      expect(el.textContent).to.contain("Configured");

      el.querySelector("form")!.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      await waitUntil(() => calls.some((c) => c.method === "PUT"), "PUT fired");
      const put = calls.find((c) => c.method === "PUT")!;
      const sent = JSON.parse(put.body ?? "{}");
      // blank secret is omitted so the stored value is kept unchanged
      expect("token" in sent.config).to.be.false;
      expect(sent.config.url).to.equal("https://hooks.example.com/x");
    } finally {
      restore();
    }
  });

  it("sends a replacement secret when the user types a new value", async () => {
    const { el, calls, restore } = await mount("ch_1");
    try {
      await waitUntil(() => el.querySelector("#token") !== null);
      setInput(el, "#token", "new-secret-value");
      await el.updateComplete;
      el.querySelector("form")!.dispatchEvent(
        new Event("submit", { bubbles: true, cancelable: true }),
      );
      await waitUntil(() => calls.some((c) => c.method === "PUT"));
      const put = calls.find((c) => c.method === "PUT")!;
      const sent = JSON.parse(put.body ?? "{}");
      expect(sent.config.token).to.equal("new-secret-value");
    } finally {
      restore();
    }
  });
});
