package notify

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"pulse/internal/domain"
)

// webhookProvider posts the fixed JSON envelope (PRD-003 4.3.1) to a generic URL,
// plus any custom headers configured on the channel.
type webhookProvider struct {
	client *http.Client
}

func (p *webhookProvider) setClient(c *http.Client) { p.client = c }

// The envelope structs mirror PRD-003 4.3.1 field-for-field. Pointers and
// omitempty are used so nullable fields encode as JSON null or are absent as the
// spec asks.

type webhookMonitor struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	Method string `json:"method"`
}

type webhookIncident struct {
	ID        string  `json:"id"`
	StartedAt string  `json:"started_at"`
	EndedAt   *string `json:"ended_at"` // null on down, set on recovery
}

type webhookCheck struct {
	CheckedAt     string  `json:"checked_at"`
	Healthy       bool    `json:"healthy"`
	FailureReason *string `json:"failure_reason"` // null on recovery
	StatusCode    *int    `json:"status_code"`    // nullable
	LatencyMs     *int    `json:"latency_ms"`     // nullable
	Error         *string `json:"error"`          // nullable
}

type webhookEnvelope struct {
	Event           string          `json:"event"`
	Monitor         webhookMonitor  `json:"monitor"`
	Incident        webhookIncident `json:"incident"`
	Check           webhookCheck    `json:"check"`
	DurationSeconds *int            `json:"duration_seconds,omitempty"` // recovery only
	SentAt          string          `json:"sent_at"`
}

// rfc3339UTC renders t as an RFC3339 string in UTC (machine format).
func rfc3339UTC(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// buildEnvelope turns an Event into the PRD-003 4.3.1 webhook envelope. It is a
// pure function so it can be tested directly.
func buildEnvelope(ev Event) webhookEnvelope {
	env := webhookEnvelope{
		Event: ev.EventType,
		Monitor: webhookMonitor{
			ID:     fmt.Sprintf("mon_%d", ev.Monitor.ID),
			Name:   ev.Monitor.Name,
			URL:    ev.Monitor.URL,
			Method: string(ev.Monitor.Method),
		},
		Incident: webhookIncident{
			ID:        fmt.Sprintf("inc_%d", ev.Incident.ID),
			StartedAt: rfc3339UTC(ev.Incident.StartedAt),
		},
		Check: webhookCheck{
			CheckedAt:  rfc3339UTC(ev.Check.CheckedAt),
			Healthy:    ev.Check.Healthy,
			StatusCode: ev.Check.StatusCode,
			LatencyMs:  ev.Check.LatencyMs,
			Error:      ev.Check.ErrorText,
		},
		SentAt: rfc3339UTC(ev.SentAt),
	}

	if ev.Incident.EndedAt != nil {
		s := rfc3339UTC(*ev.Incident.EndedAt)
		env.Incident.EndedAt = &s
	}

	if ev.Check.FailureReason != nil {
		r := string(*ev.Check.FailureReason)
		env.Check.FailureReason = &r
	}

	if ev.EventType == EventRecovery {
		env.DurationSeconds = ev.DurationSeconds
	}

	return env
}

// customHeaders reads the optional "custom_headers" map from the channel config.
// It also accepts the legacy "headers" key for backward compatibility.
func customHeaders(cfg map[string]any) map[string]string {
	raw, ok := cfg["custom_headers"]
	if !ok || raw == nil {
		raw, ok = cfg["headers"]
	}
	if !ok || raw == nil {
		return nil
	}
	out := map[string]string{}
	switch m := raw.(type) {
	case map[string]string:
		for k, v := range m {
			out[k] = v
		}
	case map[string]any:
		for k, v := range m {
			if s, ok := v.(string); ok {
				out[k] = s
			} else {
				out[k] = fmt.Sprintf("%v", v)
			}
		}
	default:
		return nil
	}
	return out
}

func (p *webhookProvider) Send(ctx context.Context, cfg map[string]any, ev Event) error {
	url := cfgString(cfg, "url")
	if ev.Test {
		// Send a minimal but valid envelope so the receiver sees a real request.
		now := time.Now()
		ev = Event{
			EventType: EventDown,
			Monitor: domain.Monitor{
				Name:   "Pulse Pager test monitor",
				URL:    "https://example.com",
				Method: "GET",
			},
			Incident: domain.Incident{StartedAt: now},
			Check:    domain.CheckResult{CheckedAt: now, Healthy: false},
			SentAt:   now,
		}
	}
	return postJSON(ctx, p.client, url, buildEnvelope(ev), customHeaders(cfg))
}

func (p *webhookProvider) Validate(cfg map[string]any) error { return nil }

func init() {
	Register(Descriptor{
		Type:        domain.ChannelWebhook,
		DisplayName: "Webhook",
		Capability:  "channel.webhook",
		ConfigFields: []ConfigField{
			{Key: "url", Label: "URL", Type: FieldString, Required: true, Secret: true, Help: "absolute https URL the envelope is POSTed to"},
			{Key: "custom_headers", Label: "Custom headers", Type: FieldStringList, Required: false, Secret: true, Help: "optional headers sent with each request; values treated as secret"},
		},
		Factory: func() Provider { return &webhookProvider{} },
	})
}
