// One light shared context (RFC-013 section 6.1) carrying the session identity,
// the active org (derived from the route :orgId), the active role, and the active
// org's entitlements. Deep components (nav, switcher, guarded buttons, upsell
// banners) consume this instead of prop-drilling or each importing singletons.
//
// app-root is the single provider; it owns bootstrap and recomputes the value on
// navigation and on session change. Decision D7: per-view @state plus this one
// @lit/context, no Redux/global store.

import { createContext } from "@lit/context";
import type { Entitlements, Me, OrgMembership, Role } from "../api/types.js";

export interface AppContext {
  // identity + org list, from /me. null before bootstrap resolves or when logged out.
  me: Me | null;
  // the membership for the :orgId in the current route, or null on account/global
  // routes (e.g. /account, /login) or when the route org is not one the user is in.
  activeOrg: OrgMembership | null;
  // activeOrg.role, drives can(). null when there is no active org.
  role: Role | null;
  // the active org's plan caps + usage; null until fetched or off an org route.
  entitlements: Entitlements | null;
  // re-pull /me after a role/org/membership change so the menus correct themselves.
  refreshMe(): Promise<void>;
}

export const appContext = createContext<AppContext>(Symbol("pulse.app-context"));

// Read the active org id from a base-relative path under /orgs/:orgId. Returns
// null for non-org routes. The URL path is the single source of truth for the
// active org (RFC-013 section 4.1), so this is the only place it is derived.
export function activeOrgIdFromPath(path: string): string | null {
  const m = /^\/orgs\/([^/]+)/.exec(path);
  return m ? decodeURIComponent(m[1]) : null;
}

// Non-authoritative hint so a bare visit to "/" can land on the last org used.
// The URL path still wins for any actual request (RFC-013 section 4.1).
const LAST_ORG_KEY = "pulse.last_org";

export function rememberLastOrg(orgId: string): void {
  try {
    localStorage.setItem(LAST_ORG_KEY, orgId);
  } catch {
    // private mode / disabled storage: the hint is optional, ignore
  }
}

export function lastOrgHint(): string | null {
  try {
    return localStorage.getItem(LAST_ORG_KEY);
  } catch {
    return null;
  }
}
