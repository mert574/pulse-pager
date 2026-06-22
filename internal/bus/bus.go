// Package bus wraps the Kafka client (franz-go) with thin producer/consumer
// helpers, the canonical topic names and partition keys (RFC-002 section 5), and
// correlation-id propagation over a message header so one check can be followed
// across services (RFC-010 section 1.2). The barebones covers reliable produce
// and consume; DLQs and per-consumer idempotency land with the real consumers.
package bus

import (
	"context"

	"github.com/twmb/franz-go/pkg/kgo"

	"pulse/internal/obs"
)

// Canonical topics (RFC-002 section 5.1). check.jobs is per-region, see CheckJobsTopic.
const (
	TopicMonitorChanged = "monitor.changed" // key: org_id
	TopicCheckResults   = "check.results"   // key: monitor_id
	TopicNotifyEvents   = "notify.events"   // key: monitor_id
	TopicAuditEvents    = "audit.events"    // key: org_id
	TopicBillingEvents  = "billing.events"  // key: org_id
	TopicRegionHealth   = "region.health"   // key: region
)

// CheckJobsTopic is the per-region job topic, e.g. check.jobs.eu-west (key: monitor_id).
func CheckJobsTopic(region string) string { return "check.jobs." + region }

const correlationHeader = "pulse-correlation-id"

// Record is a consumed message in the shape the handlers want.
type Record struct {
	Topic         string
	Key           string
	Value         []byte
	CorrelationID string
}

// Producer publishes messages.
type Producer struct {
	cl *kgo.Client
}

// NewProducer connects a producer to the brokers.
func NewProducer(brokers []string) (*Producer, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		// Let the broker create a topic on first produce when it permits it. In
		// prod, topics are provisioned by RFC-011 and the broker disallows this,
		// so the flag is a harmless no-op there.
		kgo.AllowAutoTopicCreation(),
	)
	if err != nil {
		return nil, err
	}
	return &Producer{cl: cl}, nil
}

// Produce synchronously publishes one message, keyed for per-key ordering, and
// stamps the context's correlation id as a header.
func (p *Producer) Produce(ctx context.Context, topic, key string, value []byte) error {
	rec := &kgo.Record{
		Topic: topic,
		Key:   []byte(key),
		Value: value,
	}
	if id := obs.CorrelationID(ctx); id != "" {
		rec.Headers = append(rec.Headers, kgo.RecordHeader{Key: correlationHeader, Value: []byte(id)})
	}
	return p.cl.ProduceSync(ctx, rec).FirstErr()
}

// Ping checks broker connectivity (used by /readyz).
func (p *Producer) Ping(ctx context.Context) error { return p.cl.Ping(ctx) }

// Close flushes and closes the producer.
func (p *Producer) Close() { p.cl.Close() }

// Consumer reads from a consumer group.
type Consumer struct {
	cl *kgo.Client
}

// NewConsumer joins the group and subscribes to the topics.
func NewConsumer(brokers []string, group string, topics ...string) (*Consumer, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topics...),
		// A fresh group reads from the start. Our consumers are idempotent
		// (RFC-002 section 6), so reprocessing from the beginning is safe.
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	if err != nil {
		return nil, err
	}
	return &Consumer{cl: cl}, nil
}

// Poll fetches one batch and calls handler for each record. It returns on context
// cancellation or the first fetch/handler error. Offsets autocommit by default;
// the real consumers tighten this with explicit commits once idempotency lands.
func (c *Consumer) Poll(ctx context.Context, handler func(Record) error) error {
	fetches := c.cl.PollFetches(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if errs := fetches.Errors(); len(errs) > 0 {
		return errs[0].Err
	}
	var herr error
	fetches.EachRecord(func(r *kgo.Record) {
		if herr != nil {
			return
		}
		herr = handler(toRecord(r))
	})
	return herr
}

// Ping checks broker connectivity (used by /readyz).
func (c *Consumer) Ping(ctx context.Context) error { return c.cl.Ping(ctx) }

// Close leaves the group and closes the consumer.
func (c *Consumer) Close() { c.cl.Close() }

func toRecord(r *kgo.Record) Record {
	rec := Record{Topic: r.Topic, Key: string(r.Key), Value: r.Value}
	for _, h := range r.Headers {
		if h.Key == correlationHeader {
			rec.CorrelationID = string(h.Value)
		}
	}
	return rec
}
