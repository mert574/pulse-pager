package notify

import (
	"fmt"
	"strings"
)

// The five transactional/alert emails, each returning (subject, text, html). The
// text part is the plain-text fallback (kept as it was, so the multipart message
// always has a readable body and the token-carrying URLs stay greppable); the
// html part is the branded version built from RenderEmailHTML. The Mailer sends
// both as multipart/alternative and the client shows the html when it can.

// AlertEmail renders the down/recovery alert for a monitor event. The subject and
// text come from buildEmail (unchanged); the html is the branded card.
func AlertEmail(ev Event) (subject, text, html string) {
	subject, text = buildEmail(ev)
	html = alertEmailHTML(ev)
	return subject, text, html
}

// alertEmailHTML builds the HTML body for a down or recovery alert.
func alertEmailHTML(ev Event) string {
	name := ev.Monitor.Name

	if ev.EventType == EventRecovery {
		dur := 0
		if ev.DurationSeconds != nil {
			dur = *ev.DurationSeconds
		}
		rows := []EmailRow{
			{Label: "Monitor", Value: name},
			{Label: "URL", Value: ev.Monitor.URL},
			{Label: "Was down for", Value: humanDuration(dur)},
			{Label: "Down since", Value: humanTime(ev.Incident.StartedAt)},
		}
		if ev.Incident.EndedAt != nil {
			rows = append(rows, EmailRow{Label: "Recovered at", Value: humanTime(*ev.Incident.EndedAt)})
		}
		if ev.Check.StatusCode != nil {
			rows = append(rows, EmailRow{Label: "Status", Value: fmt.Sprintf("HTTP %d", *ev.Check.StatusCode)})
		}
		if ev.Check.LatencyMs != nil {
			rows = append(rows, EmailRow{Label: "Latency", Value: fmt.Sprintf("%dms", *ev.Check.LatencyMs)})
		}
		return RenderEmailHTML(EmailContent{
			Preheader: fmt.Sprintf("%s is back up after %s", name, humanDuration(dur)),
			Banner:    &EmailBanner{Label: "Recovered", Color: colorOK, BG: colorOKBG},
			Heading:   name + " is back up",
			Intro:     "Good news. Pulse Pager can reach this monitor again and the incident is closed.",
			Rows:      rows,
			Footer:    "You're getting this because a Pulse Pager alert channel is set to notify you.",
		})
	}

	rows := []EmailRow{
		{Label: "Monitor", Value: name},
		{Label: "URL", Value: ev.Monitor.URL},
	}
	if reason := failureReason(ev); reason != "" {
		rows = append(rows, EmailRow{Label: "Reason", Value: reason})
	}
	if ev.Check.StatusCode != nil {
		rows = append(rows, EmailRow{Label: "Status", Value: fmt.Sprintf("HTTP %d", *ev.Check.StatusCode)})
	}
	if ev.Check.LatencyMs != nil {
		rows = append(rows, EmailRow{Label: "Latency", Value: fmt.Sprintf("%dms", *ev.Check.LatencyMs)})
	}
	if ev.Check.ErrorText != nil && *ev.Check.ErrorText != "" {
		rows = append(rows, EmailRow{Label: "Error", Value: *ev.Check.ErrorText})
	}
	rows = append(rows, EmailRow{Label: "Down since", Value: humanTime(ev.Incident.StartedAt)})

	return RenderEmailHTML(EmailContent{
		Preheader: fmt.Sprintf("%s is down", name),
		Banner:    &EmailBanner{Label: "Down", Color: colorDown, BG: colorDownBG},
		Heading:   name + " is down",
		Intro:     "Pulse Pager just opened an incident for this monitor. Here's what we saw.",
		Rows:      rows,
		Footer:    "You're getting this because a Pulse Pager alert channel is set to notify you.",
	})
}

// failureReason returns just the failure reason string (no "Reason:" prefix or
// HTTP suffix; the status code is its own row in the HTML).
func failureReason(ev Event) string {
	if ev.Check.FailureReason != nil {
		return string(*ev.Check.FailureReason)
	}
	return ""
}

// TestEmail renders the "test message" a user sends to confirm a channel works.
// worksLine is the channel-specific confirmation ("the SMTP channel works", "the
// Team email channel works").
func TestEmail(channelName, worksLine string) (subject, text, html string) {
	subject = "[Pulse Pager] Test message"
	text = fmt.Sprintf("This is a test message from Pulse Pager for channel %q.\nIf you received this, %s.\n", channelName, worksLine)
	html = RenderEmailHTML(EmailContent{
		Preheader: "Test message from Pulse Pager",
		Banner:    &EmailBanner{Label: "Test", Color: colorTest, BG: colorTestBG},
		Heading:   "Your channel is working",
		Intro:     fmt.Sprintf("This is a test message from Pulse Pager for the channel %q. If you received this, %s and you're all set to get real alerts here.", channelName, worksLine),
		Footer:    "You triggered this test from your Pulse Pager notification settings.",
	})
	return subject, text, html
}

