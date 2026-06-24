// Package bus is the event transport: thin producer/consumer helpers, the canonical
// topic names and partition keys (RFC-002 section 5), and correlation-id propagation
// so one check can be followed across services (RFC-010 section 1.2).
//
// The transport is pluggable behind a small backend interface (PULSE_BUS):
//   - "kafka" (default): franz-go against any Kafka-compatible broker. Used in the
//     real distributed deployment (managed Kafka, RFC-011).
//   - "redis": Redis Streams. A single-node lightweight mode that reuses the Redis
//     the services already run, so a small box does not also need a Kafka/JVM broker.
//
// Both keep the same at-least-once contract: a handler that returns an error leaves
// the message unacked so it is redelivered.
package bus

import "context"

// Canonical topics (RFC-002 section 5.1). check.jobs is per-region, see CheckJobsTopic.
const (
	TopicMonitorChanged = "monitor.changed" // key: org_id
	TopicCheckResults   = "check.results"   // key: monitor_id
	TopicNotifyEvents   = "notify.events"   // key: monitor_id
	TopicEmailEvents    = "email.events"    // key: org_id (else email); RFC-019 transactional intents
	TopicAuditEvents    = "audit.events"    // key: org_id
	TopicBillingEvents  = "billing.events"  // key: org_id
	TopicRegionHealth   = "region.health"   // key: region
)

// CheckJobsTopic is the per-region job topic, e.g. check.jobs.us-west (key: monitor_id).
func CheckJobsTopic(region string) string { return "check.jobs." + region }

const correlationHeader = "pulse-correlation-id"

// Record is a consumed message in the shape the handlers want.
type Record struct {
	Topic         string
	Key           string
	Value         []byte
	CorrelationID string
}

// producerBackend is the transport a Producer delegates to (kafka or redis).
type producerBackend interface {
	produce(ctx context.Context, topic, key string, value []byte) error
	ping(ctx context.Context) error
	close()
}

// consumerBackend is the transport a Consumer delegates to.
type consumerBackend interface {
	poll(ctx context.Context, handler func(Record) error) error
	ping(ctx context.Context) error
	close()
}

// Producer publishes messages over the configured backend.
type Producer struct{ b producerBackend }

// Produce synchronously publishes one message, keyed for per-key ordering, and
// carries the context's correlation id.
func (p *Producer) Produce(ctx context.Context, topic, key string, value []byte) error {
	return p.b.produce(ctx, topic, key, value)
}

// Ping checks backend connectivity (used by /readyz).
func (p *Producer) Ping(ctx context.Context) error { return p.b.ping(ctx) }

// Close flushes and closes the producer.
func (p *Producer) Close() { p.b.close() }

// Consumer reads from a consumer group over the configured backend.
type Consumer struct{ b consumerBackend }

// Poll fetches a batch and calls handler for each record. It returns on context
// cancellation or the first handler error; an errored record is left unacked and
// redelivered on a later poll.
func (c *Consumer) Poll(ctx context.Context, handler func(Record) error) error {
	return c.b.poll(ctx, handler)
}

// Ping checks backend connectivity (used by /readyz).
func (c *Consumer) Ping(ctx context.Context) error { return c.b.ping(ctx) }

// Close leaves the group and closes the consumer.
func (c *Consumer) Close() { c.b.close() }

// NewKafkaProducer connects a Kafka-backed producer to the brokers.
func NewKafkaProducer(brokers []string) (*Producer, error) {
	b, err := newKafkaProducer(brokers)
	if err != nil {
		return nil, err
	}
	return &Producer{b: b}, nil
}

// NewKafkaConsumer joins the group and subscribes to the topics over Kafka.
func NewKafkaConsumer(brokers []string, group string, topics ...string) (*Consumer, error) {
	b, err := newKafkaConsumer(brokers, group, topics...)
	if err != nil {
		return nil, err
	}
	return &Consumer{b: b}, nil
}

// NewRedisProducer connects a Redis Streams-backed producer to addr.
func NewRedisProducer(addr string) (*Producer, error) {
	b, err := newRedisProducer(addr)
	if err != nil {
		return nil, err
	}
	return &Producer{b: b}, nil
}

// NewRedisConsumer joins the group and subscribes to the topics over Redis Streams.
func NewRedisConsumer(addr, group string, topics ...string) (*Consumer, error) {
	b, err := newRedisConsumer(addr, group, topics...)
	if err != nil {
		return nil, err
	}
	return &Consumer{b: b}, nil
}
