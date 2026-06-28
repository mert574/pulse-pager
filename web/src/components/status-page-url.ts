// The public URL a status page resolves at, shown in the editor and list. The
// public page (src/status) resolves its slug from the subdomain in production
// ({slug}.pulsepager.com) and from ?slug= as a fallback. We build the ?slug= form
// here against the current origin so the link works in dev and behind any host
// without depending on DNS being set up.
export function publicStatusUrl(slug: string): string {
  return `${window.location.origin}/status.html?slug=${encodeURIComponent(slug)}`;
}
