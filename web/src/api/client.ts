// Typed fetch wrapper for the Pulse v2 API. Auth rides httpOnly cookies, so every
// request sends credentials and JS never reads a token. The one thing JS does read
// is the non-httpOnly pulse_csrf cookie, echoed as X-CSRF-Token on unsafe methods
// (RFC-013 section 3.4, RFC-003 double-submit).
//
// On 401 the client does a single-flight refresh-then-retry-once: a 401 usually
// just means the short access token expired, so it POSTs /auth/refresh once (all
// concurrent 401s share that one refresh), then replays the original request once.
// If refresh fails, or the retry 401s again, it clears the session and goes to
// login. This is the one central place auth expiry is handled (RFC-013 section 3.3).

import { navigate } from "../router.js";
import { session } from "../state/session.js";
import type {
  AdminBilling,
  AdminMetrics,
  AdminOrg,
  AdminOrgPlanUpdate,
  AdminRefund,
  AdminRefundRequest,
  AdminSubscription,
  AdminSubscriptionCancel,
  ApiErrorBody,
  BillingCheckoutRequest,
  BillingRedirect,
  ApiKey,
  ApiKeyCreated,
  ApiKeyInput,
  Channel,
  ChannelInput,
  ChannelTypeCatalog,
  CheckNowAccepted,
  CheckResult,
  Entitlements,
  FailureSnapshot,
  Identity,
  IdentityProviderName,
  Incident,
  IncidentAnnotation,
  IncidentAnnotationInput,
  IncidentDetail,
  Invitation,
  InvitationInput,
  InvitationPreview,
  Me,
  Member,
  MemberRoleUpdate,
  MeUpdate,
  Monitor,
  MonitorInput,
  MonitorListItem,
  MonitorRegionStates,
  OrgInput,
  OrgMembership,
  Page,
  Payment,
  PlanCatalogEntry,
  ResultsRange,
  StatusPage,
  StatusPageInput,
  TransferOwnership,
} from "./types.js";

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly fields?: Record<string, string>;
  // interpolation values some error codes carry (e.g. seat-limit). The generated
  // Error schema does not model params, so we read them off the wire defensively.
  readonly params?: Record<string, unknown>;
  // the W3C trace id the client minted for this request (RFC-021 section 8). Shown
  // on error states so a user or support can quote it into the trace tooling.
  readonly traceId?: string;

  constructor(
    status: number,
    body: ApiErrorBody & { params?: Record<string, unknown> },
    traceId?: string,
  ) {
    super(body.message);
    this.name = "ApiError";
    this.status = status;
    this.code = body.code;
    if (body.fields) this.fields = body.fields;
    if (body.params) this.params = body.params;
    if (traceId) this.traceId = traceId;
  }
}

// Versioned API root. Both the dev proxy and nginx serve /api at the origin root,
// so paths are absolute and not base-path prefixed (RFC-013 section 2.4).
const API_V1 = "/api/v1";

// Read a cookie value by name. Only pulse_csrf is ever read here; the token
// cookies are httpOnly and unreadable.
function readCookie(name: string): string | null {
  const match = document.cookie.match(
    new RegExp(
      "(?:^|; )" + name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + "=([^;]*)",
    ),
  );
  return match ? decodeURIComponent(match[1]) : null;
}

const UNSAFE_METHODS = new Set(["POST", "PUT", "PATCH", "DELETE"]);

interface RequestOptions {
  method?: string;
  body?: unknown;
  query?: Record<string, string | number | undefined>;
  // internal: skip the 401 refresh-retry (used by the refresh/login calls
  // themselves so they never recurse)
  noRetry?: boolean;
}

function buildQuery(query?: RequestOptions["query"]): string {
  if (!query) return "";
  const params = new URLSearchParams();
  for (const [k, v] of Object.entries(query)) {
    if (v !== undefined && v !== "") params.set(k, String(v));
  }
  const s = params.toString();
  return s ? `?${s}` : "";
}

// Single shared refresh promise. If ten requests 401 at once, they all await this
// one /auth/refresh rather than firing ten parallel refreshes (RFC-013 section 3.3
// single-flight rule). Reset to null once it settles so the next expiry refreshes
// again.
let refreshInFlight: Promise<boolean> | null = null;

