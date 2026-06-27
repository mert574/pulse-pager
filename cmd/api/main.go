// Command api is the control-plane HTTP edge: SPA backend, public REST, auth,
// Stripe webhooks (RFC-000 section 2.1). It boots, connects to Postgres + Redis +
// a Kafka producer, serves health/metrics on the health addr, and serves the real
// identity HTTP API (auth flow, session, me/account, orgs) on the api addr.
//
// Dev shortcut: with PULSE_DEV_AUTH=true the service runs a self-contained dev
// API (fake auth + sample data) so the SPA can get past login without the real
// auth backend. It needs no Postgres/Redis/Kafka. Never use in production.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"pulse/internal/api"
	"pulse/internal/config"
	"pulse/internal/devapi"
	"pulse/internal/obs"
	"pulse/internal/runtime"
)

func main() {
	if os.Getenv("PULSE_DEV_AUTH") == "true" {
		runDev()
		return
	}

	svc, err := runtime.Setup(config.ServiceAPI)
	if err != nil {
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	pg, err := svc.ConnectPostgres(ctx)
	if err != nil {
		svc.Log.Error("connect postgres", "err", err)
		os.Exit(1)
	}
	rd, err := svc.ConnectRedis(ctx)
	if err != nil {
		svc.Log.Error("connect redis", "err", err)
		os.Exit(1)
	}
	prod, err := svc.ConnectBusProducer()
	if err != nil {
		svc.Log.Error("connect kafka", "err", err)
		os.Exit(1)
	}

	// Build the real identity + monitors HTTP API and serve it on the api addr,
	// separate from the health/metrics addr. The producer lets the monitor handlers
	// publish monitor.changed so the live schedule tracks edits (PRD-006 5).
	_, handler, err := api.Build(ctx, api.Deps{
		Cfg:      svc.Cfg,
		Store:    pg,
		Redis:    rd,
		Log:      svc.Log,
		Reg:      svc.Reg,
		Producer: prod,
	})
	if err != nil {
		svc.Log.Error("build api", "err", err)
		os.Exit(1)
	}

	apiAddr := os.Getenv("PULSE_API_ADDR")
	if apiAddr == "" {
		apiAddr = ":8080"
	}
	apiSrv := &http.Server{
		Addr:              apiAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		svc.Log.Info("api http listening", "addr", apiAddr)
		if err := apiSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			svc.Log.Error("api http server failed", "err", err)
			os.Exit(1)
		}
	}()
	svc.AddCloser(func(ctx context.Context) error { return apiSrv.Shutdown(ctx) })

	if err := svc.Run(ctx); err != nil {
		svc.Log.Error("run", "err", err)
		os.Exit(1)
	}
}

// runDev serves the development-only stub API. Self-contained, no infra.
func runDev() {
	log := obs.Logger("api-dev", os.Getenv("PULSE_LOG_LEVEL"))
	addr := os.Getenv("PULSE_API_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           devapi.Handler(log),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Warn("DEV AUTH MODE: fake auth + sample data, do not use in production", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("dev api server failed", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	shctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shctx)
	log.Info("dev api stopped")
}
