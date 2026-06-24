package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/smtp"
	"strings"

	"pulse/internal/domain"
)

// smtpProvider sends a plain-text email per event over the configured SMTP server.
// PRD-003 calls this type "email"; the code id stays "smtp" to match
// domain.ChannelSMTP. The DisplayName is the user-facing "Email (SMTP)".
type smtpProvider struct{}

// smtpSend is the transport seam. Tests override it so the message building and
// the provider flow can run without a real SMTP server. It dials the server,
// does TLS or STARTTLS per useTLS, authenticates if auth is non-nil, and sends.
var smtpSend = realSMTPSend

func (p *smtpProvider) Send(ctx context.Context, cfg map[string]any, ev Event) error {
	if ev.Test {
		subject, text, html := TestEmail(ev.ChannelName, "the SMTP channel works")
		return p.deliver(ctx, cfg, subject, text, html)
	}
	subject, text, html := AlertEmail(ev)
	return p.deliver(ctx, cfg, subject, text, html)
}

func (p *smtpProvider) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return fmt.Errorf("smtp: missing host")
	}
	if cfgString(cfg, "from") == "" {
		return fmt.Errorf("smtp: missing from address")
	}
	if len(recipients(cfg)) == 0 {
		return fmt.Errorf("smtp: missing recipients")
	}
	return nil
}

// deliver builds the RFC 5322 message and hands it to the transport seam.
func (p *smtpProvider) deliver(ctx context.Context, cfg map[string]any, subject, body, html string) error {
	host := cfgString(cfg, "host")
	port := cfgString(cfg, "port")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	from := cfgString(cfg, "from")
	to := recipients(cfg)
	mode := tlsMode(cfg)

	if host == "" {
		return fmt.Errorf("smtp: missing host")
	}
	if port == "" {
		port = "587"
	}
	if from == "" {
		return fmt.Errorf("smtp: missing from address")
	}
	if len(to) == 0 {
		return fmt.Errorf("smtp: missing recipients")
	}

	var auth smtp.Auth
	if username != "" {
		auth = smtp.PlainAuth("", username, password, host)
	}

	addr := net.JoinHostPort(host, port)
	msg := buildMessage(from, to, subject, body, html)
	// implicit TLS wraps the connection from the start; starttls and none dial
	// plain first (none never upgrades, starttls upgrades after greeting).
	return smtpSend(ctx, addr, host, auth, from, to, msg, mode == "implicit")
}

// tlsMode reads the tls config as the PRD-003 enum (starttls|implicit|none). It
// also accepts the legacy bool form (true => implicit, false => starttls) so old
// configs keep working. Default is starttls.
func tlsMode(cfg map[string]any) string {
	raw, ok := cfg["tls"]
	if !ok || raw == nil {
		return "starttls"
	}
	switch t := raw.(type) {
	case string:
		switch t {
		case "implicit", "none", "starttls":
			return t
		case "true", "1", "yes":
			return "implicit"
		case "false", "0", "no", "":
			return "starttls"
		}
		return "starttls"
	case bool:
		if t {
			return "implicit"
		}
		return "starttls"
	default:
		return "starttls"
	}
}

