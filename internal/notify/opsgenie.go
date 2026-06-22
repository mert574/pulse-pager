package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"pulse/internal/domain"
)

// opsgenieProvider sends to the Opsgenie Alert API. A down event creates an alert
// with alias = the Pulse incident id; a recovery closes that same alias, so the
// down and the recovery act on one alert.
//
// API verified June 2026 against: https://docs.opsgenie.com/docs/alert-api
// Auth header: "Authorization: GenieKey <api_key>".
// Create: POST {host}/v2/alerts
// Close:  POST {host}/v2/alerts/{alias}/close?identifierType=alias
// Host differs by region: us=https://api.opsgenie.com, eu=https://api.eu.opsgenie.com
type opsgenieProvider struct {
	client  *http.Client
	baseURL string // overridable in tests; empty => region-derived host
}

func (p *opsgenieProvider) setClient(c *http.Client) { p.client = c }

type opsgenieCreate struct {
	Message     string `json:"message"`
	Alias       string `json:"alias"`
	Description string `json:"description,omitempty"`
	Priority    string `json:"priority,omitempty"`
}

type opsgenieClose struct {
	Source string `json:"source,omitempty"`
	Note   string `json:"note,omitempty"`
}

// opsgenieHost returns the API host for the configured region (us default, eu).
func (p *opsgenieProvider) host(cfg map[string]any) string {
	if p.baseURL != "" {
		return p.baseURL
	}
	if cfgString(cfg, "region") == "eu" {
		return "https://api.eu.opsgenie.com"
	}
	return "https://api.opsgenie.com"
}

// opsgenieAlias is the stable per-incident alias (max 512 chars, well within).
func opsgenieAlias(ev Event) string {
	return fmt.Sprintf("pulse-inc-%d", ev.Incident.ID)
}

func (p *opsgenieProvider) Send(ctx context.Context, cfg map[string]any, ev Event) error {
	apiKey := cfgString(cfg, "api_key")
	if apiKey == "" {
		return fmt.Errorf("opsgenie: missing api_key")
	}
	host := p.host(cfg)
	alias := opsgenieAlias(ev)

	if ev.EventType == EventRecovery && !ev.Test {
		url := fmt.Sprintf("%s/v2/alerts/%s/close?identifierType=alias", host, alias)
		body := opsgenieClose{Source: "pulse", Note: "auto-resolved: monitor recovered"}
		return p.post(ctx, url, apiKey, body)
	}

	// create alert (message capped at 130 chars by Opsgenie).
	url := host + "/v2/alerts"
	body := opsgenieCreate{
		Message:     truncate(summaryTitle(ev), 130),
		Alias:       alias,
		Description: plainBody(ev),
		Priority:    "P1",
	}
	if ev.EventType != EventDown {
		body.Priority = "P3"
	}
	return p.post(ctx, url, apiKey, body)
}

// post sends a JSON body with the GenieKey auth header and checks for 2xx.
// Opsgenie returns 202 (accepted, async), which counts as success.
func (p *opsgenieProvider) post(ctx context.Context, url, apiKey string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("opsgenie: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("opsgenie: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "GenieKey "+apiKey)

	resp, err := httpClientOrDefault(p.client).Do(req)
	if err != nil {
		return fmt.Errorf("opsgenie: send: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("opsgenie: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (p *opsgenieProvider) Validate(cfg map[string]any) error {
	if cfgString(cfg, "api_key") == "" {
		return fmt.Errorf("opsgenie: missing api_key")
	}
	return nil
}

// truncate caps s at n runes.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func init() {
	Register(Descriptor{
		Type:        domain.ChannelOpsgenie,
		DisplayName: "Opsgenie",
		Capability:  "channel.opsgenie",
		ConfigFields: []ConfigField{
			{Key: "api_key", Label: "API key", Type: FieldString, Required: true, Secret: true, Help: "Opsgenie API integration key"},
			{Key: "region", Label: "Region", Type: FieldEnum, Required: false, Enum: []string{"us", "eu"}, Default: "us", Help: "Opsgenie account region (endpoint host)"},
		},
		Factory: func() Provider { return &opsgenieProvider{} },
	})
}
