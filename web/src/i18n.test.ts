import { expect } from "@open-wc/testing";
import {
  t,
  setLocale,
  currentLocale,
  onLocaleChange,
  availableLocales,
} from "./i18n.js";

// Locale is module-global, so reset to English after each test to avoid leaking
// a switched language into other test files (which assert English copy).
describe("i18n", () => {
  afterEach(() => {
    setLocale("en");
    try {
      localStorage.removeItem("pulse.locale");
    } catch {
      // ignore
    }
  });

  it("defaults to English", () => {
    expect(currentLocale()).to.equal("en");
    expect(t("nav.logout")).to.equal("Log out");
  });

  it("switches locale and returns translated copy", () => {
    setLocale("de");
    expect(currentLocale()).to.equal("de");
    expect(t("nav.logout")).to.equal("Abmelden");
    expect(t("nav.monitors")).to.equal("Monitore");
    setLocale("es");
    expect(t("nav.monitors")).to.equal("Monitores");
  });

  it("notifies subscribers exactly once per change", () => {
    let calls = 0;
    const off = onLocaleChange(() => calls++);
    setLocale("de");
    setLocale("de"); // same locale: no notification
    off();
    setLocale("es"); // after unsubscribe: not counted
    expect(calls).to.equal(1);
  });

  it("offers English plus at least two more locales", () => {
    expect(availableLocales.length).to.be.greaterThan(2);
    expect(availableLocales.map((l) => l.code)).to.include("en");
  });
});
