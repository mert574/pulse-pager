import { expect, fixture, html } from "@open-wc/testing";
import "./region-chips.js";
import type { RegionChips } from "./region-chips.js";
import type { RegionState } from "../api/types.js";

async function mount(states: RegionState[]): Promise<RegionChips> {
  return fixture<RegionChips>(
    html`<region-chips .states=${states}></region-chips>`,
  );
}

describe("region-chips", () => {
  it("renders nothing for an empty state list", async () => {
    const el = await mount([]);
    expect(el.querySelector(".badge")).to.be.null;
  });

  it("renders one chip per region with the region name and a label", async () => {
    const el = await mount([
      { region: "home", state: "scheduled", updated_at: "2026-06-21T10:00:00Z" },
      { region: "eu-west", state: "running", updated_at: "2026-06-21T10:00:00Z" },
    ]);
    const chips = el.querySelectorAll(".badge");
    expect(chips.length).to.equal(2);
    expect(el.textContent).to.contain("home");
    expect(el.textContent).to.contain("scheduled");
    expect(el.textContent).to.contain("eu-west");
    expect(el.textContent).to.contain("pinging");
  });

  it("reads done + healthy as ok and gives it the success color", async () => {
    const el = await mount([
      {
        region: "home",
        state: "done",
        healthy: true,
        latency_ms: 42,
        status_code: 200,
        updated_at: "2026-06-21T10:00:00Z",
      },
    ]);
    const chip = el.querySelector(".badge")!;
    expect(chip.textContent).to.contain("ok");
    expect(chip.className).to.contain("badge-success");
  });

  it("reads done + unhealthy as down with the error color", async () => {
    const el = await mount([
      {
        region: "home",
        state: "done",
        healthy: false,
        status_code: 503,
        updated_at: "2026-06-21T10:00:00Z",
      },
    ]);
    const chip = el.querySelector(".badge")!;
    expect(chip.textContent).to.contain("down");
    expect(chip.className).to.contain("badge-error");
  });

  it("reads failed as down with the error color", async () => {
    const el = await mount([
      {
        region: "home",
        state: "failed",
        failure_reason: "timeout",
        updated_at: "2026-06-21T10:00:00Z",
      },
    ]);
    const chip = el.querySelector(".badge")!;
    expect(chip.textContent).to.contain("down");
    expect(chip.className).to.contain("badge-error");
  });

  it("puts latency and status code in the chip tooltip when present", async () => {
    const el = await mount([
      {
        region: "home",
        state: "done",
        healthy: true,
        latency_ms: 88,
        status_code: 200,
        updated_at: "2026-06-21T10:00:00Z",
      },
    ]);
    const title = el.querySelector(".badge")?.getAttribute("title") ?? "";
    expect(title).to.contain("home");
    expect(title).to.contain("88 ms");
    expect(title).to.contain("200");
  });

  it("pulses the dot only while running", async () => {
    const el = await mount([
      { region: "home", state: "running", updated_at: "2026-06-21T10:00:00Z" },
    ]);
    expect(el.querySelector(".animate-pulse")).to.not.be.null;
    const done = await mount([
      { region: "home", state: "done", healthy: true, updated_at: "2026-06-21T10:00:00Z" },
    ]);
    expect(done.querySelector(".animate-pulse")).to.be.null;
  });
});
