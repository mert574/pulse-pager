# Observability stack (k3s) — trace + metrics + logs pipeline

In-cluster trace + metrics + logs pipeline for the k3s cluster (RFC-011 / RFC-010):

```
services --OTLP traces--> otel-collector (tail sampling) --OTLP--> Tempo --service graph--> Prometheus
services --OTLP logs----> otel-collector ----------------> Loki (OTLP ingest)
services --scrape /metrics------------------------------------------------------>  Prometheus
                                                           Tempo, Prometheus, Loki <-- Grafana
```

Trace and log join both ways in Grafana: a span links to its logs (Tempo
`tracesToLogsV2`), and a log line links to its trace by `trace_id` (Loki derived field).
Prometheus evaluates the recording + alerting rules (RFC-010 sections 5.3, 7) and routes
firing alerts to the bundled Alertmanager. Grafana ships three provisioned dashboards
(overview, pipeline end-to-end, SLO + error budget).

The dev equivalent runs in docker-compose (`make up-obs`, configs in the top-level
`observability/`). This dir is the cluster version: Helm values over the upstream charts.

These are the first k8s/Helm artifacts in the repo. They deploy onto any cluster; the
target is k3s (lightweight, CNCF-conformant), so standard Helm and manifests apply.

## Charts (pinned)

| Component | Chart | Version |
|-----------|-------|---------|
| Prometheus | `prometheus-community/prometheus` | 27.5.1 |
| Collector | `open-telemetry/opentelemetry-collector` | 0.159.1 |
| Tempo | `grafana/tempo` (single binary) | 1.24.4 |
| Loki | `grafana/loki` (single binary) | 6.24.0 |
| Grafana | `grafana/grafana` | 10.5.15 |

The Prometheus chart version is a best guess; confirm it against
`helm search repo prometheus-community/prometheus` and bump if needed before installing.

## Install

```sh
helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts
helm repo add grafana https://grafana.github.io/helm-charts
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

./install.sh        # idempotent: namespace + the five helm upgrade --install
```

`install.sh` creates the `pulse-system` namespace and installs all five with the values
files here. Re-run it to apply changes.

## Point the services at the collector

The app services export to the collector's in-cluster service. On each service's
Deployment (those k8s manifests come with the wider RFC-011 migration; not in this dir
yet) set:

```
PULSE_TRACING_ENABLED=true
PULSE_OTLP_ENDPOINT=pulse-otel-collector.pulse-system.svc.cluster.local:4317
```

## See traces

Exposure (ingress, access control) is deferred until the cluster edge is decided, so
Grafana stays ClusterIP. Reach it by port-forward:

```sh
kubectl -n pulse-system port-forward svc/pulse-grafana 3000:80
# admin password:
kubectl -n pulse-system get secret pulse-grafana -o jsonpath="{.data.admin-password}" | base64 -d ; echo
```

Open http://localhost:3000 and sign in as `admin`. Grafana lands on the provisioned
"Pulse Overview" dashboard (SLO latencies, check/API rates, consumer lag, service up).
For a single trace, go to Explore -> Tempo -> Search by
TraceID, and paste a trace id from an error toast or a log line. The Tempo datasource's
Service Graph tab shows the cross-service map (it reads the service-graph series Tempo
remote-writes to Prometheus); the Prometheus datasource has the per-service metrics.

Prometheus stays ClusterIP too; port-forward it the same way if you want the raw metrics:
`kubectl -n pulse-system port-forward svc/pulse-prometheus-server 9090:80`.

## Notes / deferred

- **Exposure:** no ingress and nothing Cloudflare here yet (deferred). When the edge is
  decided, Grafana goes behind it with access control; the collector and Tempo stay
  cluster-internal regardless.
- **Tempo chart:** the single-binary `tempo` chart is deprecated upstream but is the right
  lightweight fit for k3s. `tempo-distributed` is the non-deprecated path at scale.
- **Storage:** Tempo writes traces to a local-path PVC (k3s default StorageClass). Prod at
  scale moves to an object-store backend; retention is RFC-010 open question 1.
- **Scale:** the collector runs a single replica because tail sampling needs every span of
  a trace on one instance. Scaling needs a load-balancing-exporter tier (RFC-010 §4.5).
