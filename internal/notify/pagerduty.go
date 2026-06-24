package notify

import (
	"context"
	"fmt"
	"net/http"

	"pulse/internal/domain"
)

// pagerdutyProvider sends to the PagerDuty Events API v2 (Enqueue). A down event
// triggers an incident keyed by dedup_key = the Pulse incident id, and a recovery
// resolves the same dedup_key, so PagerDuty pairs them into one incident.
//
// API verified June 2026 against:
//
//	https://developer.pagerduty.com/api-reference/368ae3d938c9e-send-an-event-to-pager-duty
//	https://developer.pagerduty.com/docs/events-api-v2/trigger-events/
//
// Endpoint: POST https://events.pagerduty.com/v2/enqueue, application/json,
// routing_key in the body (no Authorization header).
type pagerdutyProvider struct {
	client   *http.Client
	endpoint string // overridable in tests
}

func (p *pagerdutyProvider) setClient(c *http.Client) { p.client = c }

const pagerdutyEnqueueURL = "https://events.pagerduty.com/v2/enqueue"

type pdPayload struct {
	Summary  string `json:"summary"`
	Source   string `json:"source"`
	Severity string `json:"severity"`
}

type pdEvent struct {
	RoutingKey  string     `json:"routing_key"`
	EventAction string     `json:"event_action"` // "trigger" | "resolve"
	DedupKey    string     `json:"dedup_key"`
	Payload     *pdPayload `json:"payload,omitempty"` // omitted on resolve
}

// pdDedupKey is the stable per-incident key. The same incident's down and
// recovery carry the same key, so PagerDuty resolves the alert it triggered.
func pdDedupKey(ev Event) string {
	return fmt.Sprintf("pulse-inc-%d", ev.Incident.ID)
}

func (p *pagerdutyProvider) url() string {
	if p.endpoint != "" {
		return p.endpoint
	}
	return pagerdutyEnqueueURL
}

func (p *pagerdutyProvider) Send(ctx context.Context, cfg map[string]any, ev Event) error {
	routingKey := cfgString(cfg, "routing_key")
	if routingKey == "" {
		return fmt.Errorf("pagerduty: missing routing_key")
	}

	body := pdEvent{RoutingKey: routingKey, DedupKey: pdDedupKey(ev)}
	if ev.EventType == EventRecovery && !ev.Test {
		body.EventAction = "resolve"
	} else {
		body.EventAction = "trigger"
		body.Payload = &pdPayload{
			Summary:  summaryTitle(ev) + " - " + ev.Monitor.URL,
			Source:   ev.Monitor.URL,
			Severity: pdSeverity(ev),
		}
	}
	// PagerDuty returns 202 on success, which postJSON accepts as 2xx.
	return postJSON(ctx, p.client, p.url(), body, nil)
}

// pdSeverity maps an event to a PagerDuty severity (critical|error|warning|info).
// A down is critical; everything else (test trigger) is info.
func pdSeverity(ev Event) string {
	if ev.EventType == EventDown {
		return "critical"
	}
	return "info"
}

func (p *pagerdutyProvider) Validate(cfg map[string]any) error {
	if cfgString(cfg, "routing_key") == "" {
		return fmt.Errorf("pagerduty: missing routing_key")
	}
	return nil
}

func init() {
	Register(Descriptor{
		Type:        domain.ChannelPagerDuty,
		DisplayName: "PagerDuty",
		Capability:  "channel.pagerduty",
		ConfigFields: []ConfigField{
			{Key: "routing_key", Label: "Integration (routing) key", Type: FieldString, Required: true, Secret: true, Help: "Events API v2 integration key"},
		},
		Factory: func() Provider { return &pagerdutyProvider{} },
	})
}
