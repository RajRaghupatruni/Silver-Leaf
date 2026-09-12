# OptiScale — Vertical Slice 3

OptiScale is an explainable, SLO-aware Kubernetes Capacity Governor. Vertical Slice 3 demonstrates a bounded local topology: `demo-api` calls `inventory-service`, which executes real queries against a local PostgreSQL Deployment. Prometheus observes application request and database-query telemetry. A namespaced OptiScaler can scale `demo-api` or a configured scalable dependency through Kubernetes `/scale`; it identifies a saturated non-scalable PostgreSQL dependency as `CAPACITY_BLOCKED` and holds rather than adding ineffective upstream replicas.

```text
load generator → demo-api → inventory-service → PostgreSQL
                     └──────── Prometheus pod discovery ───────┘
                                      ↓
                  typed observations → analyzer → guarded policy
                         CAPACITY_BLOCKED → HOLD (no scale write)
                                      ↓
                    Deployment /scale + DecisionRecord
```

The analyzer classifies evidence as `HEALTHY`, `TARGET_SATURATED`, `DEPENDENCY_SATURATED`, `CAPACITY_BLOCKED`, or `UNCERTAIN`. The database scenario uses real inventory queries and application-observed database latency, errors, and in-flight query metrics; the query includes a configurable PostgreSQL `pg_sleep` delay for deterministic local testing. These measurements do not claim database CPU or internal wait-state telemetry. Missing, stale, malformed, or conflicting evidence is protected and cannot cause scale-down. Forecasting, machine learning, OpenTelemetry, Grafana, Karpenter, capacity learning, replay, and sophisticated optimization are not implemented.

## Run locally with Minikube

Prerequisites: Go 1.23+, Docker, kubectl, Minikube, and Make (or run the commands in the Makefile directly in PowerShell).

```powershell
minikube start
kubectl apply -f deploy/namespace.yaml
.\deploy\postgres\create-secret.ps1
make docker-build
make minikube-load
make install
make loadgen
make status
```

The install applies the namespace, CRD, namespaced RBAC, Prometheus, PostgreSQL, both HTTP services, controller, and sample OptiScaler in dependency order. To inspect the explainable decision and replica counts:

```powershell
kubectl -n optiscale-demo get optiscaler demo-api -w
kubectl -n optiscale-demo get deployment demo-api inventory-service -w
kubectl -n optiscale-system logs deployment/optiscaler-controller -f
```

The PostgreSQL pod uses the pinned `postgres:16.4-alpine` image, non-root UID/GID 70, bounded resources, and ephemeral `emptyDir` storage for this local lab. The secret is not checked in. The included script uses a clearly warned local-only password if none is supplied; never reuse it outside an isolated development cluster.

### Deterministic evidence profiles

Run one scenario at a time, then allow at least 60 seconds for the histogram and averaged-utilization queries to settle:

```powershell
.\deploy\scenarios\target-saturation.ps1
```

Expected decision: `TARGET_SATURATED` and `SCALE_TARGET` for `demo-api`; inventory remains on its healthy profile.

```powershell
.\deploy\scenarios\dependency-saturation.ps1
```

Expected decision: `DEPENDENCY_SATURATED` and `SCALE_DEPENDENCY` for `inventory-service`; the demo API's short local work keeps target saturation evidence low. The decision describes configured metric evidence and does not claim causal certainty.

```powershell
.\deploy\scenarios\database-bottleneck.ps1
```

Expected decision: `CAPACITY_BLOCKED` / `HOLD`, with `postgres` as the bottleneck. The API and inventory local-work signals stay below their thresholds while real inventory SQL calls wait inside PostgreSQL. Inventory request latency is recorded as downstream evidence, not treated as proof that adding inventory replicas helps. The decision should list rejected `SCALE_TARGET` and `SCALE_DEPENDENCY` actions. Confirm that both `demo-api` and `inventory-service` replica counts remain unchanged.

Restore baseline settings by reapplying the deployment manifests and restarting load generation:

```powershell
kubectl apply -f deploy/inventory/deployment.yaml
kubectl apply -f deploy/demo-app/deployment.yaml
kubectl apply -f deploy/loadgen/deployment.yaml
```

Reapplying `deploy/inventory/deployment.yaml` restores `INVENTORY_DB_DELAY_MS=0` and baseline inventory settings.

## Metrics and policy

Both HTTP services export monotonic `http_requests_total` and `http_errors_total` counters alongside `http_active_requests`, `http_active_work`, and `http_request_duration_seconds`. `http_errors_total` counts requests the app actually returns as errors; successful RPS is derived as total RPS minus error RPS. Inventory additionally exports `db_requests_total`, `db_errors_total`, `db_active_requests`, and the `db_request_duration_seconds` histogram around actual PostgreSQL queries. These are application-observed query metrics, not database-internal utilization. The API utilization signal specifically measures local API work, not time blocked on the dependency. The sample uses these actual metric names:

