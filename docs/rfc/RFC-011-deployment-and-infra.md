# RFC-011 - Deployment and Infra

Status: DRAFT for review
Author: Principal Platform / Infrastructure
Audience: every service author, plus whoever operates the clusters, the managed stores, and the CI/CD pipeline.
Owns (per RFC-000 section 13): how every service is packaged, deployed, scaled, and operated, including the regional data planes. Concretely: the leader-election runtime, the managed-infra vendor choices, the migration Job wiring, TLS and wildcard/custom-domain cert handling, the service-to-service trust runtime (NetworkPolicy + TLS), secrets/KMS sourcing, CI/CD + GitOps, environments, IaC, and DR / multi-region ops.
Parent: `docs/rfc/RFC-000-architecture-overview.md` (section 4 topology, section 7.2 service-to-service trust, section 10 security, section 11 deployment, ADR-0004 leader election, ADR-0006 multi-region messaging).
Informed by: RFC-001 (migration Job, BYPASSRLS role, backup/PITR, read replicas), RFC-002 (regional Kafka + MirrorMaker 2), RFC-010 (deploy the observability stack).
Product source: `PRD.md` section 12 (99.9% SLA, multi-region posture), `PRD-004` (status pages stay up + custom-domain wildcard TLS), `PRD-007` (regions topology).

House style: all timestamps are RFC3339 UTC. No em-dashes. Tables and YAML over prose. "control/limit" not "gate", "incident review" not "postmortem", "shut down" not "tear down".

---

## 1. Overview, scope, owned contracts

RFC-000 fixed the deployment decisions at the architecture level and handed RFC-011 the full design. This RFC is the single source of truth for the runtime that every service deploys into. Nothing here re-litigates a locked decision (Go microservices in one module, managed Postgres/Redis/Kafka, k8s, nginx-fronted Lit SPA, the control-plane / regional-data-plane split); it makes those decisions concrete and operable.

### 1.1 Owned contracts

| Contract | What this RFC fixes |
|----------|---------------------|
| Leader-election runtime | The scheduler runs as a multi-replica Deployment that elects one active leader via a `coordination.k8s.io/Lease` (RFC-000 ADR-0004). This RFC fixes the RBAC, replica count, lease timings, and failover behavior. |
| Managed-vs-self-run | Vendor and posture per dependency: Postgres, Redis, Kafka (control plane and regional), observability stack. |
| Migration Job | The `cmd/migrate` Kubernetes Job runs as a Helm pre-upgrade hook before any service rollout, connecting as the BYPASSRLS migration role (RFC-001 section 8). |
| TLS / wildcard certs | cert-manager + Let's Encrypt for app TLS; a wildcard cert for `*.pulse.app` status-page subdomains; on-demand per-customer certs for custom domains. |
| Service-to-service trust runtime | Default-deny NetworkPolicy plus explicit allows, TLS to every infra endpoint, worker egress controls for SSRF defense at the network layer. No service mesh in v1. |
| Secrets / KMS | k8s Secrets backed by a cloud KMS via the external-secrets operator; sourcing and rotation stance for `PULSE_SECRET_KEY`, the JWT signing key, OIDC client secrets, and DB/Kafka/Redis credentials. |
| CI/CD + GitOps | Build, test (incl. the cross-tenant isolation suite), scan, push, then Argo CD reconciles. Migration Job ordering and rollback. |

### 1.2 Out of scope (owned elsewhere)

| Topic | Owner |
|-------|-------|
| Region failover mechanics, cost-aware scheduling, region health detail | RFC-008 |
| Observability stack internals (dashboards, alert rules, SLO recording rules) | RFC-010 (this RFC only deploys it) |
| Postgres schema, RLS policy DDL, partition mechanics | RFC-001 |
| Kafka topic schemas, partition counts, MM2 topic mapping | RFC-002 (this RFC provisions the clusters and runs MM2) |
| Frontend build pipeline internals | RFC-013 (this RFC builds and serves the artifact via nginx) |

---

## 2. Container images

One image per `cmd/<service>` plus one nginx image for the SPA and status-page serving. All Go images are multi-stage, build with `CGO_ENABLED=0`, and ship a static binary into a minimal non-root final layer.

### 2.1 Per-service Go Dockerfile (one template, parameterized by `SERVICE`)

```dockerfile
# syntax=docker/dockerfile:1.7
# ---- build stage ----
FROM golang:1.23 AS build
WORKDIR /src
# cache modules separately from source for reproducible, fast rebuilds
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG SERVICE
ARG VERSION=dev
ARG COMMIT=unknown
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/app ./cmd/${SERVICE}

# ---- final stage: distroless static, non-root ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
```

Decisions and reasoning:

| Choice | Reasoning |
|--------|-----------|
| `distroless/static-debian12:nonroot` final | The binary is static (`CGO_ENABLED=0`), so it needs no libc. Distroless has no shell and no package manager, which shrinks the attack surface and the image (single-digit MB plus the binary). The `:nonroot` tag runs as uid 65532, not root. Alternative `alpine` rejected: it ships a shell and busybox we do not need and pulls musl into the picture; distroless is smaller and harder to exploit. |
| `-trimpath`, pinned base digest, no timestamps in build | Reproducible builds. CI pins the base image by digest, not floating tag, and stamps `VERSION`/`COMMIT` via ldflags only. Two builds of the same commit produce the same binary. |
| Build cache mounts | Fast incremental CI without breaking reproducibility (module and build cache are inputs, not embedded in the image). |
| One Dockerfile, `--build-arg SERVICE=<svc>` | Five binaries share one module and one Dockerfile; CI builds api, scheduler, worker, alerting, notifier from the same template. Avoids five near-identical files drifting. |

### 2.2 nginx image (SPA + status pages)

```dockerfile
# ---- SPA build ----
FROM node:20 AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ .
RUN npm run build            # -> /web/dist

# ---- nginx final ----
FROM nginxinc/nginx-unprivileged:1.27-alpine
COPY --from=web /web/dist /usr/share/nginx/html
COPY deploy/docker/nginx/default.conf /etc/nginx/conf.d/default.conf
# unprivileged image already runs as non-root and binds 8080
```

