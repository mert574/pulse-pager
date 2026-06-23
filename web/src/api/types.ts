// API wire types. These are aliases over the types generated from the OpenAPI
// spec (api/openapi/v1.yaml -> schema.d.ts via `npm run gen:api`), which is the
// single source of truth (RFC-012). Do not hand-edit shapes here: change the
// spec and regenerate. The aliases keep the existing import names stable across
// the app so call sites do not care that the shapes are now generated.

import type { components } from "./schema.js";

type Schemas = components["schemas"];

export type Status = Schemas["Status"];
export type CoverageStatus = Schemas["CoverageStatus"];
export type Method = Schemas["Method"];
export type ChannelType = Schemas["ChannelType"];
export type FailureReason = Schemas["FailureReason"];
export type CloseReason = Schemas["CloseReason"];
export type Role = Schemas["Role"];
export type Plan = Schemas["Plan"];
export type DownPolicy = Schemas["DownPolicy"];
export type ResultsRange = Schemas["ResultsRange"];

export type ApiErrorBody = Schemas["Error"];
export type OrgMembership = Schemas["OrgMembership"];
export type Me = Schemas["Me"];
export type AdminMetrics = Schemas["AdminMetrics"];
export type AdminPlanCount = Schemas["AdminPlanCount"];
export type AdminSignupPoint = Schemas["AdminSignupPoint"];
export type MeUpdate = Schemas["MeUpdate"];
export type Identity = Schemas["Identity"];
export type IdentityProviderName = Schemas["IdentityProviderName"];
export type OrgInput = Schemas["OrgInput"];
export type Entitlements = Schemas["Entitlements"];
export type PlanCatalogEntry = Schemas["PlanCatalogEntry"];
export type MonitorHeader = Schemas["MonitorHeader"];
export type Monitor = Schemas["Monitor"];
export type MonitorListItem = Schemas["MonitorListItem"];
export type MonitorInput = Schemas["MonitorInput"];
export type Channel = Schemas["Channel"];
export type ChannelInput = Schemas["ChannelInput"];
export type Member = Schemas["Member"];
export type MemberRoleUpdate = Schemas["MemberRoleUpdate"];
export type TransferOwnership = Schemas["TransferOwnership"];
export type ApiKey = Schemas["APIKey"];
export type ApiKeyInput = Schemas["APIKeyInput"];
export type ApiKeyCreated = Schemas["APIKeyCreated"];
export type Invitation = Schemas["Invitation"];
export type InvitationInput = Schemas["InvitationInput"];
export type InvitationState = Schemas["InvitationState"];
export type InvitationPreview = Schemas["InvitationPreview"];
export type LocalizedString = Schemas["LocalizedString"];
export type CatalogField = Schemas["CatalogField"];
export type ChannelTypeCatalogEntry = Schemas["ChannelTypeCatalogEntry"];
export type ChannelTypeCatalog = Schemas["ChannelTypeCatalog"];
export type CheckResult = Schemas["CheckResult"];
export type CheckNowAccepted = Schemas["CheckNowAccepted"];
export type RegionState = Schemas["RegionState"];
export type MonitorRegionStates = Schemas["MonitorRegionStates"];
export type Incident = Schemas["Incident"];
export type IncidentDetail = Schemas["IncidentDetail"];
export type IncidentAnnotation = Schemas["IncidentAnnotation"];
export type IncidentAnnotationInput = Schemas["IncidentAnnotationInput"];
export type FailureSnapshot = Schemas["FailureSnapshot"];

export type StatusPage = Schemas["StatusPage"];
export type StatusPageInput = Schemas["StatusPageInput"];
export type StatusPageState = Schemas["StatusPageState"];
export type StatusPageTheme = Schemas["StatusPageTheme"];
export type StatusPageMonitorEntry = Schemas["StatusPageMonitorEntry"];
export type StatusPagePublish = Schemas["StatusPagePublish"];
export type PublicStatusPage = Schemas["PublicStatusPage"];
export type PublicBanner = Schemas["PublicBanner"];
export type PublicMonitorStatus = Schemas["PublicMonitorStatus"];
export type PublicDisplayedMonitor = Schemas["PublicDisplayedMonitor"];
export type PublicUptime = Schemas["PublicUptime"];
export type PublicHistoryPoint = Schemas["PublicHistoryPoint"];
export type PublicIncident = Schemas["PublicIncident"];

// Generic cursor-page envelope. The spec has concrete PageCheckResult /
// PageIncident; this generic stays for ergonomic client signatures and is
// structurally identical to them.
export interface Page<T> {
  items: T[];
  next_cursor: string | null;
}
