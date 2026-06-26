//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/redis"

	"pulse/internal/bus"
)

// TestRedisBus exercises the Redis Streams bus backend against a real Redis: a
// produced message round-trips to a consumer, and a message whose handler errors is
// left unacked and redelivered on a later poll (at-least-once, same as Kafka).
func TestRedisBus(t *testing.T) {
	ctx := context.Background()

	rdC, err := redis.Run(ctx, "redis:7")
	if err != nil {
		t.Fatalf("start redis: %v", err)
	}
	defer func() { _ = rdC.Terminate(ctx) }()

	url, err := rdC.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(url, "redis://")

	const topic = bus.TopicCheckResults
	prod, err := bus.NewRedisProducer(addr)
	if err != nil {
		t.Fatalf("producer: %v", err)
	}
	defer prod.Close()

	cons, err := bus.NewRedisConsumer(addr, "alerting", topic)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	defer cons.Close()

	// 1. round-trip: produce one, consume it.
	if err := prod.Produce(ctx, topic, "m1", []byte("hello")); err != nil {
		t.Fatalf("produce m1: %v", err)
	}
	rec := pollOne(ctx, t, cons, func(context.Context, bus.Record) error { return nil })
	if rec.Topic != topic || rec.Key != "m1" || string(rec.Value) != "hello" {
		t.Fatalf("round-trip mismatch: %+v", rec)
	}

	// 2. at-least-once: a handler error leaves the entry unacked.
	if err := prod.Produce(ctx, topic, "m2", []byte("retry")); err != nil {
		t.Fatalf("produce m2: %v", err)
	}
	delivered := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		perr := cons.Poll(ctx, func(_ context.Context, r bus.Record) error {
			if r.Key == "m2" {
				delivered = true
				return errors.New("boom")
			}
			return nil
		})
		if delivered {
			if perr == nil {
				t.Fatal("expected the handler error to surface from Poll")
			}
			break
		}
	}
	if !delivered {
		t.Fatal("m2 was never delivered")
	}

	// 3. redelivery: a later poll re-delivers the unacked m2 from the pending list.
	rec2 := pollOne(ctx, t, cons, func(context.Context, bus.Record) error { return nil })
	if rec2.Key != "m2" || string(rec2.Value) != "retry" {
		t.Fatalf("expected redelivery of m2, got %+v", rec2)
	}
}

// pollOne polls until the handler sees one record, then returns it. The handler runs
// inside the poll so a nil return acks the record.
func pollOne(ctx context.Context, t *testing.T, cons *bus.Consumer, handler func(context.Context, bus.Record) error) bus.Record {
	t.Helper()
	var got bus.Record
	seen := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := cons.Poll(ctx, func(rctx context.Context, r bus.Record) error {
			got, seen = r, true
			return handler(rctx, r)
		}); err != nil {
			t.Fatalf("poll: %v", err)
		}
		if seen {
			return got
		}
	}
	t.Fatal("no record within deadline")
	return got
}