The `nginx-unprivileged` variant runs as non-root and listens on 8080, which fits a `readOnlyRootFilesystem` pod with an `emptyDir` for nginx cache and temp paths. The SPA is built static and served directly; nginx proxies `/api` to the api Service and serves status pages cache-first (section 5.4).

### 2.3 Pod-level hardening (applies to every Deployment, set in the Helm base template)

```yaml
securityContext:                 # pod
  runAsNonRoot: true
  seccompProfile: { type: RuntimeDefault }
containers:
  - name: app
    securityContext:             # container
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities: { drop: ["ALL"] }
```

Go services need no writable rootfs. nginx mounts `emptyDir` at `/tmp` and the cache path. Worker pods that need outbound checks still keep `readOnlyRootFilesystem` (they write nothing to disk).

### 2.4 Image supply chain in CI

| Step | Tool | Fails the build on |
|------|------|--------------------|
| SBOM generation | `syft` (CycloneDX) attached to the image | n/a (artifact) |
| Vulnerability scan | `grype` / `trivy` on the built image | any HIGH/CRITICAL fixable CVE |
| Image signing | `cosign` keyless (OIDC) signature + SBOM attestation | unsigned image cannot deploy (admission policy verifies the signature) |
| Base image pin | renovate keeps the digest current via PRs | n/a |

Distroless plus a static binary keeps the scan surface tiny: most CVEs come from OS packages the image does not have.

---

## 3. Kubernetes topology

### 3.1 Clusters and namespaces

Two cluster classes, separate clusters for prod isolation (RFC-000 11.1 leaned this way; this RFC confirms it).

| Cluster | Region | Runs |
|---------|--------|------|
| Control-plane cluster | home region | `pulse-control` (api, scheduler, alerting, notifier), `pulse-system` (observability, external-secrets, cert-manager, ingress, central Kafka clients), and the migration Job. |
| Regional data-plane cluster (one per operated region) | each region in PRD-007 (`us-east`, `eu-west`, `ap-southeast`, ...) | `pulse-region-<code>` (workers only) plus the regional Kafka clients and MM2. No durable state, no product decision (RFC-000 section 1.2). |

Environments are separate clusters per env for prod (`pulse-prod-*`); staging and dev share a smaller cluster split by namespace (section 10). At Phase 0/1 there is one region (home), so the only data-plane workers run alongside the control plane; the per-region cluster shape exists from day one so adding a region is additive, never a migration (RFC-000 section 4.2).

### 3.2 Deployments (control plane)

| Service | Replicas (prod baseline) | Scaling | Singleton? |
|---------|--------------------------|---------|-----------|
| api | 3 | HPA on CPU + RPS (section 4) | no |
| alerting | 3 | KEDA on `check.results` lag (section 4) | no (rollup + partition task is leader-elected inside it, RFC-001 6.3) |
| notifier | 2 | KEDA on `notify.events` + `webhook.delivery` lag | no |
| scheduler | 2 | not scaled for throughput; leader-elected | yes (one active leader, one warm standby) |
| nginx (SPA/ingress backend) | 3 | HPA on CPU/RPS | no |

Workers run in the regional clusters (section 3.5).

### 3.3 Scheduler: leader-elected Deployment (NOT a single replica)

The scheduler is a Deployment of 2 replicas (not `replicas: 1`). A single replica would mean a scheduling gap for the full pod-reschedule time on any node failure; two replicas with a Lease give a warm standby that acquires leadership within the lease timeout and rebuilds its heap from Postgres (RFC-000 section 2.2 failure behavior). It uses `client-go`'s `leaderelection` with a `coordination.k8s.io/Lease` lock (ADR-0004).

Lease timings (the standard safe ratio, leaseDuration > renewDeadline > retryPeriod):

| Param | Value | Meaning |
|-------|-------|---------|
| `leaseDuration` | 15s | how long a non-leader waits before trying to acquire a stale lease |
| `renewDeadline` | 10s | the leader must renew within this or it steps down |
| `retryPeriod` | 2s | how often the renew/acquire loop runs |

Failover: if the leader pod dies or its node partitions, the lease is not renewed; after `leaseDuration` the standby acquires it and starts dispatching. Worst-case scheduling gap is bounded by `leaseDuration` plus the heap rebuild, which sits inside the 5s scheduling-accuracy SLO budget after recovery (RFC-000 section 2.2). The active leader stops dispatching the instant it loses the lease (`OnStoppedLeading` cancels the dispatch context), so two leaders never double-dispatch.

RBAC for the Lease (namespaced Role, not cluster-wide):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: scheduler-leaderelection
  namespace: pulse-control
rules:
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "create", "update"]
    # resourceNames omitted on create; tighten get/update to the lease name once it exists
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]      # leaderelection emits k8s events on transitions
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: scheduler-leaderelection
  namespace: pulse-control
subjects:
  - kind: ServiceAccount
    name: scheduler
    namespace: pulse-control
