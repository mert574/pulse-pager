package notify

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"pulse/internal/bus"
	"pulse/internal/crypto"
	"pulse/internal/events"
)

// memFlows is the maglink.Store seam: an in-memory single-use record store.
type memFlows struct{ m map[string]string }

func newMemFlows() *memFlows { return &memFlows{m: map[string]string{}} }

func (f *memFlows) SetCache(_ context.Context, k, v string, _ time.Duration) error {
	f.m[k] = v
	return nil
}

func (f *memFlows) GetDelCache(_ context.Context, k string) (string, bool, error) {
	v, ok := f.m[k]
	delete(f.m, k)
	return v, ok, nil
}

// stubInvites is the EmailIntentStore seam: it records the call and returns a
// configurable affected count so a test can drive the still-pending vs not branches.
type stubInvites struct {
	affected int64
	calls    int
	gotOrg   int64
	gotID    int64
	gotHash  string
}

func (s *stubInvites) SetInvitationToken(_ context.Context, orgID, inviteID int64, tokenHash string) (int64, error) {
	s.calls++
	s.gotOrg, s.gotID, s.gotHash = orgID, inviteID, tokenHash
	return s.affected, nil
}

func emailRecord(t *testing.T, in events.EmailIntent) bus.Record {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	return bus.Record{Topic: bus.TopicEmailEvents, Value: b}
}

// linkToken pulls the raw token out of a URL that follows marker, up to the line end.
func linkToken(body, marker string) string {
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	if j := strings.IndexAny(rest, "\r\n "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func TestEmailRunnerMagicLink(t *testing.T) {
	mailer := &fakeMailer{}
	flows := newMemFlows()
	r := NewEmailRunner(nil, mailer, flows, &stubInvites{}, "login@account.test", "alerts@alerts.test", nil)

	rec := emailRecord(t, events.EmailIntent{
		Type:      events.EmailMagicLink,
		Locale:    "en",
		MagicLink: &events.MagicLinkRequested{Email: "user@x.test"},
	})
	if err := r.handle(context.Background(), rec); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if len(mailer.sent) != 1 {
		t.Fatalf("want 1 mail, got %d", len(mailer.sent))
	}
	m := mailer.sent[0]
	if m.To != "user@x.test" {
		t.Errorf("To = %q", m.To)
	}
	if m.From != "login@account.test" {
		t.Errorf("From = %q, want the account subdomain", m.From)
	}
	if !strings.Contains(m.Body, "/auth/email/verify?token=") {
		t.Errorf("body missing the verify link:\n%s", m.Body)
	}
	// The token in the emailed link must hash to the stored record key: mint, store,
	// and the URL all have to agree or verify would fail.
	tok := linkToken(m.Body, "/auth/email/verify?token=")
	if _, ok := flows.m["magiclink:"+crypto.HashToken(tok)]; !ok {
		t.Error("the emailed link's token does not match the stored magic-link record")
	}
}

func TestEmailRunnerInvitationSends(t *testing.T) {
	mailer := &fakeMailer{}
	inv := &stubInvites{affected: 1}
	r := NewEmailRunner(nil, mailer, newMemFlows(), inv, "login@account.test", "alerts@alerts.test", nil)

	rec := emailRecord(t, events.EmailIntent{
		Type:   events.EmailInvitation,
		Locale: "en",
		Invitation: &events.InvitationRequested{
			InvitationID: 7, OrgID: 42, OrgName: "Acme",
			Inviter: "Jane Doe (jane@acme.com)", Role: "member", Email: "newbie@x.test",
		},
	})
	if err := r.handle(context.Background(), rec); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if inv.calls != 1 || inv.gotOrg != 42 || inv.gotID != 7 || inv.gotHash == "" {
		t.Fatalf("SetInvitationToken called wrong: %+v", inv)
	}
	if len(mailer.sent) != 1 {
		t.Fatalf("want 1 mail, got %d", len(mailer.sent))
	}
	m := mailer.sent[0]
	if m.To != "newbie@x.test" || m.From != "login@account.test" {
		t.Errorf("to/from = %q / %q", m.To, m.From)
	}
	if !strings.Contains(m.Body, "/invitations/") {
		t.Errorf("body missing the accept link:\n%s", m.Body)
	}
	if !strings.Contains(m.HTML, "Jane Doe (jane@acme.com) invited you") {
		t.Error("html missing the inviter line")
	}
}

func TestEmailRunnerInvitationSkippedWhenNotPending(t *testing.T) {
	mailer := &fakeMailer{}
	inv := &stubInvites{affected: 0} // revoked/accepted before the notifier ran
	r := NewEmailRunner(nil, mailer, newMemFlows(), inv, "login@account.test", "alerts@alerts.test", nil)

	rec := emailRecord(t, events.EmailIntent{
		Type:       events.EmailInvitation,
		Invitation: &events.InvitationRequested{InvitationID: 7, OrgID: 42, Email: "x@x.test"},
	})
	if err := r.handle(context.Background(), rec); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(mailer.sent) != 0 {
		t.Fatalf("no mail should be sent when the invite is not pending, sent %d", len(mailer.sent))
	}
}

func TestEmailRunnerChannelTestGoesToClickerOnly(t *testing.T) {
	mailer := &fakeMailer{}
	r := NewEmailRunner(nil, mailer, newMemFlows(), &stubInvites{}, "login@account.test", "alerts@alerts.test", nil)

	rec := emailRecord(t, events.EmailIntent{
		Type: events.EmailChannelTest,
		ChannelTest: &events.ChannelTestRequested{
			ChannelID: 3, ChannelName: "Ops Team", OrgID: 42, RequestedByEmail: "me@x.test",
		},
	})
	if err := r.handle(context.Background(), rec); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(mailer.sent) != 1 {
		t.Fatalf("want 1 mail, got %d", len(mailer.sent))
	}
	m := mailer.sent[0]
	if m.To != "me@x.test" {
		t.Errorf("To = %q, the test must go to the clicker only", m.To)
	}
	if m.From != "alerts@alerts.test" {
		t.Errorf("From = %q, want the alerts subdomain", m.From)
	}
	if !strings.Contains(m.HTML, "Ops Team") {
		t.Error("html missing the channel name")
	}
}

func TestEmailRunnerSendFailureRedelivers(t *testing.T) {
	mailer := &fakeMailer{fail: errors.New("smtp down")}
	r := NewEmailRunner(nil, mailer, newMemFlows(), &stubInvites{affected: 1}, "a", "b", nil)

	rec := emailRecord(t, events.EmailIntent{
		Type:      events.EmailMagicLink,
		MagicLink: &events.MagicLinkRequested{Email: "u@x.test"},
	})
	if err := r.handle(context.Background(), rec); err == nil {
		t.Fatal("a send failure must return an error so the intent redelivers")
	}
}

func TestEmailRunnerDropsBadAndUnknownMessages(t *testing.T) {
	mailer := &fakeMailer{}
	r := NewEmailRunner(nil, mailer, newMemFlows(), &stubInvites{}, "a", "b", nil)

	if err := r.handle(context.Background(), bus.Record{Value: []byte("{not json")}); err != nil {
		t.Fatalf("a poison message should be committed (nil), got %v", err)
	}
	rec := emailRecord(t, events.EmailIntent{Type: "nope"})
	if err := r.handle(context.Background(), rec); err != nil {
		t.Fatalf("an unknown type should be committed (nil), got %v", err)
	}
	if len(mailer.sent) != 0 {
		t.Fatalf("nothing should be sent for bad/unknown messages, sent %d", len(mailer.sent))
	}
}
