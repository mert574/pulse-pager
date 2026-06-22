// Entry for the public status page (RFC-013 section 8). Deliberately self-contained:
// it imports only its own component and the shared CSS tokens, never the authed
// app's router, api client, session, or context. Keeping the import graph clean is
// what holds the bundle under its 30 KB budget and removes any chance of the
// authed app's auth/CSRF code being dragged onto a public, cacheable page.

import { initLocale } from "../i18n.js";
import "@fontsource-variable/inter/index.css";
import "../styles/app.css";
import "./status-page.js";

initLocale();