function refreshOnce(): Promise<boolean> {
  if (!refreshInFlight) {
    refreshInFlight = doRefresh().finally(() => {
      refreshInFlight = null;
    });
  }
  return refreshInFlight;
}

async function doRefresh(): Promise<boolean> {
  try {
    const res = await fetch("/auth/refresh", {
      method: "POST",
      credentials: "include",
      headers: csrfHeader("POST"),
    });
    return res.ok;
  } catch {
    return false;
  }
}

function csrfHeader(method: string): Record<string, string> {
  if (!UNSAFE_METHODS.has(method.toUpperCase())) return {};
  const token = readCookie("pulse_csrf");
  return token ? { "X-CSRF-Token": token } : {};
}

// Mint a W3C traceparent so the trace starts at this request (RFC-021 section 5):
// a random 16-byte trace id and 8-byte span id as hex, sampled flag on. No SDK is
// needed for the id; the api continues it across every service, and the client keeps
// the trace id to show on errors. crypto.getRandomValues is always present here.
function newTraceparent(): string {
  const hex = (bytes: number): string => {
    const buf = new Uint8Array(bytes);
    crypto.getRandomValues(buf);
    return Array.from(buf, (b) => b.toString(16).padStart(2, "0")).join("");
  };
  return `00-${hex(16)}-${hex(8)}-01`;
}

// Build the RequestInit for a call: cookies always sent, JSON body when present,
// CSRF echoed on unsafe methods.
function buildInit(opts: RequestOptions, traceparent: string): RequestInit {
  const method = opts.method ?? "GET";
  const init: RequestInit = {
    method,
    credentials: "include",
    headers: { ...csrfHeader(method), traceparent },
  };
  if (opts.body !== undefined) {
    init.headers = { ...init.headers, "Content-Type": "application/json" };
    init.body = JSON.stringify(opts.body);
  }
  return init;
}

async function parseError(res: Response, traceId: string): Promise<ApiError> {
  let body: ApiErrorBody;
  try {
    const parsed = (await res.json()) as { error?: ApiErrorBody };
    body = parsed.error ?? {
      code: "internal_error",
      message: `Request failed with status ${res.status}`,
    };
  } catch {
    body = {
      code: "internal_error",
      message: `Request failed with status ${res.status}`,
    };
  }
  return new ApiError(res.status, body, traceId);
}

async function parseBody<T>(res: Response): Promise<T> {
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  if (!text) return undefined as T;
  return JSON.parse(text) as T;
}

function failToLogin(): never {
  session.clear();
  navigate("/login");
  throw new ApiError(401, {
    code: "unauthenticated",
    message: "Your session expired. Please sign in again.",
  });
}

async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const url = path + buildQuery(opts.query);
  // Mint one traceparent for the whole call and reuse it on the refresh-retry, so
  // the user action is one trace (RFC-021 section 5). The trace id is the second
  // field; we keep it to attach to any error we throw.
  const traceparent = newTraceparent();
  const traceId = traceparent.split("-")[1];
  let res = await fetch(url, buildInit(opts, traceparent));

  if (res.status === 401 && !opts.noRetry) {
    // single-flight refresh, then replay the original request exactly once
    const refreshed = await refreshOnce();
    if (!refreshed) failToLogin();
    res = await fetch(url, buildInit(opts, traceparent));
    if (res.status === 401) failToLogin();
  }

  if (res.status === 401) failToLogin();
  if (!res.ok) throw await parseError(res, traceId);
  return parseBody<T>(res);
}

// Org-scoped path helper. Active org is the URL path under /orgs/{orgId}
// (RFC-013 section 4.1); callers pass the orgId derived from the active route.
// Ids are opaque url-safe strings (RFC-012 id codec), so they go straight into
// the path with no escaping, the same as the nav links and the router. Resource
// ids in the per-method paths below follow the same rule.
function org(orgId: string, path: string): string {
  return `${API_V1}/orgs/${orgId}${path}`;
}

