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

// Record is a consumed message in the shape the handlers want. The trace context
// is not a field: it is restored from the message headers onto the ctx the handler
// receives (RFC-021 section 4.3), so it flows to logs and onward produce the same
// way it does on an inbound api request.
type Record struct {
	Topic string
	Key   string
	Value []byte
}

// producerBackend is the transport a Producer delegates to (kafka or redis).
type producerBackend interface {
	produce(ctx context.Context, topic, key string, value []byte) error
	ping(ctx context.Context) error
	close()
}

// consumerBackend is the transport a Consumer delegates to. The handler is called
// with a per-record context carrying the restored trace (RFC-021 section 4.3).
type consumerBackend interface {
	poll(ctx context.Context, handler func(context.Context, Record) error) error
	ping(ctx context.Context) error
	lag(ctx context.Context) ([]LagEntry, error)
	close()
}

// LagEntry is one partition's consumer lag (messages behind the high-water mark),
// the primary scale/health signal for the consumer services (RFC-010 section 2.4).
type LagEntry struct {
	Topic     string
	Partition int32
	Lag       int64
}

// Producer publishes messages over the configured backend. sys is the backend name
// ("kafka"/"redis"), used only to tag the produce span (RFC-010 section 4.2).
type Producer struct {
	b   producerBackend
	sys string
}

// Produce synchronously publishes one message, keyed for per-key ordering. It wraps the
// publish in a PRODUCER span so the backend injects this span's context into the message
// headers, which is what links the consumer's span back to here (RFC-010 section 4.2).
func (p *Producer) Produce(ctx context.Context, topic, key string, value []byte) error {
	ctx, span := startProduceSpan(ctx, p.sys, topic)
	defer span.End()
	err := p.b.produce(ctx, topic, key, value)
	if err != nil {
		span.RecordError(err)
	}
	return err
}

// Ping checks backend connectivity (used by /readyz).
func (p *Producer) Ping(ctx context.Context) error { return p.b.ping(ctx) }

// Close flushes and closes the producer.
func (p *Producer) Close() { p.b.close() }

// Consumer reads from a consumer group over the configured backend. sys is the backend
// name ("kafka"/"redis"), used only to tag the consume span (RFC-010 section 4.2).
type Consumer struct {
	b   consumerBackend
	sys string
}

// Poll fetches a batch and calls handler for each record, passing a per-record context
// that carries the trace restored from the message headers (RFC-021 section 4.3). It
// wraps each record in a CONSUMER span (a child of the restored producer span) so the
// handler's work nests under it and the service graph draws the cross-service edge. It
// returns on context cancellation or the first handler error; an errored record is left
// unacked and redelivered on a later poll.
func (c *Consumer) Poll(ctx context.Context, handler func(context.Context, Record) error) error {
	return c.b.poll(ctx, func(recCtx context.Context, rec Record) error {
		recCtx, span := startConsumeSpan(recCtx, c.sys, rec.Topic)
		defer span.End()
		err := handler(recCtx, rec)
		if err != nil {
			span.RecordError(err)
		}
		return err
	})
}

// Ping checks backend connectivity (used by /readyz).
func (c *Consumer) Ping(ctx context.Context) error { return c.b.ping(ctx) }

// Lag returns the per-partition consumer lag for the group (RFC-010 section 2.4). The
// Redis backend has no group-lag concept and returns nil; the runtime polls this to
// drive the pulse_kafka_consumer_lag gauge.
func (c *Consumer) Lag(ctx context.Context) ([]LagEntry, error) { return c.b.lag(ctx) }

// Close leaves the group and closes the consumer.
func (c *Consumer) Close() { c.b.close() }

// NewKafkaProducer connects a Kafka-backed producer to the brokers.
func NewKafkaProducer(brokers []string) (*Producer, error) {
	b, err := newKafkaProducer(brokers)
	if err != nil {
		return nil, err
	}
	return &Producer{b: b, sys: "kafka"}, nil
}

// NewKafkaConsumer joins the group and subscribes to the topics over Kafka.
func NewKafkaConsumer(brokers []string, group string, topics ...string) (*Consumer, error) {
	b, err := newKafkaConsumer(brokers, group, topics...)
	if err != nil {
		return nil, err
	}
	return &Consumer{b: b, sys: "kafka"}, nil
}

// NewRedisProducer connects a Redis Streams-backed producer to addr.
func NewRedisProducer(addr string) (*Producer, error) {
	b, err := newRedisProducer(addr)
	if err != nil {
		return nil, err
	}
	return &Producer{b: b, sys: "redis"}, nil
}

// NewRedisConsumer joins the group and subscribes to the topics over Redis Streams.
func NewRedisConsumer(addr, group string, topics ...string) (*Consumer, error) {
	b, err := newRedisConsumer(addr, group, topics...)
	if err != nil {
		return nil, err
	}
	return &Consumer{b: b, sys: "redis"}, nil
}