// recipients reads the "to" config, accepting either a single string (comma
// separated allowed) or a list.
func recipients(cfg map[string]any) []string {
	raw, ok := cfg["to"]
	if !ok || raw == nil {
		return nil
	}
	var out []string
	switch t := raw.(type) {
	case string:
		for _, p := range strings.Split(t, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	case []string:
		for _, p := range t {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// buildEmail is a pure function that produces the subject and plain-text body for
// an event. It is tested directly.
func buildEmail(ev Event) (subject, body string) {
	name := ev.Monitor.Name
	var b strings.Builder

	if ev.EventType == EventRecovery {
		subject = fmt.Sprintf("[Pulse Pager] RECOVERED: %s", name)
		dur := 0
		if ev.DurationSeconds != nil {
			dur = *ev.DurationSeconds
		}
		fmt.Fprintf(&b, "Monitor %s has recovered.\n\n", name)
		fmt.Fprintf(&b, "URL: %s\n", ev.Monitor.URL)
		fmt.Fprintf(&b, "Was down for: %s\n", humanDuration(dur))
		fmt.Fprintf(&b, "Down since: %s\n", humanTime(ev.Incident.StartedAt))
		if ev.Incident.EndedAt != nil {
			fmt.Fprintf(&b, "Recovered at: %s\n", humanTime(*ev.Incident.EndedAt))
		}
		if ev.Check.StatusCode != nil {
			fmt.Fprintf(&b, "Status: HTTP %d\n", *ev.Check.StatusCode)
		}
		if ev.Check.LatencyMs != nil {
			fmt.Fprintf(&b, "Latency: %dms\n", *ev.Check.LatencyMs)
		}
		return subject, b.String()
	}

	subject = fmt.Sprintf("[Pulse Pager] DOWN: %s", name)
	fmt.Fprintf(&b, "Monitor %s is down.\n\n", name)
	fmt.Fprintf(&b, "URL: %s\n", ev.Monitor.URL)
	fmt.Fprintf(&b, "%s\n", reasonLine(ev))
	if ev.Check.LatencyMs != nil {
		fmt.Fprintf(&b, "Latency: %dms\n", *ev.Check.LatencyMs)
	}
	if ev.Check.ErrorText != nil && *ev.Check.ErrorText != "" {
		fmt.Fprintf(&b, "Error: %s\n", *ev.Check.ErrorText)
	}
	fmt.Fprintf(&b, "Down since: %s\n", humanTime(ev.Incident.StartedAt))
	return subject, b.String()
}

// multipartBoundary separates the text and html parts of an alternative message.
// A fixed, distinctive token is fine: it is long and random-looking enough that it
// will not appear in a body, which is all RFC 2046 requires.
const multipartBoundary = "----=_pulse_a1b2c3d4e5f60718"

// buildMessage wraps the subject and body in a minimal RFC 5322 message. With no
// html it is a single text/plain part (unchanged). With html it is a
// multipart/alternative carrying the plain text first and the HTML second, so a
// client that cannot render HTML still shows the text; both parts are base64 so a
// long HTML line can't trip the 998-char SMTP line limit.
func buildMessage(from string, to []string, subject, body, html string) []byte {
	var b strings.Builder
	// From display name is the product brand (RFC-017 2.6); the address is the
	// configured internal value, carried through unchanged.
	fmt.Fprintf(&b, "From: Pulse Pager <%s>\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")

	if html == "" {
		b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
		b.WriteString("\r\n")
		// Normalize line endings to CRLF for the body.
		b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
		return []byte(b.String())
	}

	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n", multipartBoundary)
	b.WriteString("\r\n")

	writePart := func(contentType, content string) {
		fmt.Fprintf(&b, "--%s\r\n", multipartBoundary)
		fmt.Fprintf(&b, "Content-Type: %s; charset=\"utf-8\"\r\n", contentType)
		b.WriteString("Content-Transfer-Encoding: base64\r\n")
		b.WriteString("\r\n")
		b.WriteString(base64Wrapped(content))
		b.WriteString("\r\n")
	}
	writePart("text/plain", body)
	writePart("text/html", html)
	fmt.Fprintf(&b, "--%s--\r\n", multipartBoundary)
	return []byte(b.String())
}

// base64Wrapped base64-encodes s and wraps it to 76-character lines with CRLF, as
// MIME expects for a base64 part.
func base64Wrapped(s string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	var b strings.Builder
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteString("\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	return b.String()
}

// realSMTPSend is the production transport. It dials the server, sets up TLS or
// STARTTLS, authenticates, and sends the message.
func realSMTPSend(ctx context.Context, addr, host string, auth smtp.Auth, from string, to []string, msg []byte, useTLS bool) error {
	var conn net.Conn
	var err error

	dialer := &net.Dialer{}
	if useTLS {
		// Implicit TLS: wrap the connection from the start.
		tlsConn, derr := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host})
		if derr != nil {
			return fmt.Errorf("smtp: dial tls: %w", derr)
		}
		conn = tlsConn
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return fmt.Errorf("smtp: dial: %w", err)
		}
	}

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp: new client: %w", err)
	}
	defer c.Close()

	// If we are not on implicit TLS, upgrade with STARTTLS when the server offers it.
	if !useTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
				return fmt.Errorf("smtp: starttls: %w", err)
			}
		}
	}

	if auth != nil {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(auth); err != nil {
				return fmt.Errorf("smtp: auth: %w", err)
			}
		}
	}

	if err := c.Mail(from); err != nil {
		return fmt.Errorf("smtp: mail from: %w", err)
	}
	for _, r := range to {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("smtp: rcpt to %s: %w", r, err)
		}
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: close data: %w", err)
	}
	return c.Quit()
}

func init() {
	Register(Descriptor{
		Type:        domain.ChannelSMTP,
		DisplayName: "Email (SMTP)",
		Capability:  "channel.smtp",
		ConfigFields: []ConfigField{
			{Key: "host", Label: "Host", Type: FieldString, Required: true, Help: "SMTP host"},
			{Key: "port", Label: "Port", Type: FieldInt, Required: true, Default: "587", Help: "1..65535, typically 587 or 465"},
			{Key: "username", Label: "Username", Type: FieldString, Required: false, Help: "SMTP auth user (blank = unauthenticated relay)"},
			{Key: "password", Label: "Password", Type: FieldString, Required: false, Secret: true, Help: "SMTP auth password"},
			{Key: "from", Label: "From", Type: FieldString, Required: true, Help: "sender address"},
			{Key: "to", Label: "To", Type: FieldStringList, Required: true, Help: "one or more recipient addresses"},
			{Key: "tls", Label: "TLS mode", Type: FieldEnum, Required: false, Enum: []string{"starttls", "implicit", "none"}, Default: "starttls", Help: "starttls is recommended; none is discouraged"},
		},
		Factory: func() Provider { return &smtpProvider{} },
	})
}