export const client = {
  // --- account-scoped (not org-prefixed) ---

  // logged-in == /me 200 (RFC-013 section 3.2). noRetry on the bootstrap call is
  // not set: a 401 here legitimately tries one refresh (a returning user with an
  // expired access token but a live refresh token), then falls to login.
  me(): Promise<Me> {
    return request<Me>(`${API_V1}/me`);
  },
  // PATCH /me: name/locale/timezone, returns the updated Me (RFC-013 section 9.3).
  updateMe(input: MeUpdate): Promise<Me> {
    return request<Me>(`${API_V1}/me`, { method: "PATCH", body: input });
  },
  // The social identities linked to the signed-in user (google/github).
  listIdentities(): Promise<Identity[]> {
    return request<Identity[]>(`${API_V1}/me/identities`);
  },
  // Unlink one provider. The server returns 409 when it is the last identity,
  // surfaced as an ApiError the account view turns into a localized message.
  unlinkIdentity(provider: IdentityProviderName): Promise<void> {
    return request<void>(`${API_V1}/me/identities/${provider}`, {
      method: "DELETE",
    });
  },
  // The orgs the user belongs to. /me already carries this list; this is for a
  // fresh pull after a membership change.
  listOrgs(): Promise<OrgMembership[]> {
    return request<OrgMembership[]>(`${API_V1}/orgs`);
  },
  // Create an org with the caller as owner; returns the new membership so the
  // caller can switch to it without a full /me re-fetch.
  createOrg(input: OrgInput): Promise<OrgMembership> {
    return request<OrgMembership>(`${API_V1}/orgs`, {
      method: "POST",
      body: input,
    });
  },
  getOrg(orgId: string): Promise<OrgMembership> {
    return request<OrgMembership>(`${API_V1}/orgs/${orgId}`);
  },
  logout(): Promise<void> {
    return request<void>("/auth/logout", { method: "POST", noRetry: true });
  },

  // Passwordless email login: ask the server to email a one-time sign-in link
  // (RFC-003). The server is enumeration-safe, so this always resolves the same way
  // whether or not the email has an account; the caller just shows a neutral "check
  // your email" message. noRetry because there is no session to refresh here.
  startEmailLogin(email: string): Promise<void> {
    return request<void>("/auth/email/start", {
      method: "POST",
      body: { email },
      noRetry: true,
    });
  },
  logoutAll(): Promise<void> {
    return request<void>(`${API_V1}/account/logout-all`, { method: "POST" });
  },

  // --- org-scoped ---

  entitlements(orgId: string): Promise<Entitlements> {
    return request<Entitlements>(org(orgId, "/entitlements"));
  },

  // The public plan catalog (tiers and their limits). Reference config, not
  // per-org data, so it is not org-prefixed; the billing view renders it as the
  // comparison/upgrade table without hardcoding the tiers.
  listPlans(): Promise<PlanCatalogEntry[]> {
    return request<PlanCatalogEntry[]>(`${API_V1}/plans`);
  },

  // Platform-wide totals for the operator admin panel (not org-scoped). The
  // server gates on the PULSE_PLATFORM_ADMINS allowlist and 403s a non-admin.
  getAdminMetrics(): Promise<AdminMetrics> {
    return request<AdminMetrics>(`${API_V1}/admin/metrics`);
  },
  // Every org with its plan, for the admin plan editor. Same allowlist gate.
  listAdminOrgs(): Promise<AdminOrg[]> {
    return request<AdminOrg[]>(`${API_V1}/admin/orgs`);
  },
  // Cross-org billing summary (paid orgs, subscription statuses, revenue).
  getAdminBilling(): Promise<AdminBilling> {
    return request<AdminBilling>(`${API_V1}/admin/billing`);
  },
  // Move an org's plan (RFC-018 5.1). plan is required; the rest is for a paid move
  // or a Custom price (cycle, mode, custom_amount/custom_currency). The server only
  // calls the provider when the org has a subscription; otherwise it is an override.
  setAdminOrgPlan(
    orgId: string,
    body: AdminOrgPlanUpdate,
  ): Promise<AdminOrg> {
    return request<AdminOrg>(`${API_V1}/admin/orgs/${orgId}/plan`, {
      method: "PUT",
      body,
    });
  },
  // Cancel an org's subscription (RFC-018 5.2). Default period_end on the server.
  cancelAdminOrgSubscription(
    orgId: string,
    when?: AdminSubscriptionCancel["when"],
  ): Promise<AdminSubscription> {
    return request<AdminSubscription>(
      `${API_V1}/admin/orgs/${orgId}/subscription/cancel`,
      { method: "POST", body: { when } },
    );
  },
  // Refund a payment (RFC-018 5.3). Omit amount for a full refund.
  refundAdminOrgPayment(
    orgId: string,
    body: AdminRefundRequest,
  ): Promise<AdminRefund> {
    return request<AdminRefund>(`${API_V1}/admin/orgs/${orgId}/refund`, {
      method: "POST",
      body,
    });
  },

  // Self-serve billing (RFC-018 6): start a hosted checkout to buy a paid plan, or
  // open the customer portal. Both return a provider URL the caller redirects to.
  createBillingCheckout(
    orgId: string,
    body: BillingCheckoutRequest,
  ): Promise<BillingRedirect> {
    return request<BillingRedirect>(org(orgId, "/billing/checkout"), {
      method: "POST",
      body,
    });
  },
  createBillingPortal(orgId: string): Promise<BillingRedirect> {
    return request<BillingRedirect>(org(orgId, "/billing/portal"), {
      method: "POST",
    });
  },
  // The org's mirrored payments (invoices) for the billing screen (RFC-018 4).
  listBillingPayments(orgId: string): Promise<Payment[]> {
    return request<Payment[]>(org(orgId, "/billing/payments"));
  },

  listMonitors(orgId: string): Promise<MonitorListItem[]> {
    return request<MonitorListItem[]>(org(orgId, "/monitors"));
  },
  getMonitor(orgId: string, id: string): Promise<Monitor> {
    return request<Monitor>(org(orgId, `/monitors/${id}`));
  },
  createMonitor(orgId: string, input: MonitorInput): Promise<Monitor> {
    return request<Monitor>(org(orgId, "/monitors"), {
      method: "POST",
      body: input,
    });
  },
  updateMonitor(
    orgId: string,
    id: string,
    input: MonitorInput,
  ): Promise<Monitor> {
    return request<Monitor>(org(orgId, `/monitors/${id}`), {
      method: "PUT",
      body: input,
    });
  },
  deleteMonitor(orgId: string, id: string): Promise<void> {
    return request<void>(org(orgId, `/monitors/${id}`), { method: "DELETE" });
  },
  // Trigger an on-demand check. The server enqueues one probe per region and
  // returns 202 right away with every region in "scheduled"; the per-region
  // progress shows up via getMonitorRegionStates (poll). 409 when a check is
  // already running, the monitor is off, or within the per-monitor cooldown;
  // 429 when over the manual-check rate limit (Retry-After header + fields).
  checkNow(orgId: string, id: string): Promise<CheckNowAccepted> {
    return request<CheckNowAccepted>(org(orgId, `/monitors/${id}/check`), {
      method: "POST",
    });
  },
  // The current per-region live states for the org's monitors, keyed by monitor
  // id. Pass a monitorId to scope it to one monitor; omit it for the whole list.
  // The list and detail views poll this every few seconds to show live progress.
  getMonitorRegionStates(
    orgId: string,
    monitorId?: string,
  ): Promise<MonitorRegionStates> {
    return request<MonitorRegionStates>(org(orgId, "/monitor-region-states"), {
      query: { monitor_id: monitorId },
    });
  },
  listResults(
    orgId: string,
    id: string,
    range: ResultsRange,
    cursor?: string,
    region?: string,
  ): Promise<Page<CheckResult>> {
    return request<Page<CheckResult>>(org(orgId, `/monitors/${id}/results`), {
      query: { range, cursor, region },
    });
  },
  listMonitorIncidents(
    orgId: string,
    id: string,
    cursor?: string,
  ): Promise<Page<Incident>> {
    return request<Page<Incident>>(org(orgId, `/monitors/${id}/incidents`), {
      query: { cursor },
    });
  },
  // The captured response of the most recent failed check, or null when none has
  // been captured (404). The headers and body are the monitored endpoint's own
  // response, i.e. attacker-controlled, so the view must render them as text only.
  async lastFailure(
    orgId: string,
    id: string,
  ): Promise<FailureSnapshot | null> {
    try {
      return await request<FailureSnapshot>(
        org(orgId, `/monitors/${id}/last-failure`),
      );
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) return null;
      throw err;
    }
  },

  // The channel-type catalog drives the picker and config forms: which types the
  // org's plan includes, and each type's config-field schema (RFC-007a, PRD-006).
  getChannelTypes(orgId: string): Promise<ChannelTypeCatalog> {
    return request<ChannelTypeCatalog>(org(orgId, "/channel-types"));
  },
  listChannels(orgId: string): Promise<Channel[]> {
    return request<Channel[]>(org(orgId, "/channels"));
  },
  createChannel(orgId: string, input: ChannelInput): Promise<Channel> {
    return request<Channel>(org(orgId, "/channels"), {
      method: "POST",
      body: input,
    });
  },
  updateChannel(
    orgId: string,
    id: string,
    input: ChannelInput,
  ): Promise<Channel> {
    return request<Channel>(org(orgId, `/channels/${id}`), {
      method: "PUT",
      body: input,
    });
  },
  deleteChannel(orgId: string, id: string): Promise<void> {
    return request<void>(org(orgId, `/channels/${id}`), { method: "DELETE" });
  },
  testChannel(orgId: string, id: string): Promise<void> {
    return request<void>(org(orgId, `/channels/${id}/test`), {
      method: "POST",
    });
  },

  // The org's incidents across all monitors, newest first (PRD-002 4). status
  // "open" filters to currently-open incidents; "all" (or omitted) returns all.
  listIncidents(
    orgId: string,
    status?: "open" | "all",
    cursor?: string,
  ): Promise<Page<Incident>> {
    return request<Page<Incident>>(org(orgId, "/incidents"), {
      query: { status, cursor },
    });
  },
  // One incident with its annotation timeline (any member may view).
  getIncident(orgId: string, id: string): Promise<IncidentDetail> {
    return request<IncidentDetail>(org(orgId, `/incidents/${id}`));
  },
  // Add a note to an incident (member+). Returns the created annotation so the
  // detail view can append it without a full reload.
  addIncidentAnnotation(
    orgId: string,
    id: string,
    note: string,
  ): Promise<IncidentAnnotation> {
    return request<IncidentAnnotation>(
      org(orgId, `/incidents/${id}/annotations`),
      {
        method: "POST",
        body: { note } satisfies IncidentAnnotationInput,
      },
    );
  },
  // Manually close an open incident (owner/admin only). The server returns 409
  // when it is already closed, surfaced as an ApiError the view localizes.
  closeIncident(orgId: string, id: string): Promise<IncidentDetail> {
    return request<IncidentDetail>(org(orgId, `/incidents/${id}/close`), {
      method: "POST",
    });
  },

  // --- status pages (PRD-004, RFC-013 section 8) ---

  // The org's status pages, draft and published (member+ to manage, any member
  // may read). Returns the editor projection with branding + displayed monitors.
  listStatusPages(orgId: string): Promise<StatusPage[]> {
    return request<StatusPage[]>(org(orgId, "/status-pages"));
  },
  getStatusPage(orgId: string, id: string): Promise<StatusPage> {
    return request<StatusPage>(org(orgId, `/status-pages/${id}`));
  },
  // Create a status page. The server returns 402 status_page_limit_reached when
  // the plan's cap is reached, which the form surfaces as an inline upsell.
  createStatusPage(orgId: string, input: StatusPageInput): Promise<StatusPage> {
    return request<StatusPage>(org(orgId, "/status-pages"), {
      method: "POST",
      body: input,
    });
  },
  // Update a page and its displayed monitors; display_monitors fully replaces the
  // displayed list.
  updateStatusPage(
    orgId: string,
    id: string,
    input: StatusPageInput,
  ): Promise<StatusPage> {
    return request<StatusPage>(org(orgId, `/status-pages/${id}`), {
      method: "PUT",
      body: input,
    });
  },
  deleteStatusPage(orgId: string, id: string): Promise<void> {
    return request<void>(org(orgId, `/status-pages/${id}`), {
      method: "DELETE",
    });
  },
  // Publish or unpublish a page. Publishing makes the public URL resolve;
  // unpublishing returns it to draft. Returns the updated page so the list can
  // reflect the new state without a re-fetch.
  publishStatusPage(
    orgId: string,
    id: string,
    published: boolean,
  ): Promise<StatusPage> {
    return request<StatusPage>(org(orgId, `/status-pages/${id}/publish`), {
      method: "PUT",
      body: { published },
    });
  },

  // --- members + invitations (PRD-001, RFC-003) ---

  // The org's members with their role and joined-at. Read access is any member;
  // mutations below are admin/owner and the server re-checks each one.
  listMembers(orgId: string): Promise<Member[]> {
    return request<Member[]>(org(orgId, "/members"));
  },
  // PATCH a member's role. The server rejects an admin trying to set owner, and
  // any role change it does not allow, with a 403/409 the view surfaces.
  changeMemberRole(
    orgId: string,
    userId: string,
    role: MemberRoleUpdate["role"],
  ): Promise<Member> {
    return request<Member>(org(orgId, `/members/${userId}`), {
      method: "PATCH",
      body: { role } satisfies MemberRoleUpdate,
    });
  },
  // Remove another member from the org.
  removeMember(orgId: string, userId: string): Promise<void> {
    return request<void>(org(orgId, `/members/${userId}`), {
      method: "DELETE",
    });
  },
  // Leave the org yourself. The server returns 409 when the caller is the last
  // owner, surfaced as an ApiError the view turns into a localized message.
  leaveOrg(orgId: string): Promise<void> {
    return request<void>(org(orgId, "/members/me"), { method: "DELETE" });
  },
  // Hand ownership to another member (owner only). step_down makes the acting
  // owner an admin; omitted keeps them a co-owner.
  transferOwnership(orgId: string, input: TransferOwnership): Promise<void> {
    return request<void>(org(orgId, "/transfer-ownership"), {
      method: "POST",
      body: input,
    });
  },

  listInvitations(orgId: string): Promise<Invitation[]> {
    return request<Invitation[]>(org(orgId, "/invitations"));
  },
  // Invite an email at a role. The server may reject with a seat-limit error
  // (code contains "seat", with params) or a 403, surfaced by the view.
  createInvitation(orgId: string, input: InvitationInput): Promise<Invitation> {
    return request<Invitation>(org(orgId, "/invitations"), {
      method: "POST",
      body: input,
    });
  },
  // Revoke a pending invitation, freeing its reserved seat.
  revokeInvitation(orgId: string, id: string): Promise<void> {
    return request<void>(org(orgId, `/invitations/${id}`), {
      method: "DELETE",
    });
  },
  // Re-send the invitation email. Returns the (re-dated) invitation.
  resendInvitation(orgId: string, id: string): Promise<Invitation> {
    return request<Invitation>(org(orgId, `/invitations/${id}/resend`), {
      method: "POST",
    });
  },

  // --- api keys (PRD-001 App A, owner/admin only) ---

  // The org's API keys, no secret on any of them (the secret is shown once at
  // create time and never returned again). Server 403s a member/viewer.
  listApiKeys(orgId: string): Promise<ApiKey[]> {
    return request<ApiKey[]>(org(orgId, "/api-keys"));
  },
  // Create a key at member or admin role (never owner; the server rejects it).
  // The response carries the full secret exactly once; the view shows it then
  // drops it, and the list never includes it.
  createApiKey(
    orgId: string,
    name: string,
    role: ApiKeyInput["role"],
  ): Promise<ApiKeyCreated> {
    return request<ApiKeyCreated>(org(orgId, "/api-keys"), {
      method: "POST",
      body: { name, role } satisfies ApiKeyInput,
    });
  },
  // Revoke a key by id. It stops working right away.
  revokeApiKey(orgId: string, id: string): Promise<void> {
    return request<void>(org(orgId, `/api-keys/${id}`), { method: "DELETE" });
  },

  // --- invitation accept (token-scoped, not org-prefixed) ---

  // Pre-login preview of an invitation: the org name, role and inviter. No session
  // required so the accept page can render before sign-in.
  getInvitationPreview(token: string): Promise<InvitationPreview> {
    return request<InvitationPreview>(`${API_V1}/invitations/${token}`);
  },
  // Accept the invitation. Needs a session whose verified email matches the
  // invited email; returns the new membership so the caller can land in the org.
  acceptInvitation(token: string): Promise<OrgMembership> {
    return request<OrgMembership>(`${API_V1}/invitations/${token}/accept`, {
      method: "POST",
    });
  },
};

// Exposed for the auth-interceptor unit test (RFC-013 section 11): lets a test
// reach the same request() path the client methods use, with a mocked fetch.
export const __test = { request };
