package bus

import (
	"context"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
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
	for k, v := range injectTrace(ctx) {
		rec.Headers = append(rec.Headers, kgo.RecordHeader{Key: k, Value: []byte(v)})
	}
	return p.cl.ProduceSync(ctx, rec).FirstErr()
}

func (p *kafkaProducer) ping(ctx context.Context) error { return p.cl.Ping(ctx) }
func (p *kafkaProducer) close()                         { p.cl.Close() }

// kafkaConsumer reads from a consumer group via franz-go.
type kafkaConsumer struct {
	cl    *kgo.Client
	group string
}

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
	return &kafkaConsumer{cl: cl, group: group}, nil
}

// lag reports the group's per-partition lag via the admin client (RFC-010 section 2.4).
func (c *kafkaConsumer) lag(ctx context.Context) ([]LagEntry, error) {
	lags, err := kadm.NewClient(c.cl).Lag(ctx, c.group)
	if err != nil {
		return nil, err
	}
	dl, ok := lags[c.group]
	if !ok {
		return nil, nil
	}
	if dl.FetchErr != nil {
		return nil, dl.FetchErr
	}
	var out []LagEntry
	for _, l := range dl.Lag.Sorted() {
		if l.Lag < 0 {
			continue // no committed offset yet; not meaningful lag
		}
		out = append(out, LagEntry{Topic: l.Topic, Partition: l.Partition, Lag: l.Lag})
	}
	return out, nil
}

func (c *kafkaConsumer) poll(ctx context.Context, handler func(context.Context, Record) error) error {
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
		recCtx := restoreTrace(ctx, kafkaHeaders(r))
		herr = handler(recCtx, toKafkaRecord(r))
	})
	return herr
}

func (c *kafkaConsumer) ping(ctx context.Context) error { return c.cl.Ping(ctx) }
func (c *kafkaConsumer) close()                         { c.cl.Close() }

func toKafkaRecord(r *kgo.Record) Record {
	return Record{Topic: r.Topic, Key: string(r.Key), Value: r.Value}
}

// kafkaHeaders flattens the record headers into a map the propagator can read.
func kafkaHeaders(r *kgo.Record) map[string]string {
	if len(r.Headers) == 0 {
		return nil
	}
	h := make(map[string]string, len(r.Headers))
	for _, hdr := range r.Headers {
		h[hdr.Key] = string(hdr.Value)
	}
	return h
}
