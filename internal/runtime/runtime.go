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
	"strconv"
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

	// lagGauge is shared across consumers in a process (a service may join several
	// groups, e.g. the notifier), registered once and keyed by group.
	lagGauge *prometheus.GaugeVec
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
	// Pool saturation gauges, sampled at scrape time (RFC-010 section 2.4).
	if s.Reg != nil {
		s.Reg.MustRegister(
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{
				Name: "pulse_db_pool_in_use",
				Help: "pgx pool connections currently acquired.",
			}, func() float64 { return float64(pg.Stat().AcquiredConns()) }),
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{
				Name: "pulse_db_pool_idle",
				Help: "pgx pool connections currently idle.",
			}, func() float64 { return float64(pg.Stat().IdleConns()) }),
		)
	}
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
	// Pool saturation gauge, sampled at scrape time (RFC-010 section 2.4). in_use is
	// the open connections that are not idle.
	if s.Reg != nil {
		s.Reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "pulse_redis_pool_in_use",
			Help: "Redis pool connections currently in use (total minus idle).",
		}, func() float64 {
			st := rd.PoolStats()
			return float64(st.TotalConns - st.IdleConns)
		}))
	}
	return rd, nil
}

// ConnectBusProducer opens an event-bus producer on the configured backend
// (PULSE_BUS) and registers its readiness check and closer.
func (s *Service) ConnectBusProducer() (*bus.Producer, error) {
	var p *bus.Producer
	var err error
	switch s.Cfg.BusBackend {
	case "redis":
		p, err = bus.NewRedisProducer(s.Cfg.RedisAddr)
	default:
		p, err = bus.NewKafkaProducer(s.Cfg.KafkaBrokers)
	}
	if err != nil {
		return nil, err
	}
	s.AddReady("bus-producer", p.Ping)
	s.AddCloser(func(context.Context) error { p.Close(); return nil })
	return p, nil
}

// ConnectBusConsumer joins a group on the configured backend (PULSE_BUS) and
// registers its readiness check and closer.
func (s *Service) ConnectBusConsumer(group string, topics ...string) (*bus.Consumer, error) {
	var c *bus.Consumer
	var err error
	switch s.Cfg.BusBackend {
	case "redis":
		c, err = bus.NewRedisConsumer(s.Cfg.RedisAddr, group, topics...)
	default:
		c, err = bus.NewKafkaConsumer(s.Cfg.KafkaBrokers, group, topics...)
	}
	if err != nil {
		return nil, err
	}
	s.AddReady("bus-consumer", c.Ping)
	s.AddCloser(func(context.Context) error { c.Close(); return nil })
	// Poll consumer-group lag into a gauge (RFC-010 section 2.4); the Kafka backend
	// reports it, the Redis backend returns nothing so the gauge stays empty. The gauge
	// is registered once per process and shared (a service may join several groups), with
	// each consumer's series keyed by its group.
	if s.Reg != nil {
		if s.lagGauge == nil {
			s.lagGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Name: "pulse_kafka_consumer_lag",
				Help: "Consumer group lag (messages behind the high-water mark) by group, topic, partition.",
			}, []string{"group", "topic", "partition"})
			s.Reg.MustRegister(s.lagGauge)
		}
		pollCtx, cancel := context.WithCancel(context.Background())
		s.AddCloser(func(context.Context) error { cancel(); return nil })
		go s.pollLag(pollCtx, c, group, s.lagGauge)
	}
	return c, nil
}

// pollLag samples the consumer group's lag on a ticker and writes it to the gauge. It
// runs until the context is cancelled (on shutdown). A fetch error is logged at debug
// and retried on the next tick, so a transient broker blip does not spam logs.
func (s *Service) pollLag(ctx context.Context, c *bus.Consumer, group string, g *prometheus.GaugeVec) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		entries, err := c.Lag(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.Log.Debug("consumer lag poll failed", "err", err)
			}
		} else {
			for _, e := range entries {
				g.WithLabelValues(group, e.Topic, strconv.Itoa(int(e.Partition))).Set(float64(e.Lag))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Run sets up tracing, serves the health endpoints, and blocks until a signal
// or a fatal health-server error, then shuts everything down with a timeout.
func (s *Service) Run(ctx context.Context) error {
	traceShutdown, err := obs.SetupTracing(ctx, string(s.Cfg.Service), s.Cfg.TracingEnabled, s.Cfg.OTLPEndpoint)
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
