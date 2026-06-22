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

// postJSON sends body as JSON to url with any extra headers, then checks for a
// 2xx response. It is shared by the Slack, Discord, webhook, and Teams providers.
func postJSON(ctx context.Context, client *http.Client, url string, body any, headers map[string]string) error {
	if url == "" {
		return fmt.Errorf("missing url")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := httpClientOrDefault(client).Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// slackProvider posts {"text": ...} to a Slack incoming webhook.
type slackProvider struct {
	client *http.Client
}

func (p *slackProvider) setClient(c *http.Client) { p.client = c }

type slackPayload struct {
	Text string `json:"text"`
}

func (p *slackProvider) Send(ctx context.Context, cfg map[string]any, ev Event) error {
	url := cfgString(cfg, "webhook_url")
	text := slackText(ev)
	if ev.Test {
		text = testText(ev.ChannelName)
	}
	return postJSON(ctx, p.client, url, slackPayload{Text: text}, nil)
}

func (p *slackProvider) Validate(cfg map[string]any) error { return nil }

// testText is the body of the "send test message" action, shared by the chat
// providers.
func testText(name string) string {
	return fmt.Sprintf(":wave: This is a test message from Pulse Pager for channel %q. If you can read this, the channel works.", name)
}

func init() {
	Register(Descriptor{
		Type:        domain.ChannelSlack,
		DisplayName: "Slack",
		Capability:  "channel.slack",
		ConfigFields: []ConfigField{
			{Key: "webhook_url", Label: "Webhook URL", Type: FieldString, Required: true, Secret: true, Help: "Slack incoming-webhook URL (https)"},
		},
		Factory: func() Provider { return &slackProvider{} },
	})
}
