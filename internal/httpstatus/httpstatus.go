// Package httpstatus maps an HTTP status code to readable text. It covers the
// IANA-registered codes through net/http, plus the common non-standard codes that
// real servers and proxies return (Cloudflare, nginx, AWS ELB, IIS), which
// net/http.StatusText leaves blank. Uptime monitoring hits arbitrary endpoints,
// many behind those proxies, so a monitored target can return a 5xx the stdlib does
// not name. Used wherever a code is shown to a person (the alert emails today).
package httpstatus

import "net/http"

// nonStandard holds vendor/proxy status codes that net/http.StatusText does not know.
// Text is the meaning each vendor documents for the code. Sources: Cloudflare and
// nginx status-code docs, AWS ELB docs.
var nonStandard = map[int]string{
	419: "Page Expired",
	420: "Enhance Your Calm",
	430: "Request Header Fields Too Large",
	440: "Login Time-out", // IIS
	444: "No Response",    // nginx
	449: "Retry With",     // IIS
	450: "Blocked by Windows Parental Controls",
	460: "Client Closed Connection",        // AWS ELB
	463: "Too Many Forwarded IP Addresses", // AWS ELB
	494: "Request Header Too Large",        // nginx
	495: "SSL Certificate Error",           // nginx
	496: "SSL Certificate Required",        // nginx
	497: "HTTP Request Sent to HTTPS Port", // nginx
	498: "Invalid Token",
	499: "Client Closed Request",                // nginx
	520: "Web Server Returned an Unknown Error", // Cloudflare
	521: "Web Server Is Down",                   // Cloudflare
	522: "Connection Timed Out",                 // Cloudflare
	523: "Origin Is Unreachable",                // Cloudflare
	524: "A Timeout Occurred",                   // Cloudflare
	525: "SSL Handshake Failed",                 // Cloudflare
	526: "Invalid SSL Certificate",              // Cloudflare
	527: "Railgun Error",                        // Cloudflare
	529: "Site Is Overloaded",
	530: "Origin DNS Error", // Cloudflare
	598: "Network Read Timeout Error",
	599: "Network Connect Timeout Error",
}

// Text returns the status code's reason text: the standard text from net/http when
// known, otherwise the common non-standard meaning, otherwise "" for a code nobody
// names (the caller then shows the bare number).
func Text(code int) string {
	if t := http.StatusText(code); t != "" {
		return t
	}
	return nonStandard[code]
}
