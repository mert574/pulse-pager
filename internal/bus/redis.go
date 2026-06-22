package bus

import (
	"context"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"pulse/internal/obs"
)

const (
	// Approximate cap per stream so a backlog cannot grow without bound on a small
	// node. In normal operation events are consumed within seconds; this only bounds
	// a pathological backlog. Approximate (~) trimming is cheap.
	redisStreamMaxLen = 10000
	// How long a poll blocks waiting for new entries before returning, so the caller's
	// loop can re-check context and run the periodic reclaim.
	redisBlock = 2 * time.Second
	// Max entries fetched or claimed per round.
	redisBatch = 64
	// A delivered-but-unacked entry idle longer than this is reclaimed from a dead
	// consumer. Longer than any normal handler (a check runs within its own timeout),
	// so a slow-but-alive handler is never double-delivered.
	redisReclaimMinIdle = 60 * time.Second
	// How often a poll runs the dead-consumer reclaim safety net.
	redisReclaimEvery = 30 * time.Second
)

// redisProducer publishes events as Redis stream entries (one stream per topic).
type redisProducer struct{ rdb *redis.Client }

func newRedisProducer(addr string) (*redisProducer, error) {
	return &redisProducer{rdb: redis.NewClient(&redis.Options{Addr: addr})}, nil
}

func (p *redisProducer) produce(ctx context.Context, topic, key string, value []byte) error {
	vals := map[string]any{"key": key, "value": value}
	if id := obs.CorrelationID(ctx); id != "" {
		vals[correlationHeader] = id
	}
	return p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: topic,
		MaxLen: redisStreamMaxLen,
		Approx: true,
		Values: vals,
	}).Err()
}

func (p *redisProducer) ping(ctx context.Context) error { return p.rdb.Ping(ctx).Err() }
func (p *redisProducer) close()                         { _ = p.rdb.Close() }

// redisConsumer reads a consumer group over Redis Streams. It assumes a single live
// consumer per group (the single-node deployment this backend is for), so it uses a
// stable consumer name and recovers its own pending entries directly.
type redisConsumer struct {
	rdb      *redis.Client
	group    string
	consumer string
	topics   []string

	lastReclaim time.Time
}

func newRedisConsumer(addr, group string, topics ...string) (*redisConsumer, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx := context.Background()
	for _, t := range topics {
		// MKSTREAM creates the stream if missing; "0" so a fresh group reads existing
		// history, matching the Kafka backend (consumers are idempotent, RFC-002 6).
		if err := rdb.XGroupCreateMkStream(ctx, t, group, "0").Err(); err != nil && !isBusyGroup(err) {
			_ = rdb.Close()
			return nil, err
		}
	}
	return &redisConsumer{rdb: rdb, group: group, consumer: group + "-c", topics: topics}, nil
}

func isBusyGroup(err error) bool {
	return err != nil && strings.Contains(err.Error(), "BUSYGROUP")
}

func (c *redisConsumer) poll(ctx context.Context, handler func(Record) error) error {
	// 1. Reprocess this consumer's own pending entries first. On startup these are
	//    whatever a previous run was delivered but did not ack (crash recovery); in
	//    steady state they are entries whose handler returned an error and must be
	//    retried. Reading id "0" returns them oldest-first, so a failing entry is
	//    retried before new work, matching the Kafka "stop at the first error" behavior.
	for _, t := range c.topics {
		if err := c.readInto(ctx, t, "0", -1, handler); err != nil {
			return err
		}
	}

	// 2. Safety net: reclaim entries stranded on a dead consumer (for example a name
	//    change across a redeploy). A single-node deployment keeps a stable name, so
	//    this rarely fires, but it guarantees no entry is orphaned.
	if time.Since(c.lastReclaim) >= redisReclaimEvery {
		c.lastReclaim = time.Now()
		for _, t := range c.topics {
			if err := c.reclaim(ctx, t, handler); err != nil {
				return err
			}
		}
	}

	// 3. Read new (never-delivered) entries across all topics, blocking briefly.
	return c.readNew(ctx, handler)
}

// readInto reads from one stream at the given id ("0" = own pending, ">" = new) and
// dispatches each entry. block < 0 means do not block.
func (c *redisConsumer) readInto(ctx context.Context, topic, id string, block time.Duration, handler func(Record) error) error {
	res, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    c.group,
		Consumer: c.consumer,
		Streams:  []string{topic, id},
		Count:    redisBatch,
		Block:    block,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil
		}
		return err
	}
	return c.dispatchStreams(ctx, res, handler)
}

func (c *redisConsumer) readNew(ctx context.Context, handler func(Record) error) error {
	streams := make([]string, 0, len(c.topics)*2)
	streams = append(streams, c.topics...)
	for range c.topics {
		streams = append(streams, ">")
	}
	res, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    c.group,
		Consumer: c.consumer,
		Streams:  streams,
		Count:    redisBatch,
		Block:    redisBlock,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil // nothing new within the block window
		}
		return err
	}
	return c.dispatchStreams(ctx, res, handler)
}

func (c *redisConsumer) reclaim(ctx context.Context, topic string, handler func(Record) error) error {
	start := "0-0"
	for {
		msgs, next, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   topic,
			Group:    c.group,
			Consumer: c.consumer,
			MinIdle:  redisReclaimMinIdle,
			Start:    start,
			Count:    redisBatch,
		}).Result()
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if err := c.dispatch(ctx, topic, m, handler); err != nil {
				return err
			}
		}
		if next == "0-0" || next == "" {
			return nil
		}
		start = next
	}
}

func (c *redisConsumer) dispatchStreams(ctx context.Context, streams []redis.XStream, handler func(Record) error) error {
	for _, st := range streams {
		for _, m := range st.Messages {
			if err := c.dispatch(ctx, st.Stream, m, handler); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *redisConsumer) dispatch(ctx context.Context, stream string, m redis.XMessage, handler func(Record) error) error {
	if err := handler(toRedisRecord(stream, m)); err != nil {
		return err // leave unacked: retried from the pending list on a later poll
	}
	return c.rdb.XAck(ctx, stream, c.group, m.ID).Err()
}

func (c *redisConsumer) ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }
func (c *redisConsumer) close()                         { _ = c.rdb.Close() }

func toRedisRecord(stream string, m redis.XMessage) Record {
	rec := Record{Topic: stream}
	if v, ok := m.Values["key"].(string); ok {
		rec.Key = v
	}
	if v, ok := m.Values["value"].(string); ok {
		rec.Value = []byte(v)
	}
	if v, ok := m.Values[correlationHeader].(string); ok {
		rec.CorrelationID = v
	}
	return rec
}
