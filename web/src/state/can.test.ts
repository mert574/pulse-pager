import { expect } from "@open-wc/testing";
import { can } from "./can.js";

// Role/entitlement mirror test (RFC-013 section 11): can() matches the RFC-003
// 7.2 / PRD-001 capability matrix summarised in RFC-013 section 4.3.
describe("can()", () => {
  it("viewer can only read", () => {
    expect(can("viewer", "monitor.write")).to.be.false;
    expect(can("viewer", "monitor.test")).to.be.false;
    expect(can("viewer", "incident.annotate")).to.be.false;
  });

  it("member can write monitors/channels, test, annotate, edit status pages", () => {
    expect(can("member", "monitor.write")).to.be.true;
    expect(can("member", "channel.write")).to.be.true;
    expect(can("member", "channel.test")).to.be.true;
    expect(can("member", "incident.annotate")).to.be.true;
    expect(can("member", "statuspage.write")).to.be.true;
    // but not admin actions
    expect(can("member", "member.manage")).to.be.false;
    expect(can("member", "incident.close")).to.be.false;
    expect(can("member", "billing.view")).to.be.false;
  });

  it("admin manages members, api keys, settings, billing read, manual close", () => {
    expect(can("admin", "member.manage")).to.be.true;
    expect(can("admin", "apikey.manage")).to.be.true;
    expect(can("admin", "org.settings")).to.be.true;
    expect(can("admin", "audit.view")).to.be.true;
    expect(can("admin", "billing.view")).to.be.true;
    expect(can("admin", "incident.close")).to.be.true;
    // but not owner-only actions
    expect(can("admin", "billing.manage")).to.be.false;
    expect(can("admin", "org.transfer")).to.be.false;
    expect(can("admin", "org.delete")).to.be.false;
  });

  it("owner can do everything including owner-only actions", () => {
    expect(can("owner", "billing.manage")).to.be.true;
    expect(can("owner", "org.transfer")).to.be.true;
    expect(can("owner", "org.delete")).to.be.true;
    expect(can("owner", "monitor.write")).to.be.true;
  });

  it("null role (no active org) can do nothing org-scoped", () => {
    expect(can(null, "monitor.write")).to.be.false;
    expect(can(null, "billing.view")).to.be.false;
  });
});
