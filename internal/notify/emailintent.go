package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"pulse/internal/bus"
	"pulse/internal/crypto"
	"pulse/internal/events"
	"pulse/internal/maglink"
)

// EmailIntentStore sets a freshly minted invite token hash on a still-pending
// invitation (RFC-019 section 5.2). *store.Pool satisfies it. affected == 0 means the
// invite is no longer pending, so the email is skipped. Defined here so notify does
// not import store.
type EmailIntentStore interface {
	SetInvitationToken(ctx context.Context, orgID, inviteID int64, tokenHash string) (int64, error)
}

// EmailRunner consumes email.events and sends the one transactional email each intent
// asks for (RFC-019). It is the only sender of magic-link, invitation, and Team-email
// test mail. Unlike the notify.events Runner it does no dedup or channel fan-out: one
// intent maps to one email. It mints the magic-link / invite token here, at send time,
// so no usable credential ever rides the bus (RFC-019 section 5). From routing picks
// the reputation subdomain per intent category (section 6): account mail (sign-in
// link, invitation) from fromAccount, alert mail (channel test) from fromAlerts; an
// empty value falls back to the mailer's configured From.
type EmailRunner struct {
	cons        Consumer
	mailer      Mailer
	flows       maglink.Store
	invites     EmailIntentStore
	fromAccount string
	fromAlerts  string
	log         *slog.Logger
}

// NewEmailRunner builds the email-intent consumer. mailer is the platform mailer;
// flows is the Redis store the magic-link record is written to (the api's Verify reads
// the same record); invites sets the invite token hash on the row. fromAccount and
// fromAlerts are the per-category From addresses (RFC-019 section 6).
func NewEmailRunner(cons Consumer, mailer Mailer, flows maglink.Store, invites EmailIntentStore, fromAccount, fromAlerts string, log *slog.Logger) *EmailRunner {
	if log == nil {
		log = slog.Default()
	}
	return &EmailRunner{
		cons:        cons,
		mailer:      mailer,
		flows:       flows,
		invites:     invites,
		fromAccount: fromAccount,
		fromAlerts:  fromAlerts,
		log:         log,
	}
}

// Run consumes email.events until ctx is cancelled, mirroring the notify Runner:
// poll, handle, commit-after-process (a returned error leaves the offset for
// redelivery, kept safe by the per-intent idempotency in section 8 of RFC-019).
func (r *EmailRunner) Run(ctx context.Context) error {
	r.log.Info("notifier email-intent consumer started")
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := r.cons.Poll(ctx, func(recCtx context.Context, rec bus.Record) error {
			return r.handle(recCtx, rec)
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			r.log.Error("email poll failed", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
		}
	}
}

// handle decodes one email.events message and dispatches by intent type. An
// unparseable or unknown message is poison: log and commit (return nil) rather than
// block the partition. A send/mint/store failure returns an error so the intent
// redelivers (at-least-once).
func (r *EmailRunner) handle(ctx context.Context, rec bus.Record) error {
	var in events.EmailIntent
	if err := json.Unmarshal(rec.Value, &in); err != nil {
		r.log.Error("bad email intent, dropping", "err", err)
		return nil
	}
	switch in.Type {
	case events.EmailMagicLink:
		return r.sendMagicLink(ctx, in)
	case events.EmailInvitation:
		return r.sendInvitation(ctx, in)
	case events.EmailChannelTest:
		return r.sendChannelTest(ctx, in)
	default:
		r.log.Error("unknown email intent type, dropping", "type", in.Type)
		return nil
	}
}

// sendMagicLink mints a one-time token, stores its hash in the shared magic-link Redis
// record, and emails the verify link from the account subdomain. The api's GET verify
// reads the same record (RFC-019 section 5.1). A redelivery mints a second link; both
// are single-use, so a rare duplicate beats a missed sign-in link.
func (r *EmailRunner) sendMagicLink(ctx context.Context, in events.EmailIntent) error {
	p := in.MagicLink
	if p == nil || p.Email == "" {
		r.log.Error("magic-link intent missing payload, dropping")
		return nil
	}
	raw, err := maglink.Mint(ctx, r.flows, p.Email)
	if err != nil {
		return fmt.Errorf("mint magic link for %s: %w", p.Email, err)
	}
	subject, body, html := MagicLinkEmail(appBaseURL+"/auth/email/verify?token="+raw, in.Locale)
	if err := r.mailer.Send(ctx, Mail{To: p.Email, From: r.fromAccount, Subject: subject, Body: body, HTML: html}); err != nil {
		return fmt.Errorf("send magic link to %s: %w", p.Email, err)
	}
	return nil
}

// sendInvitation mints a fresh invite token, writes its hash to the still-pending row,
// and emails the accept link from the account subdomain (RFC-019 section 5.2). If the
// row is no longer pending (revoked/accepted), affected is 0 and nothing is sent.
func (r *EmailRunner) sendInvitation(ctx context.Context, in events.EmailIntent) error {
	p := in.Invitation
	if p == nil || p.InvitationID == 0 || p.OrgID == 0 {
		r.log.Error("invitation intent missing payload, dropping")
		return nil
	}
	raw, err := crypto.NewOpaqueToken()
	if err != nil {
		return fmt.Errorf("mint invite token: %w", err)
	}
	affected, err := r.invites.SetInvitationToken(ctx, p.OrgID, p.InvitationID, crypto.HashToken(raw))
	if err != nil {
		return fmt.Errorf("set invite token (org %d invite %d): %w", p.OrgID, p.InvitationID, err)
	}
	if affected == 0 {
		r.log.Info("invitation no longer pending, skipping send", "org", p.OrgID, "invite", p.InvitationID)
		return nil
	}
	subject, body, html := InviteEmail(p.OrgName, p.Inviter, p.Role, appBaseURL+"/invitations/"+raw, in.Locale)
	if err := r.mailer.Send(ctx, Mail{To: p.Email, From: r.fromAccount, Subject: subject, Body: body, HTML: html}); err != nil {
		return fmt.Errorf("send invite to %s: %w", p.Email, err)
	}
	return nil
}

// sendChannelTest emails a one-off "this channel works" test to the person who clicked
// Test, from the alerts subdomain (RFC-019 section 3/6). Published only for the
// Team-email channel; the recipient is the clicker, never the whole channel.
func (r *EmailRunner) sendChannelTest(ctx context.Context, in events.EmailIntent) error {
	p := in.ChannelTest
	if p == nil || p.RequestedByEmail == "" {
		r.log.Error("channel-test intent missing payload, dropping")
		return nil
	}
	subject, body, html := TestEmail(p.ChannelName, "the Team email channel works", p.OrgID)
	if err := r.mailer.Send(ctx, Mail{To: p.RequestedByEmail, From: r.fromAlerts, Subject: subject, Body: body, HTML: html}); err != nil {
		return fmt.Errorf("send channel test to %s: %w", p.RequestedByEmail, err)
	}
	return nil
}