- Target p95: `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="optiscale-demo",app="demo-api"}[1m]))) * 1000`
- Target active work: `sum(avg_over_time(http_active_work{namespace="optiscale-demo",app="demo-api"}[1m]))` (threshold `30`)
- Inventory p95: `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="optiscale-demo",app="inventory-service"}[1m]))) * 1000`
- Inventory active work: `sum(avg_over_time(http_active_work{namespace="optiscale-demo",app="inventory-service"}[1m]))` (threshold `100`; latency is the dependency scenario signal)
- PostgreSQL-query p95: `histogram_quantile(0.95, sum by (le) (rate(db_request_duration_seconds_bucket{namespace="optiscale-demo",app="inventory-service"}[1m]))) * 1000` (threshold `250ms`)
- In-flight PostgreSQL queries: `sum(avg_over_time(db_active_requests{namespace="optiscale-demo",app="inventory-service"}[1m]))` (threshold `8`)
- Target request RPS: `sum(rate(http_requests_total{namespace="optiscale-demo",app="demo-api"}[1m]))`
- Target error RPS: `sum(rate(http_errors_total{namespace="optiscale-demo",app="demo-api"}[1m]))`
- Inventory request RPS: `sum(rate(http_requests_total{namespace="optiscale-demo",app="inventory-service"}[1m]))`
- Inventory error RPS: `sum(rate(http_errors_total{namespace="optiscale-demo",app="inventory-service"}[1m]))`
- PostgreSQL-query RPS: `sum(rate(db_requests_total{namespace="optiscale-demo",app="inventory-service"}[1m]))`
- PostgreSQL-query error RPS: `sum(rate(db_errors_total{namespace="optiscale-demo",app="inventory-service"}[1m]))`

For each component, `errorRate = error RPS / total RPS` and `successful RPS = total RPS - error RPS`. Error rate is a ratio from `0` to `1`. A zero request-rate denominator leaves error rate absent/undefined, not `0`.

When target p95 exceeds its SLO and target active-work evidence exceeds its threshold, the policy may scale the target. If the target is not locally saturated, a saturated scalable dependency may be scaled. The sample topology declares `inventory-service` as depending on `postgres`: when PostgreSQL is saturated, inventory latency alone is treated as downstream evidence. A non-scalable saturated database produces `CAPACITY_BLOCKED` and `HOLD`, with concrete threshold evidence and rejected upstream scale actions; independent upstream utilization saturation keeps attribution conservative. Decisions use configured bounds, step limits, and cooldown. Missing or invalid telemetry enters protected mode. No replica write is made for a no-op.

The signal roles remain distinct: p95/SLO describes user impact; active work indicates local constraint; request and successful-request rates describe served demand; and error ratio describes reliability/telemetry health. Request/error rates are recorded in `status.lastDecision` as capacity-model inputs only and do not change the existing scale policy. When the request-rate denominator is zero, request and successful RPS can be zero while the error ratio remains absent/undefined; missing error telemetry is never represented as zero errors.

`status.lastDecision` contains the latest analysis, evidence, qualitative confidence, action, chosen workload, and replica values. `status.currentReplicas` and `desiredReplicas` continue to describe the primary `demo-api` target. `status.lastScaleDecision` retains the most recent mutating decision across later HOLD or protected-mode evaluations.

## Layout

- `api/v1alpha1`: typed OptiScaler spec, dependency fields, status, and DecisionRecord.
- `cmd/optiscaler`: controller manager.
- `cmd/demo-app`, `cmd/inventory-service`: instrumented demo services.
- `internal/observation`, `internal/capacity`, `internal/policy`: typed observations, deterministic analyzer, and pure guardrail policy.
- `internal/controller`: reconciliation and Deployment scale-subresource calls.
- `deploy/`: canonical namespace, demo workloads, load generator, and scenario profiles.
- `config/`: CRD, RBAC, Prometheus, controller, and sample resource.
- `deploy/postgres/`: local PostgreSQL, schema seed, and local-only Secret helper.
- `deploy/scenarios/`: deterministic target, dependency, and database bottleneck profiles.
- `docs/architecture.md`: implemented V3 behavior and safety boundaries.

## Checks

```powershell
go fmt ./...
go test ./...
go build ./...
git diff --check
```

Passing local Go checks do not imply that either Minikube scenario has been run. Runtime results should be reported only after observing the actual workloads, Prometheus samples, `/scale` changes, and OptiScaler status in a cluster.