roleRef: { kind: Role, name: scheduler-leaderelection, apiGroup: rbac.authorization.k8s.io }
```

The scheduler ServiceAccount gets exactly these verbs and nothing else. It does not need to list pods or read other resources; leadership is the only k8s API it touches.

### 3.4 Probes per service

Probe semantics: `startup` protects slow boots (scheduler heap rebuild, Kafka consumer-group join), `readiness` controls traffic and rollout, `liveness` restarts a wedged pod. All probes hit a local HTTP handler in `internal/obs`.

| Service | startup | readiness | liveness |
|---------|---------|-----------|----------|
| api | `/healthz` initialDelay 0, failureThreshold 30 @ 1s | `/readyz` (DB primary reachable, Redis reachable) period 5s | `/healthz` period 10s |
| scheduler | `/healthz` failureThreshold 60 @ 1s (heap rebuild can take a moment) | `/readyz` returns 200 only on the leader once the heap is built; standbys return 503 (not Ready, no traffic, but kept alive) | `/healthz` period 10s |
| worker | `/healthz` failureThreshold 30 @ 1s | `/readyz` (Kafka consumer assigned) period 5s | `/healthz` period 10s |
| alerting | `/healthz` failureThreshold 30 @ 1s | `/readyz` (DB + Kafka consumer assigned) period 5s | `/healthz` period 10s |
| notifier | same as alerting | `/readyz` (Kafka consumer assigned) period 5s | `/healthz` period 10s |
| nginx | tcp 8080 | `/healthz` (nginx stub) | tcp 8080 |

Note on the scheduler readiness handler: standbys are deliberately Ready=false. They are healthy (liveness passes, the pod stays up) but not Ready, so a Service or HPA would not route to them. The scheduler exposes no traffic Service anyway; this just keeps "ready" honest as "this is the active leader".

`/readyz` failing must never restart the pod (that is what the separate `/healthz` liveness path is for). A flapping dependency should drop a pod from rotation, not crash-loop it.

### 3.5 Regional worker Deployment

```yaml
# pulse-region-<code>, workers only, no durable state
kind: Deployment
spec:
  replicas: 4                       # KEDA-managed min, scales on regional check.jobs lag
  template:
    spec:
      topologySpreadConstraints:
        - maxSkew: 1
          topologyKey: topology.kubernetes.io/zone
          whenUnsatisfiable: ScheduleAnyway
          labelSelector: { matchLabels: { app: worker } }
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 100
              podAffinityTerm:
                topologyKey: kubernetes.io/hostname
                labelSelector: { matchLabels: { app: worker } }
