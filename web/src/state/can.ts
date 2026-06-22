// Role-aware UI mirror (RFC-013 section 4.3). can(role, action) decides whether
// to show/enable an action for the active org's role. This is a usability mirror
// of the RFC-003 7.2 / PRD-001 capability matrix, NEVER a security boundary: every
// guarded action still hits the server, which re-checks the role per request. If
// /me and the server disagree, the server wins and its 403 shows in the error
// surface.

import type { Role } from "../api/types.js";

// Higher rank = more capability. Used to compare a role against an action's floor.
const RANK: Record<Role, number> = {
  viewer: 0,
  member: 1,
  admin: 2,
  owner: 3,
};

// The action a UI control guards, mapped to the minimum role that may do it.
// Mirrors the summary table in RFC-013 section 4.3.
export type Action =
  | "monitor.write" // create / edit / delete a monitor
  | "monitor.test" // "check now"
  | "channel.write" // create / edit / delete a channel
  | "channel.test" // send test
  | "incident.annotate"
  | "incident.close" // manual close
  | "statuspage.write" // create / edit / publish
  | "member.manage" // invite / remove / change role
  | "apikey.manage" // create / revoke
  | "audit.view"
  | "org.settings"
  | "billing.view"
  | "billing.manage"
  | "org.transfer"
  | "org.delete";

const MIN_ROLE: Record<Action, Role> = {
  "monitor.write": "member",
  "monitor.test": "member",
  "channel.write": "member",
  "channel.test": "member",
  "incident.annotate": "member",
  "statuspage.write": "member",
  "incident.close": "admin",
  "member.manage": "admin",
  "apikey.manage": "admin",
  "audit.view": "admin",
  "org.settings": "admin",
  "billing.view": "admin",
  "billing.manage": "owner",
  "org.transfer": "owner",
  "org.delete": "owner",
};

// True if the role may perform the action. A null role (no active org) can do
// nothing org-scoped.
export function can(role: Role | null, action: Action): boolean {
  if (!role) return false;
  return RANK[role] >= RANK[MIN_ROLE[action]];
}
