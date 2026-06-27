#!/usr/bin/env bash
# Install/upgrade the in-cluster trace stack into pulse-system. Idempotent: re-run to
# apply value changes. Needs `helm repo add` for open-telemetry and grafana first
# (see README). Tempo goes up before the collector, which exports to it.
set -euo pipefail

NS=pulse-system
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

helm upgrade --install pulse-tempo grafana/tempo --version 1.24.4 \
  -n "$NS" --create-namespace -f "$DIR/tempo/values.yaml"

helm upgrade --install pulse-otel-collector open-telemetry/opentelemetry-collector --version 0.159.1 \
  -n "$NS" --create-namespace -f "$DIR/otel-collector/values.yaml"

helm upgrade --install pulse-grafana grafana/grafana --version 10.5.15 \
  -n "$NS" --create-namespace -f "$DIR/grafana/values.yaml"
