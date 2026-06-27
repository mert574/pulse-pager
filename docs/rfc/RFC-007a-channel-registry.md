# RFC-007a - Channel registry (addendum to RFC-007)

Status: DRAFT
Author: Engineering (delivery/notifications)
Parent: `docs/rfc/RFC-007-notifier.md`
Product source of truth: `docs/prd/PRD-003-notifications.md` (channel types section 2, phased types section 8, plan-tier gating section 8, locked payloads section 4.3)

House style: no em-dashes.

## Why this exists

RFC-007 fixed delivery, dedup, and the four v1 channels. PRD-003 section 8 says new channel types (PagerDuty, Opsgenie, SMS, Telegram, Microsoft Teams) slot in behind the same attach model with no change to the alerting contract. The original `internal/notify` had a hardcoded `map[ChannelType]Notifier` and per-type knowledge of which fields were secret was spread across the code. This addendum replaces that with a descriptor-driven registry so adding a channel is one Provider plus one Descriptor plus a register call, and nothing generic is hardcoded per type.

## The model

Three pieces, all in `internal/notify`:

| Piece | What it is |
|-------|-----------|
| `Descriptor` | the static definition of a channel type: its `Type`, `DisplayName`, plan-gating `Capability`, its `ConfigFields`, and a `Factory` that builds a `Provider` |
| `ConfigField` | one config field: `Key`, `Label`, `Type` (string/int/bool/enum/stringlist), `Required`, `Secret`, `Enum`, `Default`, `Help`. This is the one source of truth for that field. |
| `Provider` | the delivery logic: `Send(ctx, cfg, ev)` (one attempt, no retry) and `Validate(cfg)` (semantic checks beyond schema presence) |
| `Registry` | `map[ChannelType]Descriptor` with `Register`, `Get`, `List`, `SecretKeys`, `ValidateConfig`, `AvailableFor` |

Each provider file calls the package-level `Register` from `init`, so `notify.Default()` returns a registry populated with every built-in provider. The `Manager` looks a provider up through the registry by channel type on each send (it no longer holds a hardcoded map), injects its `*http.Client` into providers that want one, and keeps the same retry, backoff, fan-out, and test-send behavior from RFC-007.

## Everything generic is derived from the descriptor

- **Which fields are secret** (encrypt at rest, redact on read): `Registry.SecretKeys(type)` reads the `Secret` flag off `ConfigFields`. The store and the API both read this list instead of carrying their own per-type knowledge. This matches the secret subfields RFC-007 section 3 fixed: slack/discord `webhook_url`, webhook `url` + `custom_headers`, smtp `password`, and now the phased types' keys below.
- **Validation**: `Registry.ValidateConfig(type, cfg)` runs the schema checks from the descriptor (required, type, enum) and then calls the provider's own `Validate` for semantic checks. There is no per-type validation branch outside the descriptor and the provider.
- **The available-types list for the UI**: `Registry.List()` and `Registry.AvailableFor(allowed)`.
- **Redaction metadata**: the same `SecretKeys` list drives the `*_set` redaction posture (PRD-003 section 2.3).

## Plan-gating is descriptor-driven and dependency-free

`internal/notify` does not import an entitlements package (no cycle). Each descriptor carries a `Capability` (e.g. `channel.pagerduty`) which is the stable name the billing side keys on. The caller maps the org's plan to a set of allowed channel types and passes it in:

- `Registry.AvailableFor(allowed map[ChannelType]bool)` returns the descriptors the plan includes, for the UI's "which types can this org add" list.
- `CheckAllowed(type, allowed)` is used at channel-create and at send time.

A plan downgrade simply changes the allowed set (the caller derives it from entitlements, cached upstream). A channel of a now-disallowed type is blocked on create and skipped-and-recorded on send, with no code change in this package. This is the PRD-003 section 8 enforcement posture, kept out of the notify package's dependency graph.

## How to add a new channel (the whole checklist)

