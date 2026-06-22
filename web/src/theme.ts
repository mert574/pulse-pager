// Theme state (RFC-013 section 9.3). Two themes: "caramellatte" (light) and the
// warm dark counterpart "coffee". The active theme is the data-theme attribute on
// <html>; an inline script in the HTML sets it before first paint (no flash),
// reading the same localStorage key, so this module only reads the attribute and
// writes changes.

export type ThemeName = "caramellatte" | "coffee";

const STORAGE_KEY = "pulse.theme";

// The active theme, read from the <html> attribute (the single source of truth).
export function currentTheme(): ThemeName {
  return document.documentElement.dataset.theme === "coffee"
    ? "coffee"
    : "caramellatte";
}

export function setTheme(theme: ThemeName): void {
  document.documentElement.dataset.theme = theme;
  try {
    localStorage.setItem(STORAGE_KEY, theme);
  } catch {
    // private mode / disabled storage: theme still applies for this session
  }
}

// Flip between the two themes and return the new one.
export function toggleTheme(): ThemeName {
  const next: ThemeName = currentTheme() === "coffee" ? "caramellatte" : "coffee";
  setTheme(next);
  return next;
}
