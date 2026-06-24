package notify

import (
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

func TestRenderEmailHTMLEscapesAndContainsContent(t *testing.T) {
	html := RenderEmailHTML(EmailContent{
		Preheader: "preview line",
		Banner:    &EmailBanner{Label: "Down", Tone: "down"},
		Heading:   "Acme <script> is down",
		Intro:     "intro text",
		Rows:      []EmailRow{{Label: "URL", Value: "https://x.test/a?b=1&c=2"}},
		Button:    &EmailButton{Label: "Go", URL: "https://x.test/accept?t=a&b=c"},
		Note:      "small note",
		Footer:    "footer line",
	})

	for _, want := range []string{
		"Pulse Pager",  // header wordmark
		"preview line", // preheader
		"DOWN",         // banner label uppercased
		"intro text",
		"small note",
		"footer line",
		"https://x.test/a?b=1&amp;c=2", // row value escaped
	} {
		if !strings.Contains(html, want) {
			t.Errorf("html missing %q", want)
		}
	}

	// User-controlled text must be escaped, never injected as a live tag.
	if strings.Contains(html, "<script>") {
		t.Error("unescaped <script> in heading")
	}
	if !strings.Contains(html, "Acme &lt;script&gt; is down") {
		t.Error("heading not escaped as expected")
	}
	// The button href escapes & to &amp; in attribute context.
	if !strings.Contains(html, "https://x.test/accept?t=a&amp;b=c") {
		t.Error("button href not escaped")
	}
}

func TestAlertEmailHasTextAndHTML(t *testing.T) {
	subject, text, html := AlertEmail(downEvent())
	if subject != "[Pulse Pager] DOWN: Prod API health" {
		t.Errorf("subject = %q", subject)
	}
	// Text part keeps the plain facts (the fallback the integration tests rely on).
	if !strings.Contains(text, "Down since:") || !strings.Contains(text, "Reason:") {
		t.Errorf("text part missing facts:\n%s", text)
	}
	// HTML part is the branded card.
	for _, want := range []string{"Prod API health is down", "status_mismatch", "HTTP 503", "DOWN"} {
		if !strings.Contains(html, want) {
			t.Errorf("html missing %q", want)
		}
	}
}

func TestBuildMessageMultipartParses(t *testing.T) {
	_, text, html := AlertEmail(downEvent())
	raw := buildMessage("from@x.test", []string{"to@x.test"}, "Subj", text, html)

	msg, err := mail.ReadMessage(strings.NewReader(strings.ReplaceAll(string(raw), "\r\n", "\n")))
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse content-type: %v", err)
	}
	// The top level is multipart/related: the alternative (text+html) plus the inline
	// logo the html references with cid:pulselogo.
	if mediaType != "multipart/related" {
		t.Fatalf("media type = %q", mediaType)
	}

	var gotText, gotHTML, gotLogo bool
	rel := multipart.NewReader(msg.Body, params["boundary"])
	for {
		part, err := rel.NextPart()
		if err != nil {
			break
		}
		ct, ctParams, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		switch {
		case ct == "multipart/alternative":
			alt := multipart.NewReader(part, ctParams["boundary"])
			for {
				p, err := alt.NextPart()
				if err != nil {
					break
				}
				inner := p.Header.Get("Content-Type")
				buf := new(strings.Builder)
				if _, err := io.Copy(buf, base64.NewDecoder(base64.StdEncoding, p)); err != nil {
					t.Fatalf("decode part: %v", err)
				}
				switch {
				case strings.HasPrefix(inner, "text/plain"):
					gotText = true
					if !strings.Contains(buf.String(), "Down since:") {
						t.Error("text part lost its content")
					}
				case strings.HasPrefix(inner, "text/html"):
					gotHTML = true
					if !strings.Contains(buf.String(), "Prod API health is down") {
						t.Error("html part lost its content")
					}
					if !strings.Contains(buf.String(), "cid:"+emailLogoCID) {
						t.Error("html part does not reference the inline logo")
					}
				}
			}
		case ct == "image/png":
			gotLogo = true
			if id := part.Header.Get("Content-ID"); id != "<"+emailLogoCID+">" {
				t.Errorf("logo Content-ID = %q", id)
			}
		}
	}
	if !gotText || !gotHTML || !gotLogo {
		t.Errorf("missing parts: text=%v html=%v logo=%v", gotText, gotHTML, gotLogo)
	}
}

func TestBuildMessageNoHTMLStaysPlainText(t *testing.T) {
	raw := string(buildMessage("from@x.test", []string{"to@x.test"}, "Subj", "hello\nworld", ""))
	if !strings.Contains(raw, "Content-Type: text/plain; charset=\"utf-8\"") {
		t.Errorf("expected plain text content-type:\n%s", raw)
	}
	if strings.Contains(raw, "multipart/alternative") {
		t.Error("should not be multipart when html is empty")
	}
	if !strings.Contains(raw, "hello\r\nworld") {
		t.Error("body not CRLF-normalized")
	}
}

func TestInviteAndMagicLinkKeepURLInText(t *testing.T) {
	_, text, html := InviteEmail("Acme", "Jane Doe (jane@acme.com)", "admin", "https://app.test/invitations/tok123", "en")
	if !strings.Contains(text, "https://app.test/invitations/tok123\n") {
		t.Errorf("invite text dropped the accept URL:\n%s", text)
	}
	if !strings.Contains(html, "Accept invitation") {
		t.Error("invite html missing CTA")
	}
	if !strings.Contains(html, "Jane Doe (jane@acme.com) invited you") {
		t.Errorf("invite html missing inviter:\n%s", html)
	}

	_, mtext, mhtml := MagicLinkEmail("https://app.test/auth/email/verify?token=tok456", "en")
	if !strings.Contains(mtext, "https://app.test/auth/email/verify?token=tok456\n") {
		t.Errorf("magic-link text dropped the verify URL:\n%s", mtext)
	}
	if !strings.Contains(mhtml, "Sign in") {
		t.Error("magic-link html missing CTA")
	}
}