1. Write a `Provider`: a struct with `Send(ctx, cfg, ev) error` and `Validate(cfg) error`. If it talks HTTP, give it a `client *http.Client` field and a `setClient` method so the Manager injects its client (and tests inject an httptest client).
2. Write a `Descriptor` for it: `Type`, `DisplayName`, `Capability`, `ConfigFields` (mark the secret fields `Secret: true`), and `Factory`.
3. Register it from `init` with `Register(Descriptor{...})`.

That is all. Secrets, redaction, schema validation, the UI type list, and plan-gating all flow from the descriptor automatically. No other file needs a per-type branch.

## Implemented providers and the API each uses

The v1 payloads (slack/discord/webhook/smtp) are unchanged and stay byte-for-byte as PRD-003 section 4.3. The phased providers were each verified against current API docs (June 2026):

| Type | Provider | Endpoint / mechanism | Secret fields | Doc verified |
|------|----------|----------------------|---------------|--------------|
| `slack` | chat webhook | POST incoming-webhook, `{"text":...}` | `webhook_url` | (locked, PRD-003 4.3.2) |
| `discord` | chat webhook | POST incoming-webhook, `{"content":...}` | `webhook_url` | (locked, PRD-003 4.3.3) |
| `webhook` | generic | POST locked JSON envelope + custom headers | `url`, `custom_headers` | (locked, PRD-003 4.3.1) |
| `smtp` | email | net/smtp, starttls/implicit/none | `password` | (locked, PRD-003 4.3.4) |
| `pagerduty` | Events API v2 Enqueue | `POST https://events.pagerduty.com/v2/enqueue`, `routing_key` in body, `event_action` trigger/resolve, `dedup_key` = incident id, severity critical on down | `routing_key` | https://developer.pagerduty.com/docs/events-api-v2/trigger-events/ |
| `opsgenie` | Alert API | create `POST {host}/v2/alerts` with `alias` = incident id; close `POST {host}/v2/alerts/{alias}/close?identifierType=alias`; auth `Authorization: GenieKey <key>`; host us/eu by region | `api_key` | https://docs.opsgenie.com/docs/alert-api |
| `telegram` | Bot API sendMessage | `POST https://api.telegram.org/bot<token>/sendMessage`, JSON `{chat_id,text}`, plain text | `bot_token` | https://core.telegram.org/bots/api#sendmessage |
| `teams` | Workflows incoming webhook | POST the message wrapper `{type:"message",attachments:[{contentType:"application/vnd.microsoft.card.adaptive",content:{AdaptiveCard 1.4}}]}`. The old O365 connector webhooks are retired (rollout completed May 2026); this uses the Power Automate Workflows path. | `webhook_url` | https://learn.microsoft.com/en-us/microsoftteams/platform/webhooks-and-connectors/how-to/add-incoming-webhook |
| `twilio` | Messages API (SMS) | `POST https://api.twilio.com/2010-04-01/Accounts/{sid}/Messages.json`, form-encoded `To/From/Body`, HTTP Basic (sid/token), short body | `auth_token` | https://www.twilio.com/docs/messaging/api/message-resource |

PagerDuty and Opsgenie pair a down and a recovery on one incident: down triggers/creates with `dedup_key`/`alias` = `pulse-inc-<incident id>`, recovery resolves/closes the same key, so the on-call tool shows one incident that opened and closed, not two unrelated ones.

## Notes and assumptions

- Teams: there is no single published JSON-schema reference for the Workflows webhook body; the `{type:"message",attachments:[...adaptive card...]}` shape is consistent across Microsoft's migration docs and the Workflows trigger template. Adaptive Card version is pinned at 1.4 (conservative; Teams supports higher). The webhook URL carries its own `sig` auth query param, so there is no separate auth header.
- PagerDuty: the interactive API reference is JS-rendered; the endpoint, `routing_key`-in-body, `event_action` values, and severity enum were confirmed from PagerDuty's official example code and docs. The resolve body (routing_key + event_action + dedup_key, no payload) is the documented standard.
- SMTP `tls` is the PRD-003 enum (`starttls`/`implicit`/`none`, default starttls). The provider also accepts the legacy bool form (true => implicit) so older configs keep working.
