package notify

import (
	"fmt"
	"math"
	"strings"
	"time"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"

	"pulse/internal/domain"
	"pulse/internal/httpstatus"
)

// Presentation helpers that turn the raw alert facts into the readable strings the
// HTML emails show: a status code with its meaning, a failure reason in plain words,
// a timestamp with a relative "ago", and a latency put next to the monitor's recent
// average. These are email-only (the chat/integration providers keep their terse
// machine-greppable lines in render.go).

// httpStatusText renders a status code with its reason text, e.g. "HTTP 503 Service
// Unavailable". Uses the httpstatus utility so non-standard proxy codes (Cloudflare
// 522, nginx 499) read too; a code nobody names drops the suffix.
func httpStatusText(code int) string {
	if txt := httpstatus.Text(code); txt != "" {
		return fmt.Sprintf("HTTP %d %s", code, txt)
	}
	return fmt.Sprintf("HTTP %d", code)
}

// reasonText is a plain-language explanation for each failure reason, a step up from
// the raw enum (status_mismatch -> "Unexpected status code"). An unknown value falls
// back to the enum with underscores turned into spaces and the first letter capped.
func reasonText(r domain.FailureReason) string {
	switch r {
	case domain.ReasonConnectionError:
		return "Couldn't connect to the server"
	case domain.ReasonTimeout:
		return "The request timed out"
	case domain.ReasonStatusMismatch:
		return "Unexpected status code"
	case domain.ReasonLatencyExceeded:
		return "Slower than the latency limit"
	case domain.ReasonBodyAssertion:
		return "Response body didn't match"
	case domain.ReasonBlockedTarget:
		return "Target blocked (private or disallowed address)"
	case domain.ReasonCertExpired:
		return "TLS certificate has expired"
	case domain.ReasonCertExpiringSoon:
		return "TLS certificate expiring soon"
	case domain.ReasonCertInvalid:
		return "TLS certificate is invalid"
	}
	s := strings.ReplaceAll(string(r), "_", " ")
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// relativeAgo renders how long before ref the time t was, short form: "just now",
// "5m ago", "2h ago", "3d ago". A t at or after ref reads as "just now".
func relativeAgo(t, ref time.Time) string {
	d := ref.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// timeWithRel pairs the absolute UTC timestamp with the relative "ago" computed
// against ref (the event's send time), e.g. "2026-06-24 09:12:00 UTC (15m ago)".
func timeWithRel(t, ref time.Time) string {
	return humanTime(t) + " (" + relativeAgo(t, ref) + ")"
}

// latencyWithAvg shows the check's latency and, when a recent average is known, how
// far off it is in plain percent: "842ms · 134% higher than the 7-day average
// (360ms)" or "96ms · 8% lower than the 7-day average (104ms)". With no average it is
// just the raw value.
func latencyWithAvg(latencyMs int, avgMs *int) string {
	if avgMs == nil || *avgMs <= 0 {
		return fmt.Sprintf("%dms", latencyMs)
	}
	pct := int(math.Round((float64(latencyMs) - float64(*avgMs)) / float64(*avgMs) * 100))
	switch {
	case pct > 0:
		return fmt.Sprintf("%dms · %d%% higher than the 7-day average (%dms)", latencyMs, pct, *avgMs)
	case pct < 0:
		return fmt.Sprintf("%dms · %d%% lower than the 7-day average (%dms)", latencyMs, -pct, *avgMs)
	default:
		return fmt.Sprintf("%dms · the same as the 7-day average (%dms)", latencyMs, *avgMs)
	}
}

// uaSummary reduces a User-Agent string to a short "Browser on OS" (e.g. "Chrome on
// macOS") for the sign-in email's "request location" lines. It is a best-effort match
// on the common tokens, no dependency; an unrecognized agent returns "".
func uaSummary(ua string) string {
	if ua == "" {
		return ""
	}
	var browser string
	switch {
	case strings.Contains(ua, "Edg"):
		browser = "Edge"
	case strings.Contains(ua, "OPR"), strings.Contains(ua, "Opera"):
		browser = "Opera"
	case strings.Contains(ua, "Firefox"):
		browser = "Firefox"
	case strings.Contains(ua, "Chrome"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari"):
		browser = "Safari"
	}
	var os string
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		os = "iOS"
	case strings.Contains(ua, "Android"):
		os = "Android"
	case strings.Contains(ua, "Mac OS X"), strings.Contains(ua, "Macintosh"):
		os = "macOS"
	case strings.Contains(ua, "Windows"):
		os = "Windows"
	case strings.Contains(ua, "Linux"):
		os = "Linux"
	}
	switch {
	case browser != "" && os != "":
		return browser + " on " + os
	case browser != "":
		return browser
	case os != "":
		return os
	}
	return ""
}

// countryName turns a two-letter country code (from Cloudflare's CF-IPCountry header)
// into an English country name, e.g. "DE" -> "Germany". Empty or unrecognized codes
// (including Cloudflare's "XX"/"T1" placeholders for unknown/Tor) return "".
func countryName(code string) string {
	if len(code) != 2 {
		return ""
	}
	region, err := language.ParseRegion(strings.ToUpper(code))
	if err != nil {
		return ""
	}
	name := display.English.Regions().Name(region)
	// ParseRegion accepts codes display has no name for; guard against echoing the code.
	if name == "" || name == strings.ToUpper(code) {
		return ""
	}
	return name
}

// signInRows builds the optional "where did this come from" fact rows for the sign-in
// email: the country the request came from (from Cloudflare's CF-IPCountry, the only
// geo we get for free, so it is country-level not a raw IP) and the device parsed from
// the user agent. Labels are passed in so the caller localizes them. With neither
// known it returns nil and the email shows no such section.
func signInRows(country, userAgent, fromLabel, deviceLabel, unknown string) []EmailRow {
	loc := countryName(country)
	device := uaSummary(userAgent)
	if loc == "" && device == "" {
		return nil
	}
	if device == "" {
		device = unknown
	}
	var rows []EmailRow
	if loc != "" {
		rows = append(rows, EmailRow{Label: fromLabel, Value: loc})
	}
	rows = append(rows, EmailRow{Label: deviceLabel, Value: device})
	return rows
}
