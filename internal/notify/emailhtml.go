package notify

import (
	"fmt"
	"html"
	"strings"
)

// HTML email rendering for every message the platform sends: the down/recovery
// alert mail, the channel test message, the org invite, and the magic-link
// sign-in. They all share one branded shell (RenderEmailHTML) so the look stays
// consistent and there is a single place to change the chrome.
//
// Everything is built for email clients, not browsers: tables for layout, inline
// styles only (Gmail strips <style> and <head>), a 600px centered card, and
// bulletproof table-wrapped buttons so Outlook renders them. No external images
// (the logo is drawn from text + the caramel "ping" dot), so nothing is blocked
// or broken when remote content is off. Dynamic values are HTML-escaped, since
// monitor names, org names, and error text are user-controlled.
//
// The palette is the product's coffee/latte brand, taken from the web theme and
// the logo: dark coffee #5b4231, cream #f7efe0, caramel #d98a4f.
const (
	colorPageBG     = "#ece2d2" // warm latte page background
	colorCardBG     = "#fffdf9" // near-white cream card
	colorHeaderBG   = "#5b4231" // dark coffee (the logo badge)
	colorHeaderText = "#f7efe0" // cream wordmark on the coffee header
	colorAccent     = "#d98a4f" // caramel (the logo ping dot)
	colorText       = "#4a3526" // body copy, dark coffee
	colorHeading    = "#3a2a1d" // headings, darkest coffee
	colorDim        = "#8a7257" // muted labels and footer
	colorBorder     = "#ece0cc" // hairline between rows
	colorRowBG      = "#faf5ec" // fact-table zebra tint
	colorButtonBG   = "#5b4231" // primary CTA, coffee
	colorButtonText = "#f7efe0" // CTA label, cream

	// Status banner colors (warm-tinted so they sit in the palette).
	colorDown   = "#c0392b"
	colorDownBG = "#fbeae7"
	colorOK     = "#1f9254"
	colorOKBG   = "#e9f6ee"
	colorTest   = "#9a5a34"
	colorTestBG = "#f6ecdd"

	fontStack = "-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif"
)

// EmailButton is an optional call-to-action rendered as a bulletproof button.
type EmailButton struct {
	Label string
	URL   string
}

// EmailRow is one label/value fact line in the body (e.g. "URL" / the address).
type EmailRow struct {
	Label string
	Value string
}

// EmailBanner is the colored status pill at the top of the body (alerts/test).
type EmailBanner struct {
	Label string // e.g. "DOWN", "RECOVERED", "TEST"
	Color string // text/dot color
	BG    string // pill background
}

// EmailContent is everything the shell needs to render one branded HTML email.
// All string fields are plain text; RenderEmailHTML escapes them.
type EmailContent struct {
	Preheader string       // hidden inbox preview line
	Banner    *EmailBanner // optional status pill
	Heading   string       // big headline
	Intro     string       // a paragraph under the heading
	Rows      []EmailRow   // optional fact table
	Button    *EmailButton // optional CTA
	Note      string       // optional small print under the body
	Footer    string       // context line in the footer ("you're getting this because...")
}