```

Workers consume their region's `check.jobs.<region>` from the regional Kafka cluster (local, low-latency, RFC-002 section 7) and produce `check.results` + `region.health` into that regional cluster, which MM2 mirrors home (section 12.4).

### 3.6 PodDisruptionBudgets, resources, spread

PDBs so a voluntary disruption (node drain, upgrade) never takes a service to zero:

| Service | PDB |
|---------|-----|
| api, alerting, nginx, worker | `minAvailable: 50%` |
| notifier | `minAvailable: 1` |
| scheduler | `maxUnavailable: 1` (allows draining the standby or the leader one at a time; the survivor keeps or acquires the lease) |

Resource requests/limits (prod baseline, tuned per RFC-010 load tests):

| Service | requests cpu/mem | limits cpu/mem |
|---------|------------------|----------------|
| api | 250m / 256Mi | 1 / 512Mi |
| scheduler | 250m / 256Mi | 1 / 512Mi |
| worker | 200m / 128Mi | 1 / 256Mi |
| alerting | 500m / 512Mi | 2 / 1Gi |
| notifier | 200m / 128Mi | 1 / 256Mi |
| nginx | 100m / 64Mi | 500m / 128Mi |

CPU limits are set generously (or omitted for api/alerting if CPU-throttling hurts tail latency, a tuning call left to RFC-010); memory limits are firm because the Go services have bounded working sets. Every Deployment carries `topologySpreadConstraints` across zones and a preferred host anti-affinity so one node or zone loss does not remove a whole service.

---

## 4. HPA / autoscaling

### 4.1 Signal per service

| Service | Primary signal | Secondary | Why |
|---------|----------------|-----------|-----|
| worker | `check.jobs.<region>` Kafka consumer lag | CPU | Lag directly measures "checks waiting to run" (RFC-000 2.3). CPU alone lags reality because a worker can be lag-bound on a slow endpoint without being CPU-bound. |
| alerting | `check.results` consumer lag | CPU | Sustained lag threatens the 5s result-to-decision SLO (RFC-002 section 8). |
| notifier | `notify.events` + `webhook.delivery` lag | - | Notification latency is the user-visible SLO. |
| api | requests-per-second (Prometheus) | CPU + p95 in-flight | api is request-driven, not queue-driven. |
| scheduler | none | none | Singleton; does not scale for throughput (RFC-000 2.2). |

### 4.2 Decision: KEDA for the lag-based services, plain HPA for api

Decision: use **KEDA** for worker, alerting, and notifier (Kafka-lag-driven), and a plain Kubernetes `HorizontalPodAutoscaler` for api and nginx (CPU + RPS via the Prometheus adapter is enough there).

| Option | Verdict |
|--------|---------|
| KEDA (`kafka` scaler reads consumer-group lag directly) | Chosen for lag-driven services. It speaks Kafka consumer-group lag natively, supports scale-to/from a sensible floor, and generates the underlying HPA for us. One config block per ScaledObject, no custom metrics pipeline to build and keep alive. It is the standard tool for "scale Kafka consumers on lag". |
| Raw HPA + custom-metrics adapter (e.g. Prometheus adapter exposing lag as an external metric) | Rejected for lag-driven services. It works but means standing up and operating the metrics adapter, writing the PromQL that turns broker lag into a metric, and keeping that pipeline healthy as another failure mode in the scaling loop. KEDA's Kafka scaler removes all of that. We still run the Prometheus adapter for api RPS, where the metric is genuinely a Prometheus query, not broker lag. |

Justification for the split: the lag services have a first-class native signal (consumer-group lag) that KEDA reads straight from the broker, so KEDA is strictly simpler and more direct than synthesizing the same number through Prometheus. api's signal (RPS, p95) is a real Prometheus query with no native k8s equivalent, so the Prometheus adapter + plain HPA is the right fit there. Running KEDA does not preclude plain HPAs; KEDA only manages the objects it owns.

### 4.3 Min/max and scale behavior

| Service | min | max | Lag target / signal | Scale-up | Scale-down |
|---------|-----|-----|---------------------|----------|------------|
| worker (per region) | 4 | up to regional `check.jobs` partition count | lag threshold ~2000 msgs/partition | fast: `+100%` or `+4 pods` per 30s, no stabilization | slow: 300s stabilization, `-1 pod` per 60s |
| alerting | 3 | 128 (the `check.results` partition count, RFC-002) | lag threshold ~5000 | fast | 300s stabilization |
| notifier | 2 | 24 | lag threshold ~1000 | fast | 300s stabilization |
| api | 3 | 20 | 65% CPU + RPS target | `+50%` per 60s | 300s stabilization, `-10%` per 60s |

Scale-up is aggressive (a backlog or traffic spike is the cost), scale-down is conservative with a long stabilization window so a brief lull does not churn pods. A consumer cannot use more pods than partitions, so the max is capped at the partition count (RFC-002 section 9): extra pods past that would idle.

---

## 5. Ingress and TLS

### 5.1 Ingress controller and TLS termination

| Choice | Value |
|--------|-------|
| Ingress controller | ingress-nginx (matches the nginx already serving the SPA; RFC-000 11.1). It terminates TLS, proxies `/api` to the api Service, and routes status-page hosts to the status-page serving path. |
| Cert management | cert-manager with Let's Encrypt (ACME). |

Decision: **cert-manager + Let's Encrypt**, not manual certs or a paid CA. Reasoning: ACME automation (issue, renew, rotate) is exactly what status-page custom domains need at scale (one cert per customer domain), and cert-manager is the standard k8s controller for it. A paid wildcard CA still does not solve the per-customer-domain case, which must be automatic.

### 5.2 The three TLS surfaces

| Surface | Host pattern | Cert strategy | ACME challenge |
|---------|--------------|---------------|----------------|
| App (dashboard, API) | `app.pulse.app`, `api.pulse.app` | single cert per host | HTTP-01 |
| Status-page subdomains | `{org-slug}.pulse.app` | one **wildcard** cert `*.pulse.app` (PRD-004, RFC-000 11.1) | DNS-01 (wildcards require DNS-01; cert-manager drives the DNS provider) |
| Custom domains | `status.customer.com` (customer CNAMEs to us) | one cert **per customer domain**, issued on demand | HTTP-01 once the CNAME resolves to us |

### 5.3 Custom-domain / on-demand TLS flow (PRD-004 section 6, phased)

1. Owner/admin adds `status.customer.com` in the status-page editor (RBAC per PRD-004; entitlement `custom_domain_not_in_plan` checked in api per RFC-000 section 12).
2. Pulse shows the required CNAME target (`status.customer.com` -> `cname.pulse.app`).
3. The customer creates the CNAME at their DNS provider.
4. api writes a `Certificate` (or annotates an Ingress) for that host; cert-manager runs the HTTP-01 challenge over the now-resolving CNAME and issues the cert.
5. ingress-nginx serves `status.customer.com` with the per-domain cert, renewed automatically.

This stays additive: the per-org subdomain shape (`{org-slug}.pulse.app`) sets custom domains up naturally (PRD-004 section 2.1), and a slug rename only changes the wildcard-covered host, never a cert.

### 5.4 Status-page serving resilience (PRD-004 section 8, PRD master 12)

The product requirement is that a status page stays up during the customer's own incident, independent of the write paths. The serving path is deliberately separate and cache-first:

| Layer | Behavior |
|-------|----------|
| Edge cache | ingress-nginx caches the public status-page projection (the read of already-computed status + uptime rollups + incidents, PRD-004 section 3.6) with a short TTL (e.g. 30-60s) and `stale-while-revalidate`, so a traffic spike during a high-profile outage does not reach the app on every request. |
| Read routing | status-page reads go to a Postgres **read replica** (RFC-001 section 10), never the primary, so a slow or degraded write path does not affect serving. The data is a derived view and lag-tolerant (RFC-000 section 8). |
| Failure stance | if the app read fails, nginx serves the last cached projection (`proxy_cache_use_stale`) rather than an error. Coverage-degraded is never surfaced publicly (PRD-004 section 7); the page keeps the last known verdict. |

The status-page path shares ingress-nginx with the app but is a distinct location block with its own cache zone and replica-only upstream, so its availability does not depend on api write health.

---

## 6. Managed vs self-run infra

Stance (RFC-000 11.3 locked managed for v1; this RFC picks vendors and decides per dependency). The phrasing is cloud-portable; the table names AWS-first defaults with the Google equivalent, because the choice is "managed", not "this exact SKU".

| Dependency | Decision | Vendor (AWS / GCP) | Reasoning |
|------------|----------|--------------------|-----------|
| Postgres (control plane) | Managed | RDS for PostgreSQL Multi-AZ / Cloud SQL HA | The 99.9% SLA needs HA failover and PITR we do not want to operate by hand. Managed gives automated failover, daily backups, 7-day PITR, and read replicas (RFC-001 section 9, 10) out of the box. We are a monitoring company, not a database team. |
| Postgres read replicas | Managed | RDS read replicas / Cloud SQL read replicas | Status-page and history reads route here (RFC-001 section 10). Managed replicas track the primary with managed replication. |
| Redis (cache, locks, rate-limit, dedup) | Managed | ElastiCache for Redis / Memorystore | Redis is fail-open for us (RFC-000 11.2): a blip degrades, it does not break correctness (singleton-ness is on the k8s Lease, not Redis). Managed HA Redis is cheap and removes cluster operation. |
| Kafka (control plane / central cluster) | Managed | MSK / Confluent Cloud | The central cluster carries the firehose (`check.results` after mirror, ~10-60k/sec, RFC-002 section 9) plus control topics. Managed brokers give us the durability and the 99.9% posture without running a Kafka platform. |
| Kafka (regional data-plane clusters) | Managed where the region has a managed offering; self-run via Strimzi only where it does not | MSK / Confluent Cloud per region; Strimzi operator fallback | See decision below. |
| Observability stack (Prometheus, Grafana, OTel collector, tracing backend) | Self-run in-cluster via the kube-prometheus-stack Helm chart (RFC-010 owns config) | n/a | The stack is stateless-ish and standard to run in-cluster; a managed APM would cost more than its operational saving at our size, and RFC-010 wants control over recording rules and retention. Long-term metric/trace storage can move to a managed object-store backend later without changing the in-cluster collection. |

Regional Kafka decision (the one genuine fork):

| Option | Verdict |
|--------|---------|
| Managed Kafka per region (MSK / Confluent Cloud) | Preferred where the operated region has a managed offering. Same operational savings as the central cluster, and the cloud provider exposes managed MirrorMaker so the mirror home is also managed (RFC-002 section 7.2). |
| Self-run via the Strimzi operator in the regional k8s cluster | Chosen as the fallback only for a region where no acceptable managed Kafka exists (a residency or coverage-driven region the provider does not offer managed Kafka in). Strimzi is the standard Kafka operator and keeps the regional cluster shaped like the others; it adds operational load we accept only when forced. |
| Self-run everywhere (Strimzi for all clusters) | Rejected. It contradicts RFC-000 11.3 and puts us in the Kafka-operations business for cost savings that do not justify the on-call burden at v1 scale. |

Why a regional broker at all (not central with cross-region consume): locked by RFC-000 ADR-0006 / RFC-002 section 7. Workers must consume locally and survive a brief home-region partition; a regional broker keeps in-region checking off the cross-region path. Mirroring only `check.results` + `region.health` home keeps egress bounded (RFC-002 section 7.5); premium-region egress is a paid entitlement so free traffic never pays it.

---

## 7. Service-to-service trust (runtime)

RFC-000 section 7.2 locked the v1 stance: NetworkPolicy + TLS-to-infra, no service mesh. This RFC provisions it.

### 7.1 Default-deny plus explicit allows

Every namespace gets a default-deny-all NetworkPolicy (ingress and egress), then explicit allows per service. Example shape for the control plane:

```yaml
# default deny everything in pulse-control
kind: NetworkPolicy
metadata: { name: default-deny, namespace: pulse-control }
spec:
  podSelector: {}
  policyTypes: [Ingress, Egress]
