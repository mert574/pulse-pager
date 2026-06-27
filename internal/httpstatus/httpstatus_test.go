package httpstatus

import "testing"

func TestText(t *testing.T) {
	cases := map[int]string{
		200:    "OK",                    // stdlib
		503:    "Service Unavailable",   // stdlib
		522:    "Connection Timed Out",  // Cloudflare, not in stdlib
		499:    "Client Closed Request", // nginx, not in stdlib
		200000: "",                      // unknown, no text
		700:    "",                      // unknown, no text
	}
	for code, want := range cases {
		if got := Text(code); got != want {
			t.Errorf("Text(%d) = %q, want %q", code, got, want)
		}
	}
}
