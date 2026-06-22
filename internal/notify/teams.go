package notify

import (
	"context"
	"fmt"
	"net/http"

	"pulse/internal/domain"
)

// teamsProvider posts an Adaptive Card to a Microsoft Teams incoming webhook.
//
// IMPORTANT: the old Office 365 Connector webhooks are retired (rollout completed
// May 2026). The supported path is a Power Automate "Workflows" incoming webhook
// ("Post to a channel when a webhook request is received"), which expects a
// message wrapper carrying an Adaptive Card, NOT a bare card and NOT the legacy
// MessageCard. The webhook URL is the workflow's generated trigger URL; auth is
// the sig query param baked into that URL.
//
// API verified June 2026 against:
//
//	https://learn.microsoft.com/en-us/microsoftteams/platform/webhooks-and-connectors/how-to/add-incoming-webhook
//	https://devblogs.microsoft.com/microsoft365dev/retirement-of-office-365-connectors-within-microsoft-teams/
type teamsProvider struct {
	client *http.Client
}

func (p *teamsProvider) setClient(c *http.Client) { p.client = c }

// The wrapper shape the Workflows trigger expects.
type teamsMessage struct {
	Type        string            `json:"type"` // "message"
	Attachments []teamsAttachment `json:"attachments"`
}

type teamsAttachment struct {
	ContentType string            `json:"contentType"` // application/vnd.microsoft.card.adaptive
	ContentURL  *string           `json:"contentUrl"`  // always null for inline cards
	Content     teamsAdaptiveCard `json:"content"`
}

type teamsAdaptiveCard struct {
	Schema  string           `json:"$schema"`
	Type    string           `json:"type"`    // "AdaptiveCard"
	Version string           `json:"version"` // "1.4"
	Body    []teamsTextBlock `json:"body"`
}

type teamsTextBlock struct {
	Type   string `json:"type"` // "TextBlock"
	Text   string `json:"text"`
	Wrap   bool   `json:"wrap"`
	Weight string `json:"weight,omitempty"`
	Size   string `json:"size,omitempty"`
}

func teamsCard(title, body string) teamsMessage {
	card := teamsAdaptiveCard{
		Schema:  "http://adaptivecards.io/schemas/adaptive-card.json",
		Type:    "AdaptiveCard",
		Version: "1.4",
		Body: []teamsTextBlock{
			{Type: "TextBlock", Text: title, Wrap: true, Weight: "Bolder", Size: "Medium"},
			{Type: "TextBlock", Text: body, Wrap: true},
		},
	}
	return teamsMessage{
		Type: "message",
		Attachments: []teamsAttachment{{
			ContentType: "application/vnd.microsoft.card.adaptive",
			ContentURL:  nil,
			Content:     card,
		}},
	}
}

func (p *teamsProvider) Send(ctx context.Context, cfg map[string]any, ev Event) error {
	url := cfgString(cfg, "webhook_url")
	if url == "" {
		return fmt.Errorf("teams: missing webhook_url")
	}
	title := summaryTitle(ev)
	body := plainBody(ev)
	if ev.Test {
		title = "Pulse Pager test message"
		body = testText(ev.ChannelName)
	}
	return postJSON(ctx, p.client, url, teamsCard(title, body), nil)
}

func (p *teamsProvider) Validate(cfg map[string]any) error {
	if cfgString(cfg, "webhook_url") == "" {
		return fmt.Errorf("teams: missing webhook_url")
	}
	return nil
}

func init() {
	Register(Descriptor{
		Type:        domain.ChannelTeams,
		DisplayName: "Microsoft Teams",
		Capability:  "channel.teams",
		ConfigFields: []ConfigField{
			{Key: "webhook_url", Label: "Workflow webhook URL", Type: FieldString, Required: true, Secret: true, Help: "Power Automate Workflows incoming-webhook URL (O365 connectors are retired)"},
		},
		Factory: func() Provider { return &teamsProvider{} },
	})
}
