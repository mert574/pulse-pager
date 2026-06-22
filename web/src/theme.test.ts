import { expect } from "@open-wc/testing";
import { currentTheme, setTheme, toggleTheme } from "./theme.js";

describe("theme", () => {
  afterEach(() => {
    setTheme("caramellatte");
    try {
      localStorage.removeItem("pulse.theme");
    } catch {
      // ignore
    }
  });

  it("sets and reads data-theme on <html>", () => {
    setTheme("coffee");
    expect(document.documentElement.dataset.theme).to.equal("coffee");
    expect(currentTheme()).to.equal("coffee");
  });

  it("toggles between caramellatte and coffee", () => {
    setTheme("caramellatte");
    expect(toggleTheme()).to.equal("coffee");
    expect(currentTheme()).to.equal("coffee");
    expect(toggleTheme()).to.equal("caramellatte");
    expect(currentTheme()).to.equal("caramellatte");
  });

  it("persists the chosen theme", () => {
    setTheme("coffee");
    expect(localStorage.getItem("pulse.theme")).to.equal("coffee");
  });
});
