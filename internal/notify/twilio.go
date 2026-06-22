package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"pulse/internal/domain"
)

// twilioProvider sends an SMS via the Twilio Messages API.
//
// API verified June 2026 against: https://www.twilio.com/docs/messaging/api/message-resource
// Endpoint: POST https://api.twilio.com/2010-04-01/Accounts/{AccountSid}/Messages.json
// HTTP Basic auth (username=AccountSid, password=AuthToken),
// Content-Type application/x-www-form-urlencoded, form fields To/From/Body.
// Twilio returns 201 Created on success.
type twilioProvider struct {
	client  *http.Client
	baseURL string // overridable in tests; empty => api.twilio.com
}

func (p *twilioProvider) setClient(c *http.Client) { p.client = c }

func (p *twilioProvider) url(accountSID string) string {
	base := p.baseURL
	if base == "" {
		base = "https://api.twilio.com"
	}
	return fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json", base, accountSID)
}

// smsBody keeps the message short: one line title plus the monitor url. SMS is
// length-limited and metered, so we do not include the full multi-line body.
func smsBody(ev Event) string {
	if ev.EventType == EventRecovery {
		return fmt.Sprintf("[Pulse Pager] RECOVERED: %s (%s)", ev.Monitor.Name, ev.Monitor.URL)
	}
	return fmt.Sprintf("[Pulse Pager] DOWN: %s (%s)", ev.Monitor.Name, ev.Monitor.URL)
}

func (p *twilioProvider) Send(ctx context.Context, cfg map[string]any, ev Event) error {
	accountSID := cfgString(cfg, "account_sid")
	authToken := cfgString(cfg, "auth_token")
	from := cfgString(cfg, "from")
	to := cfgString(cfg, "to")
	if accountSID == "" || authToken == "" {
		return fmt.Errorf("twilio: missing account_sid or auth_token")
	}
	if from == "" || to == "" {
		return fmt.Errorf("twilio: missing from or to number")
	}

	text := smsBody(ev)
	if ev.Test {
		text = fmt.Sprintf("[Pulse Pager] Test message for channel %q.", ev.ChannelName)
	}

	form := url.Values{}
	form.Set("To", to)
	form.Set("From", from)
	form.Set("Body", text)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url(accountSID), strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("twilio: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(accountSID, authToken)

	resp, err := httpClientOrDefault(p.client).Do(req)
	if err != nil {
		return fmt.Errorf("twilio: send: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("twilio: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (p *twilioProvider) Validate(cfg map[string]any) error {
	if cfgString(cfg, "account_sid") == "" || cfgString(cfg, "auth_token") == "" {
		return fmt.Errorf("twilio: missing account_sid or auth_token")
	}
	if cfgString(cfg, "from") == "" || cfgString(cfg, "to") == "" {
		return fmt.Errorf("twilio: missing from or to number")
	}
	return nil
}

func init() {
	Register(Descriptor{
		Type:        domain.ChannelTwilio,
		DisplayName: "SMS (Twilio)",
		Capability:  "channel.twilio",
		ConfigFields: []ConfigField{
			{Key: "account_sid", Label: "Account SID", Type: FieldString, Required: true, Help: "Twilio Account SID"},
			{Key: "auth_token", Label: "Auth token", Type: FieldString, Required: true, Secret: true, Help: "Twilio auth token"},
			{Key: "from", Label: "From number", Type: FieldString, Required: true, Help: "Twilio sender number (E.164)"},
			{Key: "to", Label: "To number", Type: FieldString, Required: true, Help: "recipient number (E.164)"},
		},
		Factory: func() Provider { return &twilioProvider{} },
	})
}
