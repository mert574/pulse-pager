package notify

import (
	"context"
	"net/http"

	"pulse/internal/domain"
)

// discordProvider posts {"content": ...} to a Discord incoming webhook. Discord
// returns 204 on success, which postJSON accepts as 2xx.
type discordProvider struct {
	client *http.Client
}

func (p *discordProvider) setClient(c *http.Client) { p.client = c }

type discordPayload struct {
	Content string `json:"content"`
}

func (p *discordProvider) Send(ctx context.Context, cfg map[string]any, ev Event) error {
	url := cfgString(cfg, "webhook_url")
	content := discordText(ev)
	if ev.Test {
		content = testText(ev.ChannelName)
	}
	return postJSON(ctx, p.client, url, discordPayload{Content: content}, nil)
}

func (p *discordProvider) Validate(cfg map[string]any) error { return nil }

func init() {
	Register(Descriptor{
		Type:        domain.ChannelDiscord,
		DisplayName: "Discord",
		Capability:  "channel.discord",
		ConfigFields: []ConfigField{
			{Key: "webhook_url", Label: "Webhook URL", Type: FieldString, Required: true, Secret: true, Help: "Discord incoming-webhook URL (https)"},
		},
		Factory: func() Provider { return &discordProvider{} },
	})
}
