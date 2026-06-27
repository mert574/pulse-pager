#!/usr/bin/env bash
# Install/upgrade the in-cluster trace + metrics stack into pulse-system. Idempotent:
# re-run to apply value changes. Needs `helm repo add` for open-telemetry, grafana, and
# prometheus-community first (see README). Prometheus goes up before Tempo (Tempo
# remote-writes the service graph to it); Tempo before the collector, which exports to it.
set -euo pipefail

NS=pulse-system
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

kubectl apply -f "$DIR/namespace.yaml"

# Dashboards as code: build the ConfigMap Grafana loads from the same JSON the dev stack
# uses, so dev and cluster cannot drift. --from-file on the dir picks up every dashboard
# (overview, pipeline, slo). Re-applied each run (idempotent).
kubectl -n "$NS" create configmap pulse-dashboards \
  --from-file="$DIR/../../observability/grafana/dashboards/" \
  --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install pulse-prometheus prometheus-community/prometheus --version 27.5.1 \
  -n "$NS" --create-namespace -f "$DIR/prometheus/values.yaml"

helm upgrade --install pulse-tempo grafana/tempo --version 1.24.4 \
  -n "$NS" --create-namespace -f "$DIR/tempo/values.yaml"

helm upgrade --install pulse-loki grafana/loki --version 6.24.0 \
  -n "$NS" --create-namespace -f "$DIR/loki/values.yaml"

helm upgrade --install pulse-otel-collector open-telemetry/opentelemetry-collector --version 0.159.1 \
  -n "$NS" --create-namespace -f "$DIR/otel-collector/values.yaml"

helm upgrade --install pulse-grafana grafana/grafana --version 10.5.15 \
  -n "$NS" --create-namespace -f "$DIR/grafana/values.yaml"