// InviteEmail renders the org invitation, localized to the invite locale (RFC-014
// 7/9). v1 ships English, German, and Spanish; an unknown locale falls back to
// English. The accept URL carries the raw token and is kept on its own line in the
// text part so the accept flow can read it back.
func InviteEmail(orgName, role, acceptURL, locale string) (subject, text, html string) {
	switch {
	case strings.HasPrefix(strings.ToLower(locale), "de"):
		subject = fmt.Sprintf("Du wurdest zu %s bei Pulse Pager eingeladen", orgName)
		text = fmt.Sprintf("Du wurdest eingeladen, %s als %s beizutreten.\n\nEinladung annehmen:\n%s\n\nDieser Link ist 7 Tage gultig.\n", orgName, role, acceptURL)
		html = RenderEmailHTML(EmailContent{
			Preheader: fmt.Sprintf("Du wurdest zu %s eingeladen", orgName),
			Heading:   fmt.Sprintf("Du wurdest zu %s eingeladen", orgName),
			Intro:     fmt.Sprintf("Du wurdest eingeladen, %s als %s bei Pulse Pager beizutreten.", orgName, role),
			Button:    &EmailButton{Label: "Einladung annehmen", URL: acceptURL},
			Note:      "Dieser Link ist 7 Tage gultig.",
			Footer:    "Wenn du diese Einladung nicht erwartet hast, kannst du diese E-Mail ignorieren.",
		})
		return subject, text, html
	case strings.HasPrefix(strings.ToLower(locale), "es"):
		subject = fmt.Sprintf("Te han invitado a %s en Pulse Pager", orgName)
		text = fmt.Sprintf("Te han invitado a unirte a %s como %s.\n\nAcepta la invitacion:\n%s\n\nEste enlace es valido durante 7 dias.\n", orgName, role, acceptURL)
		html = RenderEmailHTML(EmailContent{
			Preheader: fmt.Sprintf("Te han invitado a %s", orgName),
			Heading:   fmt.Sprintf("Te han invitado a %s", orgName),
			Intro:     fmt.Sprintf("Te han invitado a unirte a %s como %s en Pulse Pager.", orgName, role),
			Button:    &EmailButton{Label: "Aceptar invitacion", URL: acceptURL},
			Note:      "Este enlace es valido durante 7 dias.",
			Footer:    "Si no esperabas esta invitacion, puedes ignorar este correo.",
		})
		return subject, text, html
	}
	subject = fmt.Sprintf("You're invited to %s on Pulse Pager", orgName)
	text = fmt.Sprintf("You've been invited to join %s as %s.\n\nAccept the invitation:\n%s\n\nThis link is valid for 7 days.\n", orgName, role, acceptURL)
	html = RenderEmailHTML(EmailContent{
		Preheader: fmt.Sprintf("You're invited to join %s", orgName),
		Heading:   fmt.Sprintf("You're invited to %s", orgName),
		Intro:     fmt.Sprintf("You've been invited to join %s as %s on Pulse Pager. Accept below to get started.", orgName, role),
		Button:    &EmailButton{Label: "Accept invitation", URL: acceptURL},
		Note:      "This link is valid for 7 days.",
		Footer:    "If you weren't expecting this invitation, you can ignore this email.",
	})
	return subject, text, html
}

// MagicLinkEmail renders the passwordless sign-in email, localized to the locale
// (RFC-014). The verify URL carries the raw token and stays on its own line in the
// text part so the login flow can read it back.
func MagicLinkEmail(verifyURL, locale string) (subject, text, html string) {
	switch {
	case strings.HasPrefix(locale, "de"):
		subject = "Dein Anmeldelink fur Pulse Pager"
		text = "Klicke auf den Link, um dich bei Pulse Pager anzumelden:\n\n" + verifyURL + "\n\nDieser Link ist 15 Minuten gultig und kann nur einmal verwendet werden. Wenn du das nicht warst, ignoriere diese E-Mail.\n"
		html = RenderEmailHTML(EmailContent{
			Preheader: "Dein Anmeldelink fur Pulse Pager",
			Heading:   "Bei Pulse Pager anmelden",
			Intro:     "Klicke auf den Button, um dich bei Pulse Pager anzumelden.",
			Button:    &EmailButton{Label: "Anmelden", URL: verifyURL},
			Note:      "Dieser Link ist 15 Minuten gultig und kann nur einmal verwendet werden. Wenn du das nicht warst, ignoriere diese E-Mail.",
			Footer:    "Aus Sicherheitsgrunden gib diesen Link niemals weiter.",
		})
		return subject, text, html
	case strings.HasPrefix(locale, "es"):
		subject = "Tu enlace para iniciar sesion en Pulse Pager"
		text = "Haz clic en el enlace para iniciar sesion en Pulse Pager:\n\n" + verifyURL + "\n\nEste enlace es valido durante 15 minutos y solo se puede usar una vez. Si no fuiste tu, ignora este correo.\n"
		html = RenderEmailHTML(EmailContent{
			Preheader: "Tu enlace para iniciar sesion en Pulse Pager",
			Heading:   "Inicia sesion en Pulse Pager",
			Intro:     "Haz clic en el boton para iniciar sesion en Pulse Pager.",
			Button:    &EmailButton{Label: "Iniciar sesion", URL: verifyURL},
			Note:      "Este enlace es valido durante 15 minutos y solo se puede usar una vez. Si no fuiste tu, ignora este correo.",
			Footer:    "Por seguridad, nunca compartas este enlace con nadie.",
		})
		return subject, text, html
	}
	subject = "Your Pulse Pager sign-in link"
	text = "Click the link to sign in to Pulse Pager:\n\n" + verifyURL + "\n\nThis link is valid for 15 minutes and can only be used once. If this wasn't you, you can ignore this email.\n"
	html = RenderEmailHTML(EmailContent{
		Preheader: "Your Pulse Pager sign-in link",
		Heading:   "Sign in to Pulse Pager",
		Intro:     "Click the button below to sign in to Pulse Pager.",
		Button:    &EmailButton{Label: "Sign in", URL: verifyURL},
		Note:      "This link is valid for 15 minutes and can only be used once. If this wasn't you, you can ignore this email.",
		Footer:    "For your security, never share this link with anyone.",
	})
	return subject, text, html
}
