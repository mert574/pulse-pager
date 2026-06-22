package bus

import (
	"context"

	"github.com/twmb/franz-go/pkg/kgo"

	"pulse/internal/obs"
)

// kafkaProducer publishes to a Kafka-compatible broker via franz-go.
type kafkaProducer struct{ cl *kgo.Client }

func newKafkaProducer(brokers []string) (*kafkaProducer, error) {
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
	return &kafkaProducer{cl: cl}, nil
}

func (p *kafkaProducer) produce(ctx context.Context, topic, key string, value []byte) error {
	rec := &kgo.Record{Topic: topic, Key: []byte(key), Value: value}
	if id := obs.CorrelationID(ctx); id != "" {
		rec.Headers = append(rec.Headers, kgo.RecordHeader{Key: correlationHeader, Value: []byte(id)})
	}
	return p.cl.ProduceSync(ctx, rec).FirstErr()
}

func (p *kafkaProducer) ping(ctx context.Context) error { return p.cl.Ping(ctx) }
func (p *kafkaProducer) close()                         { p.cl.Close() }

// kafkaConsumer reads from a consumer group via franz-go.
type kafkaConsumer struct{ cl *kgo.Client }

func newKafkaConsumer(brokers []string, group string, topics ...string) (*kafkaConsumer, error) {
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
	return &kafkaConsumer{cl: cl}, nil
}

func (c *kafkaConsumer) poll(ctx context.Context, handler func(Record) error) error {
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
		herr = handler(toKafkaRecord(r))
	})
	return herr
}

func (c *kafkaConsumer) ping(ctx context.Context) error { return c.cl.Ping(ctx) }
func (c *kafkaConsumer) close()                         { c.cl.Close() }

func toKafkaRecord(r *kgo.Record) Record {
	rec := Record{Topic: r.Topic, Key: string(r.Key), Value: r.Value}
	for _, h := range r.Headers {
		if h.Key == correlationHeader {
			rec.CorrelationID = string(h.Value)
		}
	}
	return rec
}
