// Command emailpreview renders every email Pulse Pager sends (down/recovery
// alerts, the channel test message, the org invite, and the magic-link sign-in)
// with realistic sample data, writes each one to an .html file you can open in a
// browser, and optionally sends them through an SMTP server so you can see how a
// real inbox renders them.
//
// It is a dev tool, not a service: make build does not build it, and it ships in
// no binary. Run it with:
//
//	go run ./cmd/emailpreview                       # write HTML files only
//	go run ./cmd/emailpreview -to you@example.com   # write files and send
//
// Sending reads the same PULSE_SMTP_* env the platform mailer uses (host, port,
// username, password, from, tls mode). With Resend that is host smtp.resend.com,
// username "resend", password your API key, from an address on a verified domain.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"pulse/internal/domain"
	"pulse/internal/notify"
)

func main() {
	outDir := flag.String("out", "/tmp/pulse-emails", "directory to write the rendered .html files into")
	to := flag.String("to", "", "send the rendered emails to this address (needs PULSE_SMTP_* env); empty = write files only")
	flag.Parse()

	// Set a sample SPA origin so the previews show the "manage your channels" link the
	// real alert/test emails carry.
	notify.SetAppBaseURL("https://app.pulsepager.com")

	samples := buildSamples()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir:", err)
		os.Exit(1)
	}

	for _, s := range samples {
		path := filepath.Join(*outDir, s.slug+".html")
		if err := os.WriteFile(path, []byte(s.html), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		fmt.Printf("wrote %s  (%s)\n", path, s.subject)
	}

	if *to == "" {
		fmt.Println("\nno -to address given, wrote files only. open them in a browser to preview.")
		return
	}

	mailer := notify.NewMailerFromConfig(
		os.Getenv("PULSE_SMTP_HOST"),
		os.Getenv("PULSE_SMTP_PORT"),
		os.Getenv("PULSE_SMTP_USERNAME"),
		os.Getenv("PULSE_SMTP_PASSWORD"),
		os.Getenv("PULSE_SMTP_FROM"),
		os.Getenv("PULSE_SMTP_TLS_MODE"),
		slog.New(slog.NewTextHandler(os.Stderr, nil)),
	)

	ctx := context.Background()
	for _, s := range samples {
		// Tag the subject so the reviewer can tell the samples apart in one inbox.
		subj := s.label + " - " + s.subject
		if err := mailer.Send(ctx, notify.Mail{To: *to, Subject: subj, Body: s.text, HTML: s.html}); err != nil {
			fmt.Fprintf(os.Stderr, "send %s: %v\n", s.slug, err)
			os.Exit(1)
		}
		fmt.Printf("sent %s to %s\n", s.slug, *to)
		// A small gap keeps a strict provider from rate-limiting a burst.
		time.Sleep(600 * time.Millisecond)
	}
	fmt.Printf("\nsent %d sample emails to %s\n", len(samples), *to)
}

type sample struct {
	slug    string
	label   string
	subject string
	text    string
	html    string
}

// sampleOrgID is the org the sample alert/test emails belong to; it makes the
// "manage your channels" link resolve to a realistic /orgs/{id}/channels path.
const sampleOrgID = 42

func buildSamples() []sample {
	var out []sample

	add := func(slug, label, subject, text, html string) {
		out = append(out, sample{slug: slug, label: label, subject: subject, text: text, html: html})
	}

	// 1) Down alert.
	subj, text, html := notify.AlertEmail(downEvent())
	add("01-alert-down", "Alert: down", subj, text, html)

	// 2) Recovery alert.
	subj, text, html = notify.AlertEmail(recoveryEvent())
	add("02-alert-recovery", "Alert: recovery", subj, text, html)

	// 3) Channel test message.
	subj, text, html = notify.TestEmail("Ops on-call", "the email channel works", sampleOrgID)
	add("03-test-message", "Channel test", subj, text, html)

	// 4) Org invite (English, German, Spanish).
	const inviter = "Jane Doe (jane@acme.com)"
	subj, text, html = notify.InviteEmail("Acme Inc", inviter, "admin", "https://app.pulsepager.com/invitations/inv_8f2c1a9b4d", "en")
	add("04-invite-en", "Invite (EN)", subj, text, html)
	subj, text, html = notify.InviteEmail("Acme Inc", inviter, "admin", "https://app.pulsepager.com/invitations/inv_8f2c1a9b4d", "de")
	add("05-invite-de", "Invite (DE)", subj, text, html)
	subj, text, html = notify.InviteEmail("Acme Inc", inviter, "admin", "https://app.pulsepager.com/invitations/inv_8f2c1a9b4d", "es")
	add("06-invite-es", "Invite (ES)", subj, text, html)

	// 5) Magic-link sign-in (English, German, Spanish). Sample request country (a
	// CF-IPCountry code) and device so the "requested from" / "device" section shows.
	const sampleCountry = "DE"
	const sampleUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
	subj, text, html = notify.MagicLinkEmail("https://app.pulsepager.com/auth/email/verify?token=ml_3a7d9e2f", "en", sampleCountry, sampleUA)
	add("07-magiclink-en", "Sign-in (EN)", subj, text, html)
	subj, text, html = notify.MagicLinkEmail("https://app.pulsepager.com/auth/email/verify?token=ml_3a7d9e2f", "de", sampleCountry, sampleUA)
	add("08-magiclink-de", "Sign-in (DE)", subj, text, html)
	subj, text, html = notify.MagicLinkEmail("https://app.pulsepager.com/auth/email/verify?token=ml_3a7d9e2f", "es", sampleCountry, sampleUA)
	add("09-magiclink-es", "Sign-in (ES)", subj, text, html)

	return out
}

func ptrInt(n int) *int { return &n }

func ptrReason(r domain.FailureReason) *domain.FailureReason { return &r }

func downEvent() notify.Event {
	started := time.Date(2026, 6, 24, 9, 12, 0, 0, time.UTC)
	return notify.Event{
		EventType:   notify.EventDown,
		OrgID:       sampleOrgID,
		ChannelName: "Ops on-call",
		Monitor: domain.Monitor{
			ID:     123,
			Name:   "Prod API health",
			URL:    "https://api.example.com/health",
			Method: "GET",
		},
		Incident: domain.Incident{ID: 456, StartedAt: started},
		Check: domain.CheckResult{
			CheckedAt:     started,
			Healthy:       false,
			FailureReason: ptrReason(domain.ReasonStatusMismatch),
			StatusCode:    ptrInt(503),
			LatencyMs:     ptrInt(842),
		},
		AvgLatencyMs: ptrInt(360),
		SentAt:       started,
	}
}

func recoveryEvent() notify.Event {
	started := time.Date(2026, 6, 24, 9, 12, 0, 0, time.UTC)
	ended := time.Date(2026, 6, 24, 9, 27, 0, 0, time.UTC)
	return notify.Event{
		EventType:   notify.EventRecovery,
		OrgID:       sampleOrgID,
		ChannelName: "Ops on-call",
		Monitor: domain.Monitor{
			ID:     123,
			Name:   "Prod API health",
			URL:    "https://api.example.com/health",
			Method: "GET",
		},
		Incident: domain.Incident{ID: 456, StartedAt: started, EndedAt: &ended},
		Check: domain.CheckResult{
			CheckedAt:  ended,
			Healthy:    true,
			StatusCode: ptrInt(200),
			LatencyMs:  ptrInt(96),
		},
		AvgLatencyMs:    ptrInt(104),
		DurationSeconds: ptrInt(900),
		SentAt:          ended,
	}
}