// RenderEmailHTML builds the full HTML document for one email. It is exported so
// the api package (invite, magic link) and a preview tool can reuse the same
// shell the notify providers use.
func RenderEmailHTML(c EmailContent) string {
	var b strings.Builder

	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<meta name="color-scheme" content="light"></head>`)
	b.WriteString(fmt.Sprintf(`<body style="margin:0;padding:0;background:%s;">`, colorPageBG))

	// Hidden preheader: sets the inbox preview text without showing in the body.
	if c.Preheader != "" {
		b.WriteString(fmt.Sprintf(
			`<div style="display:none;max-height:0;overflow:hidden;opacity:0;mso-hide:all;">%s</div>`,
			esc(c.Preheader)))
	}

	// Outer full-width table centers the card on the latte background.
	b.WriteString(fmt.Sprintf(
		`<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" border="0" style="background:%s;">`,
		colorPageBG))
	b.WriteString(`<tr><td align="center" style="padding:32px 16px;">`)

	// The card is fluid up to 600px: width:100% + max-width caps it on phones, and
	// an Outlook-only ghost table caps it on desktop Outlook (which ignores
	// max-width). So it fills a narrow screen and centers at 600px everywhere else.
	b.WriteString(`<!--[if mso]><table role="presentation" width="600" align="center" cellpadding="0" cellspacing="0" border="0"><tr><td><![endif]-->`)
	b.WriteString(fmt.Sprintf(
		`<table role="presentation" align="center" width="100%%" cellpadding="0" cellspacing="0" border="0" style="width:100%%;max-width:600px;background:%s;border-radius:16px;overflow:hidden;box-shadow:0 1px 3px rgba(58,42,29,0.08);">`,
		colorCardBG))

	// Header: coffee bar with the cream wordmark and the caramel ping dot.
	b.WriteString(fmt.Sprintf(`<tr><td style="background:%s;padding:26px 32px;text-align:center;">`, colorHeaderBG))
	b.WriteString(fmt.Sprintf(
		`<span style="font-family:%s;font-size:20px;font-weight:700;letter-spacing:-0.01em;color:%s;">`+
			`<span style="color:%s;font-size:22px;line-height:1;">&#9679;</span>&nbsp;Pulse Pager</span>`,
		fontStack, colorHeaderText, colorAccent))
	b.WriteString(`</td></tr>`)

	// Body.
	b.WriteString(`<tr><td style="padding:32px;">`)

	if c.Banner != nil {
		b.WriteString(renderBanner(*c.Banner))
	}

	if c.Heading != "" {
		b.WriteString(fmt.Sprintf(
			`<h1 style="margin:0 0 12px;font-family:%s;font-size:22px;line-height:1.3;font-weight:700;color:%s;">%s</h1>`,
			fontStack, colorHeading, esc(c.Heading)))
	}

	if c.Intro != "" {
		b.WriteString(fmt.Sprintf(
			`<p style="margin:0 0 20px;font-family:%s;font-size:15px;line-height:1.6;color:%s;">%s</p>`,
			fontStack, colorText, esc(c.Intro)))
	}

	if len(c.Rows) > 0 {
		b.WriteString(renderRows(c.Rows))
	}

	if c.Button != nil {
		b.WriteString(renderButton(*c.Button))
	}

	if c.Note != "" {
		b.WriteString(fmt.Sprintf(
			`<p style="margin:20px 0 0;font-family:%s;font-size:13px;line-height:1.6;color:%s;">%s</p>`,
			fontStack, colorDim, esc(c.Note)))
	}

	b.WriteString(`</td></tr>`)

	// Footer.
	b.WriteString(fmt.Sprintf(`<tr><td style="padding:22px 32px;border-top:1px solid %s;">`, colorBorder))
	if c.Footer != "" {
		b.WriteString(fmt.Sprintf(
			`<p style="margin:0 0 6px;font-family:%s;font-size:12px;line-height:1.5;color:%s;">%s</p>`,
			fontStack, colorDim, esc(c.Footer)))
	}
	b.WriteString(fmt.Sprintf(
		`<p style="margin:0;font-family:%s;font-size:12px;line-height:1.5;color:%s;">`+
			`Pulse Pager &middot; open-source uptime monitoring &middot; `+
			`<a href="https://pulsepager.com" style="color:%s;text-decoration:none;">pulsepager.com</a></p>`,
		fontStack, colorDim, colorAccent))
	b.WriteString(`</td></tr>`)

	b.WriteString(`</table>`) // card
	b.WriteString(`<!--[if mso]></td></tr></table><![endif]-->`)
	b.WriteString(`</td></tr></table>`)
	b.WriteString(`</body></html>`)
	return b.String()
}

// renderBanner draws the colored status pill (DOWN / RECOVERED / TEST).
func renderBanner(bn EmailBanner) string {
	return fmt.Sprintf(
		`<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 20px;">`+
			`<tr><td style="background:%s;border-radius:999px;padding:7px 16px;font-family:%s;">`+
			`<span style="color:%s;font-size:13px;">&#9679;</span>`+
			`<span style="color:%s;font-size:13px;font-weight:700;letter-spacing:0.08em;">&nbsp;%s</span>`+
			`</td></tr></table>`,
		bn.BG, fontStack, bn.Color, bn.Color, esc(strings.ToUpper(bn.Label)))
}

// renderRows draws the label/value fact table used by the alert emails.
func renderRows(rows []EmailRow) string {
	var b strings.Builder
	b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 4px;">`)
	for i, r := range rows {
		bg := ""
		if i%2 == 1 {
			bg = fmt.Sprintf("background:%s;", colorRowBG)
		}
		b.WriteString(`<tr>`)
		b.WriteString(fmt.Sprintf(
			`<td style="%spadding:11px 12px;font-family:%s;font-size:13px;color:%s;white-space:nowrap;vertical-align:top;border-radius:6px 0 0 6px;">%s</td>`,
			bg, fontStack, colorDim, esc(r.Label)))
		b.WriteString(fmt.Sprintf(
			`<td style="%spadding:11px 12px;font-family:%s;font-size:14px;font-weight:500;color:%s;word-break:break-word;vertical-align:top;border-radius:0 6px 6px 0;">%s</td>`,
			bg, fontStack, colorText, esc(r.Value)))
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</table>`)
	return b.String()
}

// renderButton draws a bulletproof, table-wrapped CTA so Outlook renders it.
func renderButton(btn EmailButton) string {
	return fmt.Sprintf(
		`<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin:26px 0 4px;">`+
			`<tr><td align="center" bgcolor="%s" style="border-radius:10px;">`+
			`<a href="%s" target="_blank" style="display:inline-block;padding:14px 30px;font-family:%s;font-size:15px;font-weight:600;line-height:1;color:%s;text-decoration:none;border-radius:10px;">%s</a>`+
			`</td></tr></table>`,
		colorButtonBG, escAttr(btn.URL), fontStack, colorButtonText, esc(btn.Label))
}

// esc escapes text for HTML body context.
func esc(s string) string { return html.EscapeString(s) }

// escAttr escapes a value for an attribute (href). EscapeString already handles
// the quote and angle brackets that matter here.
func escAttr(s string) string { return html.EscapeString(s) }
