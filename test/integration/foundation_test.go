//go:build integration

// Package integration holds the barebones end-to-end wiring test. It starts
// Postgres, Redis, and Redpanda with testcontainers, then asserts the whole
// foundation: migrations run, RLS isolates tenants, Redis locks/caches, Kafka
// round-trips a keyed message with its correlation header, and the health
// server reports ready. Run with: go test -tags integration ./test/integration/
package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/modules/redpanda"

	"pulse/internal/bus"
	"pulse/internal/kv"
	"pulse/internal/obs"
	"pulse/internal/store"
)

func TestFoundation(t *testing.T) {
	ctx := context.Background()

	// --- start infrastructure ---
	pgC, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("pulse"),
		postgres.WithUsername("pulse"),
		postgres.WithPassword("pulse"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	defer func() { _ = pgC.Terminate(ctx) }()

	rdC, err := redis.Run(ctx, "redis:7")
	if err != nil {
		t.Fatalf("start redis: %v", err)
	}
	defer func() { _ = rdC.Terminate(ctx) }()

	rpC, err := redpanda.Run(ctx, "redpandadata/redpanda:v24.2.7", redpanda.WithAutoCreateTopics())
	if err != nil {
		t.Fatalf("start redpanda: %v", err)
	}
	defer func() { _ = rpC.Terminate(ctx) }()

	// --- connection strings ---
	host, err := pgC.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := pgC.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatal(err)
	}
	adminDSN := fmt.Sprintf("postgres://pulse:pulse@%s:%s/pulse?sslmode=disable", host, port.Port())
	appDSN := fmt.Sprintf("postgres://pulse_app:pulse_app@%s:%s/pulse?sslmode=disable", host, port.Port())

	redisURL, err := rdC.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	redisAddr := strings.TrimPrefix(redisURL, "redis://")

	broker, err := rpC.KafkaSeedBroker(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// --- apply schema + seed (the privileged role bypasses RLS) ---
	admin, err := store.Open(ctx, adminDSN)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer admin.Close()

	if err := store.ApplySchema(ctx, admin); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	// Re-running must reset cleanly: this is how we "migrate" in early dev.
	if err := store.ApplySchema(ctx, admin); err != nil {
		t.Fatalf("re-apply schema (reset): %v", err)
	}

	var orgA, orgB int64
	if err := admin.QueryRow(ctx, "INSERT INTO organizations(name, slug) VALUES('Org A', 'org-a') RETURNING id").Scan(&orgA); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "INSERT INTO organizations(name, slug) VALUES('Org B', 'org-b') RETURNING id").Scan(&orgB); err != nil {
		t.Fatal(err)
	}
	// monitors is a real RLS-protected tenant table; use it to prove isolation.
	if _, err := admin.Exec(ctx, "INSERT INTO monitors(org_id, name, url) VALUES($1, 'a-monitor', 'http://a')", orgA); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "INSERT INTO monitors(org_id, name, url) VALUES($1, 'b-monitor', 'http://b')", orgB); err != nil {
		t.Fatal(err)
	}

	app, err := store.Open(ctx, appDSN)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	defer app.Close()

	t.Run("rls_isolation", func(t *testing.T) {
		// Scoped to org A: see only org A's monitor.
		var names []string
		err := app.WithOrg(ctx, orgA, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, "SELECT name FROM monitors ORDER BY name")
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var n string
				if err := rows.Scan(&n); err != nil {
					return err
				}
				names = append(names, n)
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatalf("scoped query: %v", err)
		}
		if len(names) != 1 || names[0] != "a-monitor" {
			t.Fatalf("org A should see only [a-monitor], got %v", names)
		}

		// Unscoped (no app.current_org set): RLS hides every row, fails safe.
		var n int
		if err := app.QueryRow(ctx, "SELECT count(*) FROM monitors").Scan(&n); err != nil {
			t.Fatalf("unscoped count: %v", err)
		}
		if n != 0 {
			t.Fatalf("unscoped query must see 0 rows under RLS, got %d", n)
		}
	})

	t.Run("redis_lock_and_cache", func(t *testing.T) {
		rd, err := kv.Open(ctx, redisAddr)
		if err != nil {
			t.Fatalf("open redis: %v", err)
		}
		defer rd.Close()

		key := "pulse:checknow:42"
		ok, err := rd.AcquireLock(ctx, key, "holder-1", 5*time.Second)
		if err != nil || !ok {
			t.Fatalf("first acquire should succeed: ok=%v err=%v", ok, err)
		}
		ok2, err := rd.AcquireLock(ctx, key, "holder-2", 5*time.Second)
		if err != nil {
			t.Fatalf("second acquire err: %v", err)
		}
		if ok2 {
			t.Fatal("second acquire should fail while lock is held")
		}
		if err := rd.ReleaseLock(ctx, key, "holder-1"); err != nil {
			t.Fatalf("release: %v", err)
		}
		ok3, err := rd.AcquireLock(ctx, key, "holder-2", 5*time.Second)
		if err != nil || !ok3 {
			t.Fatalf("acquire after release should succeed: ok=%v err=%v", ok3, err)
		}

		if err := rd.SetCache(ctx, "ent:v1:7", "team", time.Minute); err != nil {
			t.Fatalf("set cache: %v", err)
		}
		v, found, err := rd.GetCache(ctx, "ent:v1:7")
		if err != nil || !found || v != "team" {
			t.Fatalf("get cache: v=%q found=%v err=%v", v, found, err)
		}
	})

	t.Run("kafka_roundtrip", func(t *testing.T) {
		const topic = "pulse.itest"

		prod, err := bus.NewKafkaProducer([]string{broker})
		if err != nil {
			t.Fatalf("producer: %v", err)
		}
		defer prod.Close()

		pctx := obs.WithCorrelationID(ctx, "corr-123")
		if err := prod.Produce(pctx, topic, "org-1", []byte("hello")); err != nil {
			t.Fatalf("produce: %v", err)
		}

		cons, err := bus.NewKafkaConsumer([]string{broker}, "itest-group", topic)
		if err != nil {
			t.Fatalf("consumer: %v", err)
		}
		defer cons.Close()

		deadline := time.Now().Add(30 * time.Second)
		var got bus.Record
		found := false
		for time.Now().Before(deadline) && !found {
			pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := cons.Poll(pollCtx, func(r bus.Record) error {
				got = r
				found = true
				return nil
			})
			cancel()
			if err != nil && err != context.DeadlineExceeded {
				t.Fatalf("poll: %v", err)
			}
		}
		if !found {
			t.Fatal("did not consume the produced message")
		}
		if got.Key != "org-1" || string(got.Value) != "hello" {
			t.Fatalf("record mismatch: key=%q value=%q", got.Key, got.Value)
		}
		if got.CorrelationID != "corr-123" {
			t.Fatalf("correlation id not propagated: %q", got.CorrelationID)
		}
	})

	t.Run("health_server", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		h := obs.NewHealthServer("127.0.0.1:18099", reg,
			obs.ReadyCheck{Name: "postgres", Check: admin.Ping},
		)
		errc := h.Start()
		defer func() {
			shctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			_ = h.Shutdown(shctx)
			<-errc
		}()
		time.Sleep(200 * time.Millisecond) // let the listener come up

		for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
			resp, err := http.Get("http://127.0.0.1:18099" + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
			}
			_ = resp.Body.Close()
		}
	})
}
