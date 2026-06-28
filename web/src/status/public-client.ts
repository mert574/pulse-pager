// Standalone fetch for the public status page (PRD-004 3.6, RFC-013 section 8).
// Deliberately NOT the authed api/client: that one imports the router and session
// and carries cookies + CSRF. The public page is unauthenticated and cache-first,
// so this is a plain GET with no credentials and no auth code at all. Keeping it
// here keeps the public bundle off the authed import graph.

import type { PublicStatusPage } from "../api/types.js";

const API_V1 = "/api/v1";

// Raised when the public endpoint returns a non-2xx so the page can tell a 404
// (unknown or unpublished slug) apart from any other failure.
export class PublicFetchError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "PublicFetchError";
    this.status = status;
  }
}

// GET the public projection for a slug. No credentials so a CDN / browser cache
// can serve it; a 404 means the slug is unknown or the page is still a draft.
export async function fetchPublicStatusPage(slug: string): Promise<PublicStatusPage> {
  const res = await fetch(`${API_V1}/public/status-pages/${encodeURIComponent(slug)}`, {
    credentials: "omit",
  });
  if (!res.ok) {
    throw new PublicFetchError(res.status, `status page request failed (${res.status})`);
  }
  return (await res.json()) as PublicStatusPage;
}

// Resolve which status page to show. The subdomain wins ({slug}.pulsepager.com), so a
// custom-branded host resolves on its own; otherwise a ?slug= query param or the
// last path segment is used, which is handy for local dev and path-based hosting.
// Returns null when no slug can be determined (the page then shows its 404 state).
export function resolveSlug(loc: Location = window.location): string | null {
  const fromQuery = new URLSearchParams(loc.search).get("slug");
  if (fromQuery) return fromQuery;

  const host = loc.hostname;
  const labels = host.split(".");
  // a real subdomain: sub.domain.tld (3+ labels) and not a bare www/localhost
  if (labels.length >= 3 && labels[0] !== "www") {
    return labels[0];
  }

  // fall back to the last non-empty path segment (e.g. /status/acme -> "acme")
  const segments = loc.pathname.split("/").filter((s) => s && s !== "status.html");
  return segments.length ? segments[segments.length - 1] : null;
}
