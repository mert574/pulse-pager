# ADR-0003: Kafka client is franz-go

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-000 section 5, RFC-002 section 2, ADR-0009

## Context
All services build with CGO_ENABLED=0 into static distroless images. The bus needs an idempotent producer so a produce retry does not double-append (ADR-0009 leans on this), and clean consumer-group handling so workers, alerting, and notifier can scale by adding group members on lag without stop-the-world rebalances. We pick one Go Kafka client for internal/bus.

## Options considered
- franz-go - pure Go, idempotent producer on by default, supports cooperative-sticky rebalance, actively maintained with current protocol support.
- sarama - pure Go and widely used, but idempotent-producer and consumer-group paths are the rough edges (manual offset handling, sharper rebalance).
- confluent-kafka-go - most battle-tested protocol via librdkafka, but cgo, which breaks the static distroless build and cross-compiles and adds a C toolchain to CI.
- segmentio/kafka-go - pure Go and ergonomic, but idempotent-producer and rebalance maturity lag franz-go.

## Decision
Use github.com/twmb/franz-go as the single Go Kafka client, wrapped by internal/bus. Enable the idempotent producer at client construction and use cooperative-sticky group balancing.

## Consequences
The idempotent producer is the default rather than a careful opt-in, so produce retries stay safe with no config dance, and the static CGO_ENABLED=0 build story is preserved. Cooperative-sticky balancing reassigns only some partitions when an HPA adds a pod, keeping rebalances cheap. internal/bus hides raw franz-go types so envelope, headers, keying, trace propagation, and commit-after-process are uniform across services. We accept franz-go over the more battle-tested librdkafka because cgo conflicts directly with the build constraint. Revisit only if a franz-go limitation appears at scale or if a feature we need lands first elsewhere.
