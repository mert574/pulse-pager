package notify

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
)

// Mailer is the transactional-email seam (RFC-003 2.6, PRD-001 6.1). The invite
// flow needs to send one tokenized accept link; it does not fit the alert-channel
// Event model (those are per-monitor incident notifications, fanned out to many
// channels). So invitations talk to this small interface instead. The api wires a
// real SMTP mailer when SMTP is configured, and a dev/no-op mailer that logs the
// accept URL when it is not, so the flow is complete and testable without a server.
type Mailer interface {
	Send(ctx context.Context, msg Mail) error
}

// Mail is one transactional message. Body is the plain-text part (always set, so
// there is a readable fallback and the token-carrying links stay greppable). HTML
// is the optional branded part; when it is set the message goes out as
// multipart/alternative and the client shows the HTML.
type Mail struct {
	To      string
	Subject string
	Body    string
	HTML    string
}

// LogMailer is the dev/no-op mailer: it logs the message (including the accept URL
// in the body) instead of sending it, so a developer can copy the link and the
// invite flow works end to end without an SMTP server. It is the default when no
// SMTP is configured. Never the production choice for a real deployment.
type LogMailer struct {
	Log *slog.Logger
}

// Send logs the mail at info level.
func (m LogMailer) Send(_ context.Context, msg Mail) error {
	if m.Log != nil {
		m.Log.Info("dev mailer: would send email", "to", msg.To, "subject", msg.Subject, "body", msg.Body)
	}
	return nil
}

// SMTPMailerConfig is the connection settings for the real transactional mailer.
// It mirrors the SMTP channel config but is read from app config, not a channel
// row, because transactional email is platform-level, not per-org.
type SMTPMailerConfig struct {
	Host     string
	Port     string
	Username string
	Password string
	From     string
	// TLSMode is starttls | implicit | none (defaults to starttls).
	TLSMode string
}

// NewMailerFromConfig picks the platform mailer from SMTP settings: a real SMTPMailer
// when host is set, else a LogMailer that logs the message (so a self-host without
// SMTP still completes the invite flow and the operator sees the link). Both the api
// (invitation email) and the notifier (the Team email channel) call it, so the choice
// lives in one place. It takes plain fields, not a config struct, so this package
// stays free of an internal/config import.
func NewMailerFromConfig(host, port, username, password, from, tlsMode string, log *slog.Logger) Mailer {
	if host == "" {
		if log != nil {
			log.Warn("no SMTP configured: platform emails will be logged, not sent")
		}
		return LogMailer{Log: log}
	}
	return NewSMTPMailer(SMTPMailerConfig{
		Host:     host,
		Port:     port,
		Username: username,
		Password: password,
		From:     from,
		TLSMode:  tlsMode,
	})
}

// SMTPMailer sends transactional email over the configured SMTP server. It reuses
// the same transport seam (smtpSend) as the SMTP alert channel, so TLS/STARTTLS
// and the test override behave identically.
type SMTPMailer struct {
	cfg SMTPMailerConfig
}

// NewSMTPMailer builds the real mailer. Port defaults to 587 and TLS to starttls.
func NewSMTPMailer(cfg SMTPMailerConfig) *SMTPMailer {
	if cfg.Port == "" {
		cfg.Port = "587"
	}
	if cfg.TLSMode == "" {
		cfg.TLSMode = "starttls"
	}
	return &SMTPMailer{cfg: cfg}
}

// Send delivers one message to a single recipient.
func (m *SMTPMailer) Send(ctx context.Context, msg Mail) error {
	if m.cfg.Host == "" {
		return fmt.Errorf("mailer: missing host")
	}
	if m.cfg.From == "" {
		return fmt.Errorf("mailer: missing from address")
	}
	if msg.To == "" {
		return fmt.Errorf("mailer: missing recipient")
	}
	to := []string{msg.To}
	var auth smtp.Auth
	if m.cfg.Username != "" {
		auth = smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)
	}
	addr := net.JoinHostPort(m.cfg.Host, m.cfg.Port)
	raw := buildMessage(m.cfg.From, to, msg.Subject, msg.Body, msg.HTML)
	return smtpSend(ctx, addr, m.cfg.Host, auth, m.cfg.From, to, raw, strings.EqualFold(m.cfg.TLSMode, "implicit"))
}
