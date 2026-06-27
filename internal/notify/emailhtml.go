package notify

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"strconv"
	"strings"
)

// HTML email rendering for every message the platform sends: the down/recovery
// alert mail, the channel test message, the org invite, and the magic-link sign-in.
// They all share one branded shell (assets/email.html.tmpl) so the look stays
// consistent and there is a single place to change the chrome. The template is an
// html/template, so every value is escaped for its context (monitor names, org
// names, and error text are user-controlled). The Go side here just fills an
// EmailContent and runs the template.

// emailLogoPNG is the Pulse Pager mark (the cream-badge "dark mode" variant, which
// reads well on the dark coffee header). It is embedded so every email carries its
// own logo as an inline attachment (see buildMessage): no remote image fetch, so it
// renders even when a client blocks remote content. emailLogoCID is the Content-ID
// the html <img src="cid:..."> and the attachment part agree on.
//
//go:embed assets/logo-email.png
var emailLogoPNG []byte

const emailLogoCID = "pulselogo"

//go:embed assets/email.html.tmpl
var emailTemplateSrc string

// emailTemplate is the branded shell, parsed once at startup. The funcs cover what
// the template context needs: raw emits the Outlook conditional comments
// (html/template would strip them), css passes a trusted color into a style
// attribute, and upper uppercases the status banner label.
var emailTemplate = template.Must(template.New("email").Funcs(template.FuncMap{
	"raw":   func(s string) template.HTML { return template.HTML(s) },
	"css":   func(s string) template.CSS { return template.CSS(s) },
	"upper": strings.ToUpper,
}).Parse(emailTemplateSrc))

// appBaseURL is the SPA origin (e.g. https://app.pulsepager.com), set once at
// startup with SetAppBaseURL. The alert/test emails use it to link the recipient to
// their channels page so they can change what notifies them. Empty (the default, and
// in tests) just drops the link; the rest of the email is unaffected. A package var
// set at boot follows the same shape as the registry and smtpSend seams.
var appBaseURL string

// SetAppBaseURL records the SPA origin used to build in-email links. The api and the
// notifier both call it at startup from config. Trailing slashes are trimmed so the
// joined paths never double up.
func SetAppBaseURL(u string) { appBaseURL = strings.TrimRight(u, "/") }

// channelsURL is the recipient's channels page for an org, or "" when the base URL
// is not configured (then the callout shows its text without a link). orgID is a
// string because org ids are moving to hex; today's int64 is formatted by the caller.
func channelsURL(orgID string) string {
	if appBaseURL == "" || orgID == "" {
		return ""
	}
	return appBaseURL + "/orgs/" + orgID + "/channels"
}

// incidentURL is the SPA page for one incident, or "" when the base URL or either id
// is missing (then the down alert shows no "view incident" button). The path mirrors
// the SPA route /orgs/:orgId/incidents/:id.
func incidentURL(orgID, incidentID int64) string {
	if appBaseURL == "" || orgID == 0 || incidentID == 0 {
		return ""
	}
	return appBaseURL + "/orgs/" + orgIDString(orgID) + "/incidents/" + strconv.FormatInt(incidentID, 10)
}

// orgIDString renders the current int64 org id as the string the email layer works
// with. It is the one spot to change when org ids become hex strings.
func orgIDString(orgID int64) string { return strconv.FormatInt(orgID, 10) }

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

// EmailBanner is the colored status pill at the top of the body. Tone selects the
// pill colors in the template ("down", "ok", or "test"); the template owns the
// actual hex values so the whole palette lives in one place.
type EmailBanner struct {
	Label string
	Tone  string
}

// EmailCallout is the "Why am I getting this?" box: a short explanation plus an
// optional link (e.g. "Manage your alert channels") so the recipient can change
// what notifies them. The link is dropped when LinkURL is empty.
type EmailCallout struct {
	Title     string
	Body      string
	LinkLabel string
	LinkURL   string
}

// EmailContent is everything the shell needs to render one branded HTML email.
// All string fields are plain text; the template escapes them.
type EmailContent struct {
	Preheader string        // hidden inbox preview line
	Banner    *EmailBanner  // optional status pill
	Heading   string        // big headline
	Intro     string        // a paragraph under the heading
	Rows      []EmailRow    // optional fact table
	Button    *EmailButton  // optional CTA
	Note      string        // optional small print under the body
	Callout   *EmailCallout // optional "why am I getting this?" box
	Footer    string        // context line in the footer ("you're getting this because...")
}

// emailView is the data the template renders: the content plus the inline logo's
// src. LogoSrc is a template.URL so the cid: scheme is not stripped by the URL
// filter (it is a trusted, fixed value, not user input).
type emailView struct {
	EmailContent
	LogoSrc template.URL
}

// RenderEmailHTML builds the full HTML document for one email. It is exported so the
// api package (invite, magic link) and a preview tool can reuse the same shell the
// notify providers use.
func RenderEmailHTML(c EmailContent) string {
	var b bytes.Buffer
	view := emailView{EmailContent: c, LogoSrc: template.URL("cid:" + emailLogoCID)}
	if err := emailTemplate.Execute(&b, view); err != nil {
		// The template is embedded and parsed at startup, so an execute error is a
		// programming bug, not a runtime condition. Surface it rather than send a
		// half-rendered email.
		return fmt.Sprintf("email render error: %v", err)
	}
	return b.String()
}
