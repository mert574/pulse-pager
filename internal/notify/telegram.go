package notify

import (
	"context"
	"fmt"
	"net/http"

	"pulse/internal/domain"
)

// telegramProvider sends via the Telegram Bot API sendMessage method.
//
// API verified June 2026 against: https://core.telegram.org/bots/api#sendmessage
// Endpoint: POST https://api.telegram.org/bot<token>/sendMessage (the literal
// "bot" prefix joins the token, no separator). JSON body with chat_id and text.
// We send plain text (no parse_mode) so monitor names/urls never break MarkdownV2
// escaping rules.
type telegramProvider struct {
	client  *http.Client
	baseURL string // overridable in tests; empty => api.telegram.org
}

func (p *telegramProvider) setClient(c *http.Client) { p.client = c }

type telegramMessage struct {
	ChatID string `json:"chat_id"`
	Text   string `json:"text"`
}

func (p *telegramProvider) url(token string) string {
	base := p.baseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	return fmt.Sprintf("%s/bot%s/sendMessage", base, token)
}

func (p *telegramProvider) Send(ctx context.Context, cfg map[string]any, ev Event) error {
	token := cfgString(cfg, "bot_token")
	chatID := cfgString(cfg, "chat_id")
	if token == "" {
		return fmt.Errorf("telegram: missing bot_token")
	}
	if chatID == "" {
		return fmt.Errorf("telegram: missing chat_id")
	}

	text := summaryTitle(ev) + "\n" + plainBody(ev)
	if ev.Test {
		text = testText(ev.ChannelName)
	}
	return postJSON(ctx, p.client, p.url(token), telegramMessage{ChatID: chatID, Text: text}, nil)
}

func (p *telegramProvider) Validate(cfg map[string]any) error {
	if cfgString(cfg, "bot_token") == "" {
		return fmt.Errorf("telegram: missing bot_token")
	}
	if cfgString(cfg, "chat_id") == "" {
		return fmt.Errorf("telegram: missing chat_id")
	}
	return nil
}

func init() {
	Register(Descriptor{
		Type:        domain.ChannelTelegram,
		DisplayName: "Telegram",
		Capability:  "channel.telegram",
		ConfigFields: []ConfigField{
			{Key: "bot_token", Label: "Bot token", Type: FieldString, Required: true, Secret: true, Help: "Telegram bot token from BotFather"},
			{Key: "chat_id", Label: "Chat ID", Type: FieldString, Required: true, Help: "numeric chat id or @channelusername"},
		},
		Factory: func() Provider { return &telegramProvider{} },
	})
}
