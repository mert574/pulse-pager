package notify

import (
	"context"
	"errors"
	"testing"
)

// fakeMailer records every Mail it is asked to send so a test can assert exactly
// who got mail. send, when set, lets a test make a delivery fail.
type fakeMailer struct {
	sent []Mail
	fail error
}

func (m *fakeMailer) Send(_ context.Context, msg Mail) error {
	if m.fail != nil {
		return m.fail
	}
	m.sent = append(m.sent, msg)
	return nil
}

// fakeResolver returns the emails for the ids it knows about for the given org. An
// id it does not know is dropped, mirroring the real store join that returns only
// active members of the org. It records the org it was asked about.
type fakeResolver struct {
	byOrg     map[int64]map[int64]string
	gotOrgID  int64
	gotIDs    []int64
	returnErr error
}

func (r *fakeResolver) ActiveMemberEmails(_ context.Context, orgID int64, userIDs []int64) ([]string, error) {
	r.gotOrgID = orgID
	r.gotIDs = userIDs
	if r.returnErr != nil {
		return nil, r.returnErr
	}
	var out []string
	known := r.byOrg[orgID]
	for _, id := range userIDs {
		if email, ok := known[id]; ok {
			out = append(out, email)
		}
	}
	return out, nil
}

func TestPlatformEmailResolvesIDsAndSends(t *testing.T) {
	mailer := &fakeMailer{}
	resolver := &fakeResolver{byOrg: map[int64]map[int64]string{
		7: {10: "a@org.com", 11: "b@org.com"},
	}}
	p := &platformEmailProvider{mailer: mailer, resolver: resolver}

	ev := downEvent()
	ev.OrgID = 7
	cfg := map[string]any{"members": []any{"10", "11"}}
	if err := p.Send(context.Background(), cfg, ev); err != nil {
		t.Fatal(err)
	}

	if resolver.gotOrgID != 7 {
		t.Errorf("resolver called with org %d, want 7", resolver.gotOrgID)
	}
	if len(mailer.sent) != 2 {
		t.Fatalf("sent %d mails, want 2", len(mailer.sent))
	}
	got := map[string]bool{}
	for _, m := range mailer.sent {
		got[m.To] = true
		if m.Subject == "" || m.Body == "" {
			t.Error("mail missing subject or body")
		}
	}
	if !got["a@org.com"] || !got["b@org.com"] {
		t.Errorf("recipients = %v, want a@org.com and b@org.com", got)
	}
}

func TestPlatformEmailDropsIDsNotReturnedByResolver(t *testing.T) {
	mailer := &fakeMailer{}
	// The resolver only knows id 10 for org 7; id 99 (not a member, or a tampered
	// config) is dropped, so it is never emailed.
	resolver := &fakeResolver{byOrg: map[int64]map[int64]string{
		7: {10: "a@org.com"},
	}}
	p := &platformEmailProvider{mailer: mailer, resolver: resolver}

	ev := downEvent()
	ev.OrgID = 7
	cfg := map[string]any{"members": []any{"10", "99"}}
	if err := p.Send(context.Background(), cfg, ev); err != nil {
		t.Fatal(err)
	}
	if len(mailer.sent) != 1 || mailer.sent[0].To != "a@org.com" {
		t.Fatalf("sent = %v, want only a@org.com", mailer.sent)
	}
}

func TestPlatformEmailZeroResolvedIsNoOp(t *testing.T) {
	mailer := &fakeMailer{}
	// All selected ids belong to a different org, so the resolver (scoped to org 7)
	// returns nothing. That is a no-op success, not an error: nothing to send.
	resolver := &fakeResolver{byOrg: map[int64]map[int64]string{
		7: {10: "a@org.com"},
	}}
	p := &platformEmailProvider{mailer: mailer, resolver: resolver}

	ev := downEvent()
	ev.OrgID = 7
	cfg := map[string]any{"members": []any{"500"}}
	if err := p.Send(context.Background(), cfg, ev); err != nil {
		t.Fatalf("zero resolved should be a no-op success, got %v", err)
	}
	if len(mailer.sent) != 0 {
		t.Errorf("sent %d mails, want 0", len(mailer.sent))
	}
}

func TestPlatformEmailEmptyMembersErrors(t *testing.T) {
	p := &platformEmailProvider{mailer: &fakeMailer{}, resolver: &fakeResolver{}}
	ev := downEvent()
	ev.OrgID = 7
	if err := p.Send(context.Background(), map[string]any{"members": []any{}}, ev); err == nil {
		t.Error("empty members should error on send")
	}
}

func TestPlatformEmailValidateRejectsEmpty(t *testing.T) {
	p := &platformEmailProvider{}
	if err := p.Validate(map[string]any{}); err == nil {
		t.Error("missing members should fail Validate")
	}
	if err := p.Validate(map[string]any{"members": []any{}}); err == nil {
		t.Error("empty members should fail Validate")
	}
	if err := p.Validate(map[string]any{"members": []any{"1"}}); err != nil {
		t.Errorf("non-empty members should pass Validate, got %v", err)
	}
}

func TestPlatformEmailNeedsDeps(t *testing.T) {
	ev := downEvent()
	ev.OrgID = 7
	cfg := map[string]any{"members": []any{"1"}}

	noMailer := &platformEmailProvider{resolver: &fakeResolver{}}
	if err := noMailer.Send(context.Background(), cfg, ev); err == nil {
		t.Error("missing mailer should error")
	}
	noResolver := &platformEmailProvider{mailer: &fakeMailer{}}
	if err := noResolver.Send(context.Background(), cfg, ev); err == nil {
		t.Error("missing resolver should error")
	}
}

func TestPlatformEmailSurfacesResolverError(t *testing.T) {
	resolver := &fakeResolver{returnErr: errors.New("db down")}
	p := &platformEmailProvider{mailer: &fakeMailer{}, resolver: resolver}
	ev := downEvent()
	ev.OrgID = 7
	if err := p.Send(context.Background(), map[string]any{"members": []any{"1"}}, ev); err == nil {
		t.Error("a resolver error should propagate so the Manager retries")
	}
}

// TestManagerInjectsEmailDeps confirms the Manager wires the mailer and resolver
// into the email provider through SetEmailDeps, the same injection seam as the http
// client.
func TestManagerInjectsEmailDeps(t *testing.T) {
	mailer := &fakeMailer{}
	resolver := &fakeResolver{byOrg: map[int64]map[int64]string{7: {1: "x@org.com"}}}
	mgr := NewManager(nil, nil)
	mgr.SetEmailDeps(mailer, resolver)

	p, ok := mgr.provider("email")
	if !ok {
		t.Fatal("no email provider registered")
	}
	ep, ok := p.(*platformEmailProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *platformEmailProvider", p)
	}
	if ep.mailer == nil || ep.resolver == nil {
		t.Error("Manager did not inject mailer/resolver into the email provider")
	}
}
