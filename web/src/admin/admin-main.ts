// Entry for the operator admin app (admin.pulsepager.com). Self-contained: it
// imports only its own shell + the admin view, the shared CSS tokens, and i18n.
// It never imports the customer SPA's router/session/context, so the two apps stay
// on separate origins with isolated cookies.
//
// Auth: in production the origin is behind Cloudflare Access, and the api authorizes
// GET /admin/metrics against the PULSE_PLATFORM_ADMINS allowlist using the verified
// CF Access identity. There is no in-app login. In local dev (import.meta.env.DEV)
// we run the dev API, which gates on a dev session cookie, so we establish one once
// before mounting. This dev-only block is dropped from the production bundle.

import { initLocale } from "../i18n.js";
import "@fontsource-variable/inter/index.css";
import "../styles/app.css";

initLocale();

// dev API only: establish the dev session cookie (GET /auth/dev/login) BEFORE the
// shell mounts, so the first /admin/metrics call is authenticated. Compiled out of
// the production bundle (import.meta.env.DEV is statically false there).
if (import.meta.env.DEV) {
  try {
    await fetch("/auth/dev/login", { credentials: "include" });
  } catch {
    // ignore; the view will show its own error/forbidden state
  }
}

await import("./admin-shell.js");