```

| Service | Allowed ingress | Allowed egress |
|---------|-----------------|----------------|
| api | from ingress-nginx only (port 8080) | Postgres (primary + replica), Redis, central Kafka, OIDC providers (Google/GitHub) over 443, DNS |
| scheduler | none (no traffic Service) | central Kafka, regional Kafka clusters (produce `check.jobs.<region>`), Postgres, Redis, k8s API (Lease), DNS |
| alerting | none | central Kafka, Postgres, Redis, DNS |
| notifier | none | central Kafka, Postgres, Redis, plus outbound to notification endpoints (Slack/Discord/webhook/SMTP) over 443/587, DNS |
| worker (regional) | none | regional Kafka, Postgres (result write, RFC-000 2.3), Redis (check-now lock), DNS, and constrained outbound to customer endpoints (section 7.3) |

TLS to every infra endpoint is required, not optional: Postgres (`sslmode=verify-full`), Redis (TLS), Kafka (TLS). The cluster network is the trust boundary; a mesh with pod-to-pod mTLS is deferred to the SOC 2 / scale trigger recorded in RFC-000 section 7.2.

### 7.2 Why no mesh in v1

The internal call graph is small and mostly flows through Kafka, not direct service-to-service HTTP (RFC-000 section 7.2). NetworkPolicy + TLS-to-infra covers the realistic threat (a compromised pod reaching a store it should not). A mesh's pod-identity benefit does not justify its operational weight for five services that barely call each other directly. Trigger to revisit: SOC 2 (PRD 13) or a service count / direct-call growth that makes cryptographic pod identity worth it.

### 7.3 Worker egress controls (SSRF defense at the network layer, RFC-005)

SSRF is defended in the worker code (resolution-time validation, the dialer `Control` re-check, per-hop redirect re-validation, RFC-000 section 10 / RFC-005). This RFC adds a second layer at the network so a code bypass still cannot reach internal services:

| Control | How |
|---------|-----|
| Block private and link-local ranges at egress | NetworkPolicy egress (or a CNI egress policy / egress firewall) denies worker pods to RFC1918 (`10/8`, `172.16/12`, `192.168/16`), link-local (`169.254/16`, includes `169.254.169.254` cloud metadata), `127/8`, and the cluster pod/service CIDRs. Worker outbound is allowed only to public internet ranges and DNS. |
| Block cloud metadata explicitly | the `169.254.169.254` block above plus, where the cloud supports it, IMDSv2-only / hop-limit 1 on the node so a pod cannot reach instance metadata even if a rule is missed. |
| No path to control-plane stores from a region | regional worker pods have no network route to control-plane Postgres/Redis other than the specific result-write endpoint; they reach the regional Kafka and the public internet, nothing else. |

This is defense in depth: the worker's own SSRF guard is primary; the network controls mean a bug in that guard still cannot reach `169.254.169.254` or an internal `10.x` host.

---

## 8. Secrets and KMS

RFC-000 section 10 fixed that the 32-byte app key comes from a k8s Secret sourced from a cloud KMS / secret manager, never an env var baked into an image. This RFC fixes the runtime.

### 8.1 external-secrets operator backed by cloud KMS

Decision: the **external-secrets operator** (ESO) syncs secrets from the cloud secret manager (AWS Secrets Manager / GCP Secret Manager, encrypted by the cloud KMS) into k8s Secrets. Pods read them as mounted files or env from the synced Secret. The source of truth is the cloud secret manager (Terraform-provisioned, section 11); k8s Secrets are a synced projection, never the master copy, and are never committed to git.

Why ESO over k8s Secrets alone or Sealed Secrets:

| Option | Verdict |
|--------|---------|
| external-secrets operator + cloud secret manager | Chosen. Secret values live in the KMS-encrypted cloud store; rotation in the store propagates to the cluster automatically; nothing secret is in git or in an image. Fits the GitOps model (section 9): the `ExternalSecret` manifest is in git, the value is not. |
| Plain k8s Secrets in git | Rejected. base64 is not encryption; a secret in git is a leaked secret. |
| Sealed Secrets | Rejected. Encrypts-in-git is better than plain, but the source of truth is then a sealed blob in git rather than a KMS-backed store with rotation and audit. ESO keeps the cloud secret manager authoritative. |

### 8.2 What is sourced this way

| Secret | Source | Rotation stance |
|--------|--------|-----------------|
| `PULSE_SECRET_KEY` (AES-256-GCM, `internal/crypto`) | cloud secret manager, mounted into api/notifier/worker as needed | Rotation is key-versioned: a new key is added as the active encrypt key while the old key stays available for decrypt. `internal/crypto` (LoadKey) is extended to accept a keyset (active + previous) so a rotation re-encrypts lazily on next write without a big-bang re-encrypt. The crypto wire contract (Encrypt/Decrypt) is unchanged (RFC-000 section 10). Manual, infrequent, owned by the security pass; this RFC fixes the sourcing and the keyset shape. |
| JWT signing private key (RS256, RFC-003 / ADR-0005) | cloud secret manager, mounted **only into api** | Rotated by publishing the new public key in JWKS first, signing with the new key after verifiers have picked it up, then retiring the old key after the token TTL window. Private key never leaves api. |
| OIDC client secrets (Google/GitHub) | cloud secret manager, into api | Rotated at the provider, updated in the secret manager, ESO propagates. |
| DB / Kafka / Redis credentials | cloud secret manager (or IAM auth where the managed service supports it) | Prefer short-lived IAM auth (RDS IAM, MSK IAM) over static passwords where available; otherwise managed-secret rotation. |

No secret is ever in a Dockerfile, an image layer, a plain env var in a manifest, or a log line (reused redaction discipline, RFC-000 section 9.2).

---

## 9. CI/CD

### 9.1 Pipeline

```
push / PR
  -> lint + go vet + staticcheck
  -> unit tests + the cross-tenant isolation suite (RLS T1-T6, RFC-001 5.4)  [BLOCKS on failure]
  -> the alerting table test (RFC-000 11.4)
  -> build per-service images (one Dockerfile, SERVICE arg) + SBOM (syft)
  -> scan images (grype/trivy)  [BLOCKS on HIGH/CRITICAL]
  -> cosign sign + push to registry
  -> bump image digests in the GitOps repo (the deploy desired-state)
  -> Argo CD reconciles: migration Job (Helm pre-upgrade hook) -> rollout
