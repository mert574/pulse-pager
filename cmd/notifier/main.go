// Command notifier consumes notify.events and delivers to a monitor's attached
// channels with retry/backoff (reusing internal/notify), deduping each event first
// (Redis fast path + Postgres backstop) and recording the per-channel outcome
// (RFC-007). It connects Postgres + Redis and a consumer for notify.events, then
// runs the delivery Runner alongside the health server.
package main

import (
	"context"
	"fmt"
	"os"

	"pulse/internal/bus"
	"pulse/internal/config"
	"pulse/internal/crypto"
	"pulse/internal/notify"
	"pulse/internal/runtime"
)

func main() {
	svc, err := runtime.Setup(config.ServiceNotifier)
	if err != nil {
		fmt.Fprintln(os.Stderr, "notifier:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	pg, err := svc.ConnectPostgres(ctx)
	if err != nil {
		svc.Log.Error("connect postgres", "err", err)
		os.Exit(1)
	}
	// The notifier reads encrypted channel config, so it needs the cipher to decrypt
	// secret subfields in memory. config already validated the key is present.
	cipher, err := crypto.LoadKey(svc.Cfg.SecretKey)
	if err != nil {
		svc.Log.Error("load secret key", "err", err)
		os.Exit(1)
	}
	pg.SetCipher(cipher)

	rd, err := svc.ConnectRedis(ctx)
	if err != nil {
		svc.Log.Error("connect redis", "err", err)
		os.Exit(1)
	}
	cons, err := svc.ConnectBusConsumer("notifier", bus.TopicNotifyEvents)
	if err != nil {
		svc.Log.Error("connect kafka consumer", "err", err)
		os.Exit(1)
	}
	// The notifier is also the only sender of transactional email (magic-link, invite,
	// Team-email test): it consumes the semantic intents on email.events in its own
	// group, mints the token at send time, and sends (RFC-019).
	emailCons, err := svc.ConnectBusConsumer("notifier-email", bus.TopicEmailEvents)
	if err != nil {
		svc.Log.Error("connect email consumer", "err", err)
		os.Exit(1)
	}

	// From routing for reputation segmentation (RFC-019 section 6): account mail from
	// the account subdomain, alert mail from the alerts subdomain, each falling back to
	// the single From when its specific address is unset.
	fromAccount := svc.Cfg.SMTP.FromAccount
	if fromAccount == "" {
		fromAccount = svc.Cfg.SMTP.From
	}
	fromAlerts := svc.Cfg.SMTP.FromAlerts
	if fromAlerts == "" {
		fromAlerts = svc.Cfg.SMTP.From
	}

	// Reuse the proven Manager + default registry for delivery; the Runner adds the
	// consume loop, dedup, channel loading, and outcome recording around it. The same
	// Runner also fans down/recovery out to the org's registered outbound webhooks,
	// signed per-webhook (PRD-005 7); the pool is the webhook store.
	registry := notify.Default()
	mgr := notify.NewManager(nil, svc.Log)
	// Wire the Team email channel's deps: the platform mailer (real SMTP when
	// PULSE_SMTP_* is set, else a logging dev mailer) and the member-email resolver
	// (the pool, which resolves member ids to addresses scoped to the event's org).
	// The mailer's default From is the alerts address: the Team-email alert path leaves
	// Mail.From unset and so sends from there, while the email-intent path below sets
	// From per category.
	mailer := notify.NewMailerFromConfig(
		svc.Cfg.SMTP.Host, svc.Cfg.SMTP.Port, svc.Cfg.SMTP.Username,
		svc.Cfg.SMTP.Password, fromAlerts, svc.Cfg.SMTP.TLSMode, svc.Log,
	)
	mgr.SetEmailDeps(mailer, pg)
	// The alert/test emails link the recipient to their channels page; the notifier
	// builds those emails, so it needs the SPA origin from config.
	notify.SetAppBaseURL(svc.Cfg.AppBaseURL)
	runner := notify.NewRunner(mgr, registry, pg, rd, cons, svc.Log, notify.WithWebhooks(pg))
	// The email-intent consumer: the only sender of magic-link / invite / Team-email
	// test mail. It mints tokens (Redis record for magic-link, invitation row for
	// invite) at send time, so nothing usable rides the bus (RFC-019 section 5).
	emailRunner := notify.NewEmailRunner(emailCons, mailer, rd, pg, fromAccount, fromAlerts, svc.Log)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		if err := runner.Run(runCtx); err != nil {
			svc.Log.Error("notifier runner stopped", "err", err)
		}
	}()
	go func() {
		if err := emailRunner.Run(runCtx); err != nil {
			svc.Log.Error("notifier email runner stopped", "err", err)
		}
	}()

	if err := svc.Run(ctx); err != nil {
		svc.Log.Error("run", "err", err)
		os.Exit(1)
	}
}
