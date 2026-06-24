// Command billing is the billing sync edge (RFC-018 Phase 1). It serves one
// hand-wired, signature-verified webhook (POST /billing/webhooks/{provider}) that is
// the authoritative path from the billing provider into Pulse: it verifies the
// signature, normalizes the event through the provider adapter, and reconciles the
// org's subscription and plan in one transaction. No UI, no operator/self-serve calls
// (those are later phases). It boots, connects Postgres, serves health/metrics on the
// health addr, and serves the webhook on the billing addr.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"pulse/internal/billing"
	"pulse/internal/billing/paddle"
	"pulse/internal/config"
	"pulse/internal/runtime"
)

func main() {
	svc, err := runtime.Setup(config.ServiceBilling)
	if err != nil {
		fmt.Fprintln(os.Stderr, "billing:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	pg, err := svc.ConnectPostgres(ctx)
	if err != nil {
		svc.Log.Error("connect postgres", "err", err)
		os.Exit(1)
	}

	// Pick the provider adapter. The stub is the default (no provider account); paddle
	// is the real adapter, with its price map loaded from plan_prices.
	var provider billing.Provider
	switch svc.Cfg.Billing.Provider {
	case "paddle":
		prices, err := pg.ListPlanPrices(ctx, "paddle")
		if err != nil {
			svc.Log.Error("load plan_prices", "err", err)
			os.Exit(1)
		}
		provider, err = paddle.New(paddle.Config{
			APIKey:        svc.Cfg.Billing.PaddleAPIKey,
			BaseURL:       svc.Cfg.Billing.APIBase,
			WebhookSecret: svc.Cfg.Billing.WebhookSecret,
			Prices:        paddle.PriceMap(prices),
		})
		if err != nil {
			svc.Log.Error("build paddle adapter", "err", err)
			os.Exit(1)
		}
	default:
		provider = billing.NewStub(svc.Cfg.Billing.WebhookSecret)
	}

	handler := billing.NewHandler(provider, pg, svc.Log)
	mux := http.NewServeMux()
	mux.Handle("POST /billing/webhooks/{provider}", handler)

	srv := &http.Server{
		Addr:              svc.Cfg.Billing.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		svc.Log.Info("billing webhook listening", "addr", svc.Cfg.Billing.Addr, "provider", provider.Name())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			svc.Log.Error("billing http server failed", "err", err)
			os.Exit(1)
		}
	}()
	svc.AddCloser(func(ctx context.Context) error { return srv.Shutdown(ctx) })

	if err := svc.Run(ctx); err != nil {
		svc.Log.Error("run", "err", err)
		os.Exit(1)
	}
}
