#!/usr/bin/env bash
# Install/upgrade the in-cluster trace + metrics stack into pulse-system. Idempotent:
# re-run to apply value changes. Needs `helm repo add` for open-telemetry, grafana, and
# prometheus-community first (see README). Prometheus goes up before Tempo (Tempo
# remote-writes the service graph to it); Tempo before the collector, which exports to it.
set -euo pipefail

NS=pulse-system
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

helm upgrade --install pulse-prometheus prometheus-community/prometheus --version 27.5.1 \
  -n "$NS" --create-namespace -f "$DIR/prometheus/values.yaml"

helm upgrade --install pulse-tempo grafana/tempo --version 1.24.4 \
  -n "$NS" --create-namespace -f "$DIR/tempo/values.yaml"

helm upgrade --install pulse-otel-collector open-telemetry/opentelemetry-collector --version 0.159.1 \
  -n "$NS" --create-namespace -f "$DIR/otel-collector/values.yaml"

helm upgrade --install pulse-grafana grafana/grafana --version 10.5.15 \
  -n "$NS" --create-namespace -f "$DIR/grafana/values.yaml"
