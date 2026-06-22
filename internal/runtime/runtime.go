// Package runtime is the shared service bootstrap: load config, build the logger
// and metrics registry, connect dependencies (registering each as a readiness
// check and a shutdown closer), serve the health/metrics endpoints, and shut
// down gracefully on SIGINT/SIGTERM. Every cmd/<service> main is a thin caller.
package runtime

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"pulse/internal/bus"
	"pulse/internal/config"
	"pulse/internal/kv"
	"pulse/internal/obs"
	"pulse/internal/store"
)

// Closer shuts a dependency down with a deadline.
type Closer func(context.Context) error

// Service holds the bootstrapped basics and the registered checks/closers.
type Service struct {
	Cfg *config.Config
	Log *slog.Logger
	Reg *prometheus.Registry

	checks  []obs.ReadyCheck
	closers []Closer
}

// Setup loads config and builds the logger and metrics registry for a service.
func Setup(svc config.Service) (*Service, error) {
	cfg, err := config.Load(svc)
	if err != nil {
		return nil, err
	}
	return &Service{
		Cfg: cfg,
		Log: obs.Logger(string(svc), cfg.LogLevel),
		Reg: obs.NewRegistry(string(svc)),
	}, nil
}

// AddReady registers a dependency readiness probe surfaced on /readyz.
func (s *Service) AddReady(name string, check func(context.Context) error) {
	s.checks = append(s.checks, obs.ReadyCheck{Name: name, Check: check})
}

// AddCloser registers a shutdown step, run in reverse order on stop.
func (s *Service) AddCloser(c Closer) { s.closers = append(s.closers, c) }

// ConnectPostgres opens the pool and registers its readiness check and closer.
func (s *Service) ConnectPostgres(ctx context.Context) (*store.Pool, error) {
	pg, err := store.Open(ctx, s.Cfg.PostgresDSN)
	if err != nil {
		return nil, err
	}
	s.AddReady("postgres", pg.Ping)
	s.AddCloser(func(context.Context) error { pg.Close(); return nil })
	return pg, nil
}

// ConnectRedis opens the kv (Redis) client and registers its readiness check and closer.
func (s *Service) ConnectRedis(ctx context.Context) (*kv.Client, error) {
	rd, err := kv.Open(ctx, s.Cfg.RedisAddr)
	if err != nil {
		return nil, err
	}
	s.AddReady("redis", rd.Ping)
	s.AddCloser(func(context.Context) error { return rd.Close() })
	return rd, nil
}

// ConnectKafkaProducer opens a producer and registers its readiness check and closer.
func (s *Service) ConnectKafkaProducer() (*bus.Producer, error) {
	p, err := bus.NewProducer(s.Cfg.KafkaBrokers)
	if err != nil {
		return nil, err
	}
	s.AddReady("kafka-producer", p.Ping)
	s.AddCloser(func(context.Context) error { p.Close(); return nil })
	return p, nil
}

// ConnectKafkaConsumer joins a group and registers its readiness check and closer.
func (s *Service) ConnectKafkaConsumer(group string, topics ...string) (*bus.Consumer, error) {
	c, err := bus.NewConsumer(s.Cfg.KafkaBrokers, group, topics...)
	if err != nil {
		return nil, err
	}
	s.AddReady("kafka-consumer", c.Ping)
	s.AddCloser(func(context.Context) error { c.Close(); return nil })
	return c, nil
}

// Run sets up tracing, serves the health endpoints, and blocks until a signal
// or a fatal health-server error, then shuts everything down with a timeout.
func (s *Service) Run(ctx context.Context) error {
	traceShutdown, err := obs.SetupTracing(ctx, string(s.Cfg.Service), s.Cfg.TracingEnabled)
	if err != nil {
		return err
	}
	s.AddCloser(traceShutdown)

	h := obs.NewHealthServer(s.Cfg.HealthAddr, s.Reg, s.checks...)
	errc := h.Start()
	s.Log.Info("service started", "health_addr", s.Cfg.HealthAddr, "region", s.Cfg.Region)

	sigctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-sigctx.Done():
		s.Log.Info("shutdown signal received")
	case err := <-errc:
		if err != nil {
			s.Log.Error("health server failed", "err", err)
		}
	}

	shctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = h.Shutdown(shctx)
	for i := len(s.closers) - 1; i >= 0; i-- {
		if err := s.closers[i](shctx); err != nil {
			s.Log.Warn("closer error during shutdown", "err", err)
		}
	}
	s.Log.Info("service stopped")
	return nil
}