```

The cross-tenant isolation suite is a hard control: a migration that creates a tenant table without its RLS policy fails T4-T6 and blocks the release (RFC-001 section 8). This is the release-blocking guarantee RFC-000 section 10 requires.

### 9.2 GitOps: Argo CD (decision)

Decision: **Argo CD** (pull-based GitOps), not pipeline-push (`kubectl apply` / `helm upgrade` from CI), and not Flux.

| Option | Verdict |
|--------|---------|
| Argo CD | Chosen. Pull-based: the cluster reconciles itself toward a git desired-state, so the cluster state is auditable in git and drift is detected and corrected. The Argo UI is a real operational win for seeing rollout and sync status across the control-plane and regional clusters. ApplicationSet templates the per-region Application from one definition, which fits the "add a region = additive" model. |
| Flux | Viable and lighter, but Argo's multi-cluster UI and ApplicationSet for the per-region fan-out fit our control-plane / N-region shape better. Close call; Argo wins on the regional fan-out ergonomics. |
| Pipeline-push from CI | Rejected as the primary mechanism. It gives CI cluster-admin credentials (a bigger blast radius) and no continuous drift correction. CI's job ends at "push image + bump the digest in git"; the cluster pulls from there. |

### 9.3 Migration Job ordering (RFC-001 section 8)

The `cmd/migrate` binary runs as a Kubernetes Job wired as a **Helm pre-upgrade / pre-install hook**, so Argo (via the Helm chart) runs it before the service rollout:

1. The Job connects as the **migration role** (`BYPASSRLS` + DDL rights, distinct from the service role, RFC-001 section 8). It needs BYPASSRLS to touch all rows during a backfill; the running services never use this role.
2. It runs `migrate.Up()` (golang-migrate, forward-only). If `schema_migrations.dirty` is true from a prior failed run, it fails loudly and a human investigates (RFC-001 section 8).
3. On Job success, the rollout of the five services proceeds; they connect as the non-superuser, non-BYPASSRLS **service role**.

The Job is a Helm `pre-upgrade,pre-install` hook with `hook-delete-policy: before-hook-creation` so a re-run starts clean. Because migrations are forward-only and run as one discrete Job, the five services never race to migrate (the fix for the v1 boot-time approach, RFC-000 section 6.3), and the Job start time is the clean PITR pin-point for "restore to just before the bad migration" (RFC-001 section 9.2).

### 9.4 Progressive delivery and rollback

Decision for v1: **rolling updates with a readiness-controlled rollout**, not canary/blue-green automation.

| Option | Verdict |
|--------|---------|
| Rolling update (k8s default, `maxUnavailable: 0`, `maxSurge: 1`) controlled by readiness probes | Chosen for v1. Simple, well-understood, and safe because readiness controls traffic and the migration Job already ran. For five services this is enough. |
| Canary (Argo Rollouts) | Deferred. Real value once api traffic and the cost of a bad rollout justify it; recorded as a "later, when X" so the chart can adopt Argo Rollouts without a redesign. |
| Blue-green | Rejected for v1: double the running footprint for a class of bug a forward-only migration + readiness rollout already controls. |

Rollback strategy:

| Failure | Rollback |
|---------|----------|
| Bad service image | Argo rolls the Deployment back to the previous synced revision (previous image digest in git). Fast, no DB involvement. |
| Bad migration | Forward-only fix: a new migration corrects it (RFC-001 section 8 ships up-only, no down). If data is corrupted, PITR-restore to just before the migration Job start time (RFC-001 section 9.2). We never run an untested down migration against production. |

### 9.5 Docs site + OpenAPI sync (PRD-005, RFC-000 11.4)

The OpenAPI 3 spec in `api/openapi/` is the single source of truth (RFC-012). A CI job on merge to main regenerates the public docs site (Swagger UI + pricing/docs pages) and publishes it to **GitHub Pages**. The job fails if the committed spec drifts from what the api code serves, so the published docs can never lag the API (PRD-005, RFC-000 11.4).

---

## 10. Environments

Three environments. Parity in shape, sized down lower in the stack.

| Env | Infra | Purpose |
|-----|-------|---------|
| dev (local) | docker-compose (section 13): Postgres, Redis, Kafka in KRaft mode, the five services, nginx, Prometheus, Grafana. No managed cloud. | A developer runs the whole platform on a laptop. This is also the shape the implementation targets first. |
| staging | a smaller k8s cluster, managed Postgres/Redis/Kafka at small sizes, one region (home), full observability. Same Helm charts and Terraform modules as prod with smaller variables. | Pre-prod verification; the migration Job and the rollout run here exactly as in prod. |
| prod | dedicated control-plane cluster + per-region data-plane clusters, managed HA Postgres/Redis/Kafka, full DR (section 12). | Live. |

Promotion: the same image digest that passed staging is promoted to prod by bumping the prod GitOps Application to that digest. Terraform modules and Helm charts are shared; only the per-env values file differs (sizes, replica counts, region list). Parity means "same manifests, different scale", so a behavior that works in staging works in prod.

---

## 11. IaC

### 11.1 Terraform for cloud, Helm for in-cluster

| Layer | Tool | Owns |
|-------|------|------|
| Cloud infra | Terraform | k8s clusters (control + per-region), managed Postgres (primary, replicas, backup/PITR config per RFC-001 section 9), managed Redis, managed Kafka (central + regional) or Strimzi where self-run, DNS zones + records (the `cname.pulse.app` target, the wildcard zone), KMS keys + the cloud secret manager entries, IAM roles for IRSA/Workload Identity. |
| In-cluster workloads | Helm | per-service charts under an umbrella chart: Deployments, Services, HPAs/ScaledObjects, PDBs, NetworkPolicies, ServiceAccounts + RBAC (incl. the scheduler Lease Role), the migration Job hook, Ingress + cert-manager `Certificate`/`ClusterIssuer`, ExternalSecret resources. |
| Reconciliation | Argo CD | renders the Helm charts from the GitOps repo against each cluster (section 9.2). |

### 11.2 Repo layout for `deploy/`

```
deploy/
  docker/
    Dockerfile                 # one parameterized Go service image (SERVICE arg)
    nginx/Dockerfile           # SPA + status-page nginx image
    nginx/default.conf         # /api proxy + status-page cache-first location
  helm/
    umbrella/                  # one release deploys the control plane
      Chart.yaml
      values.yaml              # base
      values-staging.yaml
      values-prod.yaml
    charts/
      api/  scheduler/  worker/  alerting/  notifier/  nginx/
      migrate-job/             # the pre-upgrade hook Job
      observability/           # kube-prometheus-stack values + OTel collector (RFC-010)
  terraform/
    modules/
      cluster/  postgres/  redis/  kafka/  dns/  kms/  secrets/  region/
    envs/
      staging/  prod/
  argocd/
    apps/                      # Argo Application + ApplicationSet (per-region fan-out)
  compose/
    docker-compose.yml         # local dev (section 13)
