// Display formatting helpers. Dates come off the wire as RFC3339 UTC strings and
// are rendered with Intl in the active locale (RFC-013 section 9.2), so the wire
// stays UTC and only the presentation is localized.

import { getReasonPhrase } from "http-status-codes";
import { currentLocale, tDynamic } from "./i18n.js";

// Localized date+time, or null for a null/invalid input (callers show a fallback).
export function formatDateTime(iso: string | null): string | null {
  if (!iso) return null;
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return null;
  return new Intl.DateTimeFormat(currentLocale(), {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(d);
}

// Latency in milliseconds as "<n> ms", or null when there is no value.
export function formatLatency(ms: number | null): string | null {
  if (ms === null) return null;
  return `${ms.toLocaleString(currentLocale())} ms`;
}

// An HTTP status code with its reason phrase, e.g. "503 Service Unavailable".
// Unknown codes render as the bare number; null renders empty.
export function formatStatusCode(code: number | null): string {
  if (code === null) return "";
  try {
    return `${code} ${getReasonPhrase(code)}`;
  } catch {
    return String(code);
  }
}

// A duration in seconds as a short "1h 5m" / "30m" / "45s" string. Unit letters
// are not localized (symbol-like, same as "ms"). Empty for a null input.
export function formatDuration(seconds: number | null): string {
  if (seconds === null) return "";
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const rem = minutes % 60;
  return rem ? `${hours}h ${rem}m` : `${hours}h`;
}

// A short relative time like "5m ago" / "2d ago" / "in 3d", or "just now" inside the
// last minute. Empty for null/invalid. The unit letters match formatDuration (not
// localized); the surrounding words are. Pairs with <relative-time>, which shows the
// full localized timestamp on hover.
export function formatRelative(iso: string | null): string {
  if (!iso) return "";
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "";
  const diffMs = Date.now() - then;
  const sec = Math.abs(diffMs) / 1000;
  if (sec < 45) return tDynamic("time.justNow", "just now");
  const mag = relativeMagnitude(sec);
  return diffMs >= 0
    ? tDynamic("time.ago", "{when} ago", { when: mag })
    : tDynamic("time.in", "in {when}", { when: mag });
}

// The bare magnitude of a relative time ("5m", "3h", "2d", "4mo", "1y"), rounded to
// the largest sensible unit. Seconds in, never below a minute (formatRelative shows
// "just now" under 45s).
function relativeMagnitude(sec: number): string {
  if (sec < 90) return "1m";
  const min = Math.round(sec / 60);
  if (min < 60) return `${min}m`;
  if (min < 90) return "1h";
  const hr = Math.round(min / 60);
  if (hr < 24) return `${hr}h`;
  if (hr < 36) return "1d";
  const day = Math.round(hr / 24);
  if (day < 30) return `${day}d`;
  if (day < 45) return "1mo";
  const mo = Math.round(day / 30);
  if (mo < 12) return `${mo}mo`;
  return `${Math.round(mo / 12)}y`;
}

// Whole seconds from now until a future RFC3339 instant, clamped at 0. null for a
// null/invalid input. The next-check countdown uses this against next_check_at.
export function secondsUntil(iso: string | null): number | null {
  if (!iso) return null;
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return null;
  return Math.max(0, Math.round((d.getTime() - Date.now()) / 1000));
}
