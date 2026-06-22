import { expect, fixture, html } from "@open-wc/testing";
import "./relative-time.js";
import type { RelativeTime } from "./relative-time.js";
import { formatDateTime } from "../format.js";

describe("relative-time", () => {
  it("shows a relative label with the full timestamp on hover", async () => {
    const iso = new Date(Date.now() - 5 * 60 * 1000).toISOString();
    const el = await fixture<RelativeTime>(
      html`<relative-time .datetime=${iso}></relative-time>`,
    );
    // a few-minutes-old time reads as "Nm ago".
    expect(el.textContent?.trim()).to.match(/^\d+m ago$/);
    // the hover title is the full localized date+time.
    const span = el.querySelector("span");
    expect(span?.getAttribute("title")).to.equal(formatDateTime(iso));
  });

  it("reads as 'just now' inside the last minute", async () => {
    const iso = new Date(Date.now() - 5 * 1000).toISOString();
    const el = await fixture<RelativeTime>(
      html`<relative-time .datetime=${iso}></relative-time>`,
    );
    expect(el.textContent?.trim()).to.equal("just now");
  });

  it("renders nothing for an empty datetime", async () => {
    const el = await fixture<RelativeTime>(
      html`<relative-time .datetime=${""}></relative-time>`,
    );
    expect(el.textContent?.trim()).to.equal("");
  });
});
