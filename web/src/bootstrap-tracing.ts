// Side-effecting tracing init, imported first by main.ts (before any component).
// Custom elements upgrade synchronously when defined, so app-root's connectedCallback
// (which bootstraps the session via GET /api/v1/me) runs during the component import.
// Registering the tracer here, in an import that evaluates before those, guarantees the
// provider exists before that first request, so even the bootstrap /me is browser-rooted
// (RFC-021 phase 2). Keeping it a separate module (not auto-running in tracing.ts) keeps
// the test bundle from exporting.
import { initTracing } from "./tracing.js";

initTracing();