```

The per-region cluster is one Terraform `region` module instance and one Argo `ApplicationSet` entry, so adding a region is a small additive change (RFC-000 section 4.2).

---

## 12. DR and multi-region ops

### 12.1 Control-plane availability stance (PRD master 12, PRD-007)

The 99.9% SLA target is for a **single-region control plane** in v1 (PRD master 12, PRD-007 section 1.4: "not building active-active multi-region control plane"). Within the home region, HA is the managed redundancy: Multi-AZ Postgres, HA Redis, multi-broker Kafka, and multiple zones for the k8s nodes with topology spread (section 3). A whole-home-region loss is a DR event recovered from cross-region backups (section 12.2), not an automatic failover; multi-region control plane is a later phase and is explicitly out of scope here.

### 12.2 Backup / restore drills (RFC-001 section 9)

This RFC owns the schedule and the runbook for the policy RFC-001 fixed:

| Item | Value |
|------|-------|
| Automated full backup | daily, retained 30 days (managed) |
| PITR window | 7 days minimum |
| Cross-region backup copy | backups replicated to a second region so a home-region loss is recoverable |
| Restore drill | **quarterly**: PITR-restore prod to a fresh instance, run the cross-tenant suite + smoke test, record wall-clock as the practical RTO, confirm it meets the DR target (RFC-001 section 9.3). |
| DR target (this RFC sets the number) | RTO 4 hours, RPO 5 minutes (PITR resolution) for a full home-region loss. Within-region single-AZ loss is covered by Multi-AZ failover (minutes, no data loss). |

The 14-day org-deletion grace interacts with backups exactly as RFC-001 section 9.4 states: live delete is immediate and cascading at grace end; backups carry the data until they age out of the 30-day window, satisfied by the documented backup-expiry window for GDPR erasure.

### 12.3 Bringing a regional data plane up / shutting one down

Adding a region (additive, no central migration):

1. Terraform `region` module: new k8s data-plane cluster + regional Kafka (managed, or Strimzi if no managed option).
2. RFC-002 topic provisioning: `check.jobs.<region>` on the regional cluster; the MM2 flow for `check.results` + `region.health` from this region to central.
3. Argo `ApplicationSet` entry deploys the worker Deployment + KEDA ScaledObject into `pulse-region-<code>`.
4. The region catalog row flips to `available` (PRD-007 section 2); the scheduler starts dispatching there for entitled orgs.

Shutting a region down (PRD-007 retire flow): catalog lifecycle to `deprecated` then `retired`; the scheduler stops dispatching; workers drain their `check.jobs` (1h retention means stale jobs simply expire, RFC-002 section 8); MM2 flow and the regional cluster are removed by Terraform. Monitors that still selected it fall back to the home region (PRD-007 section 2), never an empty region set.

### 12.4 MirrorMaker 2 operations (RFC-002 section 7)

| Aspect | Operation |
|--------|-----------|
| What mirrors | only `check.results` + `region.health`, each region -> central, unprefixed topic mapping so central consumers see one logical stream (RFC-002 section 7.3). Jobs are produced straight into the regional cluster and never mirrored. |
| Where MM2 runs | as the managed mirror where the provider offers it; otherwise a Connect/MM2 deployment in the regional cluster, deployed by Helm. |
| Ordering | MM2 preserves per-partition order and maps source partition to the same destination partition, so a monitor's results stay on one partition end-to-end (RFC-002 section 7.4). |
| Health | MM2 lag and the central-consumer lag are SLIs (RFC-010); a stalled mirror surfaces as a region's results not arriving, which probe-fleet health turns into coverage-degraded, never a false page (RFC-002 section 6 / RFC-000 section 1.2). |
| Egress | bounded to result + heartbeat volume; premium-region egress is a paid entitlement (RFC-002 section 7.5). At Phase 0/1 there is one region (home), so there is no mirror and no egress. |

---

## 13. Local dev (docker-compose)

One `deploy/compose/docker-compose.yml` brings the whole platform up on a laptop. Kafka runs in **KRaft mode** (no ZooKeeper). nginx serves the built SPA and proxies `/api`, matching prod's serving shape. The migration runs as a one-shot `migrate` container before the services start (compose `depends_on` + a healthcheck on the `migrate` exit, mirroring the prod pre-rollout Job).

### 13.1 Service list

| Compose service | Image | Role | Depends on |
|-----------------|-------|------|-----------|
| `postgres` | postgres:16 | the single Postgres (no RLS-bypass split needed locally beyond two roles seeded by init SQL) | - |
| `redis` | redis:7 | cache, locks, rate-limit, dedup | - |
| `kafka` | a KRaft-mode Kafka image (Bitnami/Confluent KRaft) | single-broker bus, all topics (no MM2 locally; one region = home) | - |
| `migrate` | local `migrate` build | runs `migrate.Up()` as the migration role, then exits 0 | postgres |
| `api` | local `api` build | api service | postgres, redis, kafka, migrate |
| `scheduler` | local `scheduler` build | scheduler (leader election degenerates to a single replica locally) | postgres, redis, kafka, migrate |
| `worker` | local `worker` build | worker (consumes `check.jobs.home`) | kafka, postgres, redis, migrate |
| `alerting` | local `alerting` build | alerting + the in-process rollup/partition task | postgres, redis, kafka, migrate |
| `notifier` | local `notifier` build | notifier | postgres, redis, kafka, migrate |
| `nginx` | local nginx build | serves the SPA, proxies `/api` to `api`, serves status pages | api |
| `prometheus` | prom/prometheus | scrapes every service's `/metrics` (RFC-010) | - |
| `grafana` | grafana/grafana | dashboards over Prometheus (RFC-010) | prometheus |

This is the full loop on one machine: sign in (OIDC against a stubbed/dev client), create a monitor, the scheduler dispatches, the worker checks, alerting decides, the notifier delivers, the status page serves, and Prometheus/Grafana show the metrics. Topic creation is done by an init step or `KAFKA_AUTO_CREATE_TOPICS` for dev only (prod creates topics explicitly via RFC-002 provisioning).

---

## 14. Open questions and dependencies

### 14.1 Open questions

| # | Question | Owner |
|---|----------|-------|
| 1 | `PULSE_SECRET_KEY` rotation cadence and the keyset re-encrypt policy (lazy-on-write vs a background re-encrypt sweep). This RFC fixes the keyset shape (active + previous) and KMS sourcing; the formal cadence is the security pass. | security pass / RFC-011 |
| 2 | Exact DR RTO/RPO commitment to publish externally vs the internal target set here (RTO 4h / RPO 5m). Needs a product + legal sign-off against the 99.9% SLA wording. | product + RFC-008 |
| 3 | Canary (Argo Rollouts) adoption trigger: at what api traffic / blast-radius does v1's rolling update stop being enough? Record the "later, when X". | RFC-011 / RFC-010 |
| 4 | Per-region managed-Kafka availability map: which operated regions (PRD-007) lack a managed Kafka and therefore need Strimzi. Drives the section 6 fallback per region. | RFC-008 |
| 5 | DLQ replay tooling (RFC-002 open question 6): the small admin command in `internal/bus` and how it runs as a k8s Job. | RFC-011 / RFC-002 |
| 6 | Custom-domain cert issuance rate-limit handling: Let's Encrypt has per-domain and per-account ACME limits; at scale of many custom domains, confirm the issuer config (and whether a second ACME account or a commercial ACME CA is needed). | RFC-011 / security pass |

### 14.2 Dependencies

| This RFC depends on | For |
|---------------------|-----|
| RFC-000 | leader-election (ADR-0004), managed-infra stance (11.3), topology (section 4), service-to-service trust (7.2), security (section 10), the deployment skeleton (section 11). |
| RFC-001 | the migration Job + BYPASSRLS role (section 8), backup/PITR policy (section 9), read replicas (section 10). |
| RFC-002 | regional Kafka clusters, MM2 topic mapping, partition counts, DLQ retention (section 7, 9). |
| RFC-010 | the observability stack this RFC deploys, and the SLIs (lag, MM2 health) the HPA/KEDA and DR signals rely on. |
| PRD master 12, PRD-004, PRD-007 | the 99.9% single-region control-plane stance, status-page resilience + custom-domain TLS, the region topology. |

| Depends on this RFC | For |
|---------------------|-----|
| Every service RFC (004-009) | the Deployment, probes, HPA, NetworkPolicy, secrets, and image it ships in. |
| RFC-008 (multi-region) | the regional cluster provisioning, MM2 operations, and region add/retire mechanics. |
| RFC-012 (api surface) | the OpenAPI -> GitHub Pages sync wiring and the `consistency=strong` read routing the ingress/replica split assumes. |
| RFC-013 (frontend) | the nginx image and the SPA serving + status-page cache-first path. |

---

## 15. ADR candidates

| ADR | Decision | One-line reasoning |
|-----|----------|--------------------|
| ADR-0011-a | KEDA for Kafka-lag autoscaling, plain HPA for api RPS/CPU | KEDA reads consumer-group lag natively; api's signal is a real Prometheus query, so the adapter + HPA fits there. |
| ADR-0011-b | cert-manager + Let's Encrypt, wildcard `*.pulse.app` (DNS-01) + on-demand per-custom-domain (HTTP-01) | automatic issue/renew is the only way to serve one cert per customer domain at scale. |
| ADR-0011-c | Argo CD pull-based GitOps, rolling updates (canary deferred) | cluster reconciles from git desired-state; CI never holds cluster-admin; rolling + readiness is enough for v1. |
| ADR-0011-d | external-secrets operator backed by cloud KMS/secret manager | KMS-encrypted store stays authoritative; nothing secret in git or images; rotation propagates. |
| ADR-0011-e | distroless static non-root images, one parameterized Dockerfile per `cmd/<service>` | tiny attack surface, reproducible builds, no five drifting Dockerfiles. |
| ADR-0011-f | managed Kafka per region, Strimzi only where no managed offering exists | keep out of the Kafka-operations business except where a region forces it. |
