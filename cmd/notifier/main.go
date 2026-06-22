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
	cons, err := svc.ConnectKafkaConsumer("notifier", bus.TopicNotifyEvents)
	if err != nil {
		svc.Log.Error("connect kafka consumer", "err", err)
		os.Exit(1)
	}

	// Reuse the proven Manager + default registry for delivery; the Runner adds the
	// consume loop, dedup, channel loading, and outcome recording around it. The same
	// Runner also fans down/recovery out to the org's registered outbound webhooks,
	// signed per-webhook (PRD-005 7); the pool is the webhook store.
	registry := notify.Default()
	mgr := notify.NewManager(nil, svc.Log)
	runner := notify.NewRunner(mgr, registry, pg, rd, cons, svc.Log, notify.WithWebhooks(pg))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		if err := runner.Run(runCtx); err != nil {
			svc.Log.Error("notifier runner stopped", "err", err)
		}
	}()

	if err := svc.Run(ctx); err != nil {
		svc.Log.Error("run", "err", err)
		os.Exit(1)
	}
}
