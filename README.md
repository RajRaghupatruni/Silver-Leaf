# OptiScale — Predictive Capacity Vertical Slice

OptiScale is an explainable, SLO-aware Kubernetes Capacity Governor. The proven local topology is `demo-api` → `inventory-service` → PostgreSQL. Alongside reactive target/dependency scaling and `CAPACITY_BLOCKED`, the controller now has an optional bounded trend forecast that can choose `PRESCALE_TARGET` before the target SLO is violated. Forecasting uses transparent least-squares regression and measured healthy throughput; it is not opaque ML or durable production history.

```text
load generator → demo-api → inventory-service → PostgreSQL
                     └──────── Prometheus pod discovery ───────┘
                                      ↓
                  typed observations → analyzer → guarded policy
             healthy rising demand → forecast → PRESCALE_TARGET
                         CAPACITY_BLOCKED → HOLD (no scale write)
                                      ↓
                    Deployment /scale + DecisionRecord
```

The analyzer classifies evidence as `HEALTHY`, `TARGET_SATURATED`, `DEPENDENCY_SATURATED`, `CAPACITY_BLOCKED`, or `UNCERTAIN`. Reactive and dependency decisions take precedence over prediction; incomplete forecast inputs only suppress prescaling. The database scenario uses real inventory queries and application-observed database latency, errors, and in-flight query metrics; the query includes a configurable PostgreSQL `pg_sleep` delay for deterministic local testing. These measurements do not claim database CPU or internal wait-state telemetry. Histories and empirical capacity evidence are controller-local and reset on restart. Grafana OSS is provided as a local, read-only visualization layer; it does not feed controller decisions. No Random Forest, persistent learning, OpenTelemetry, Karpenter, replay, or sophisticated optimization is implemented.

## Run locally with Minikube

Prerequisites: Go 1.23+, Docker, kubectl, Minikube, and Make (or run the commands in the Makefile directly in PowerShell).

```powershell
minikube start --cpus=4 --memory=4096
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

### Predictive runtime proofs

Two complementary Kubernetes-local k6 scenarios exercise predictive scaling. The success profile is the canonical end-to-end predictive demo; the original profile remains available as a capacity-realization guardrail case.

#### Predictive success

Run:

```powershell
.\deploy\scenarios\predictable-ramp-success.ps1
```

The pinned `grafana/k6:2.2.0` Job uses an open `ramping-arrival-rate` executor (one GET per scheduled iteration, 400 preallocated/max VUs): 200 RPS for 110 seconds (65 seconds of baseline validation with OptiScale stopped, then 45 seconds of controller warmup), a linear 200 -> 480 RPS ramp over 180 seconds, then a 480 RPS plateau for 45 seconds. It normally reuses HTTP connections, while approximately every 25th iteration per VU sends `Connection: close` to provide moderate, bounded connection turnover and opportunities for a newly added endpoint to receive fresh connections. This is a controlled demo traffic model, not a claim that production traffic generally has the same connection behavior. The Job fails on dropped iterations or an HTTP failure rate of 1% or more; the script requires a new high-quality `PRESCALE_TARGET`, a real Deployment `/scale` change from 2 -> 3, the third replica Ready below the 250ms SLO, and a new `PreScaleTarget` Event.

Runtime evidence collected on 2026-09-12:

- Baseline: about 200.03 RPS, p95 73.86ms, and 2 Ready / 2 effective-serving replicas; the safe occupancy boundary was 15 slots per replica.
- The `PRESCALE_TARGET` decision was made while the target remained healthy: current/desired replicas 2/3, decision p95 93.08ms and live p95 94.77ms against the 250ms SLO. Predictive demand was 417.62 RPS; forecast demand was 496.65 RPS against 482.33 RPS realized current safe capacity (241.17 RPS per serving replica). The forecast slope was +1.5528 RPS/s^2, R^2 was 0.9999956, and confidence was HIGH. The 51-second planning horizon was 21 seconds measured readiness + 15 seconds control-loop allowance + 15 seconds demand-observation lag. Ready/effective-serving replicas at the decision were 2/2.
- The real scale request took demo-api from 2 to 3. The third replica became Ready at about 97.28ms p95, and effective-serving count also reached 3. Later samples were about 454 RPS / 96.87ms p95, 468 RPS / 96.20ms, and 476 RPS / 93.56ms, with 3 Ready / 3 effective-serving replicas. Realized safe capacity rose to about 712-724 RPS; the controller then correctly held at 3 rather than scaling further.
- k6 completed 104,795 requests with 0 dropped iterations, 0 HTTP failures, and overall p95 about 86.63ms.

This run is the end-to-end proof that forecast-driven prescaling requested bounded capacity early enough, the new replica became traffic-bearing, and the SLO remained healthy in this controlled scenario. It does not establish that arbitrary production traffic will distribute connections the same way.

#### Capacity-realization guard

Run the original scenario:

```powershell
.\deploy\scenarios\predictable-ramp.ps1
```

Keep this as a distinct guardrail/capacity-realization test, not as an obsolete or failed copy of the success proof. With normal long-lived connection reuse, Kubernetes may report 3 Ready replicas after a 2 -> 3 scale while recent request telemetry still shows only 2 effective-serving replicas. Connection reuse or traffic distribution can plausibly produce that observation; this scenario does not identify a definitive cause. OptiScale does not credit every Ready pod as useful capacity and holds further target scaling/prescaling while Ready exceeds effective-serving replicas, avoiding blind 3 -> 4 -> 5 scale-ups. This profile may cross the SLO; its purpose is to exercise the capacity-realization guard, not to prove SLO preservation.

Restore baseline settings by reapplying the deployment manifests and restarting load generation:

```powershell
kubectl apply -f deploy/inventory/deployment.yaml
kubectl apply -f deploy/demo-app/deployment.yaml
kubectl apply -f deploy/loadgen/deployment.yaml
```

Reapplying `deploy/inventory/deployment.yaml` restores `INVENTORY_DB_DELAY_MS=0` and baseline inventory settings.

### Grafana demo dashboard

`make install` provisions Grafana OSS and the `OptiScale Capacity Governor` dashboard from checked-in configuration. To open it, run this helper in a PowerShell window and visit `http://localhost:3000`:

```powershell
.\deploy\grafana\open-dashboard.ps1
```

The helper waits for the Grafana Deployment and Service endpoints, then binds `kubectl port-forward` to loopback only. The local lab allows anonymous Viewer access; the Service is ClusterIP-only, and this is not an Internet-facing authentication model. Grafana's data directory is ephemeral; its Prometheus datasource and dashboard are reprovisioned from files after a pod restart.

Dashboard panels:

1. **Demand RPS** — observed demo-api request rate from `http_requests_total`.
2. **p95 latency vs SLO** — demo-api p95 with a visible 250ms reference line.
3. **Effective-serving replicas** — the same `>1 RPS over 30s` traffic-bearing definition used by OptiScale. This repository does not scrape kube-state-metrics, so requested and Kubernetes Ready replica series are unavailable in Prometheus; use `kubectl get deployment demo-api` for those values.
4. **Concurrency slot occupancy** — aggregate and hottest traffic-bearing held-slot rates with the 15-slot safe operating boundary.
5. **Success/error RPS** — a truthful substitute because forecast demand and realized safe capacity are currently exposed in OptiScaler status/DecisionRecord, not as Prometheus series. The dashboard does not fabricate or reconstruct those values.
6. **Dependency health** — inventory HTTP p95 and application-observed PostgreSQL query p95.

Grafana is for portfolio/demo observability only. Its panels do not supply inputs to the controller or change policy decisions; inspect the OptiScaler resource and controller logs for the actual forecast and DecisionRecord.

## Metrics and policy

Both HTTP services export monotonic `http_requests_total` and `http_errors_total` counters alongside `http_active_requests`, `http_active_work`, and `http_request_duration_seconds`. `demo-api` also exports cumulative `http_local_work_seconds_total` for diagnostic visibility and the production capacity signal `http_concurrency_slot_seconds_total`, which adds the full wall-clock time each request holds a `DEMO_CONCURRENCY` semaphore slot, including local work and downstream HTTP wait. Time queued before acquiring a slot is excluded and counted separately in `http_concurrency_queue_seconds_total`; instantaneous `http_concurrency_slots_in_use` and `http_concurrency_queue_waiters` gauges aid debugging. `http_active_work` and `http_local_work_seconds_total` remain useful diagnostics but no longer define target capacity. `http_errors_total` counts requests the app actually returns as errors; successful RPS is derived as total RPS minus error RPS. Inventory additionally exports `db_requests_total`, `db_errors_total`, `db_active_requests`, and the `db_request_duration_seconds` histogram around actual PostgreSQL queries. These are application-observed query metrics, not database-internal utilization. The sample uses these actual metric names:

- Target p95: `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="optiscale-demo",app="demo-api"}[1m]))) * 1000`; the demo-api histogram has 25ms bucket boundaries from 200ms through 300ms around the 250ms SLO.
- Experimental direct 250ms SLO compliance ratio: `sum(rate(http_request_duration_seconds_bucket{namespace="optiscale-demo",app="demo-api",le="0.25"}[1m])) / sum(rate(http_request_duration_seconds_count{namespace="optiscale-demo",app="demo-api"}[1m]))`. A result >= `0.95` means at least 95% of observed requests completed within 250ms (equivalent to p95 <= 250ms); it is for runtime evidence only and does not replace the configured p95 observation used by OptiScale. With no requests the ratio is undefined.
- Aggregate held-slot occupancy: `sum(rate(http_concurrency_slot_seconds_total{namespace="optiscale-demo",app="demo-api"}[1m]))`.
- Hottest traffic-bearing pod occupancy: `max((sum by(instance) (rate(http_concurrency_slot_seconds_total{namespace="optiscale-demo",app="demo-api"}[1m]))) and on(instance) (sum by(instance) (rate(http_requests_total{namespace="optiscale-demo",app="demo-api"}[30s])) > 1))`; the traffic filter matches the configured effective-serving definition, so an idle or below-cutoff pod is excluded from hottest-serving saturation evidence.
- Effective serving replicas: `count(sum by(instance) (rate(http_requests_total{namespace="optiscale-demo",app="demo-api"}[30s])) > 1)`. Here “effective” means an instance exceeded 1 request/s over the configured 30s rate window; the controller validates that this integer count is no greater than Kubernetes Ready replicas.
- Target physical concurrency limit: `physicalConcurrencyLimit: 20`, matching `DEMO_CONCURRENCY=20`. The existing `safeCapacityMargin: 0.75` derives a safe operating occupancy of 15 slots per serving replica; it is applied once, not again to a pre-reduced limit. The 30% capacity-learning floor is 4.5 mean held slots per effective serving replica.
- Empirical capacity learning uses `successful RPS / aggregate held-slot occupancy` as throughput efficiency, extrapolates that efficiency to the physical 20-slot per-pod ceiling, then applies the 0.75 margin. Realized current safe capacity is `safe RPS per serving replica × effective serving replicas`, never `× Ready replicas`.
- `http_local_work_seconds_total` and `http_active_work` remain diagnostic signals; they omit dependency wait from the target's constrained semaphore-slot measurement.
- Target instant active-work gauge (debugging only): `http_active_work{namespace="optiscale-demo",app="demo-api"}`
- Inventory p95: `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="optiscale-demo",app="inventory-service"}[1m]))) * 1000`
- Inventory active work: `sum(avg_over_time(http_active_work{namespace="optiscale-demo",app="inventory-service"}[1m]))` (threshold `100`; latency is the dependency scenario signal)
- PostgreSQL-query p95: `histogram_quantile(0.95, sum by (le) (rate(db_request_duration_seconds_bucket{namespace="optiscale-demo",app="inventory-service"}[1m]))) * 1000` (threshold `250ms`)
- In-flight PostgreSQL queries: `sum(avg_over_time(db_active_requests{namespace="optiscale-demo",app="inventory-service"}[1m]))` (threshold `8`)
- Target request RPS: `sum(rate(http_requests_total{namespace="optiscale-demo",app="demo-api"}[1m]))`
- Target predictive-demand RPS (forecast trend only): `sum(rate(http_requests_total{namespace="optiscale-demo",app="demo-api"}[30s]))`
- Target error RPS: `sum(rate(http_errors_total{namespace="optiscale-demo",app="demo-api"}[1m]))`
- Inventory request RPS: `sum(rate(http_requests_total{namespace="optiscale-demo",app="inventory-service"}[1m]))`
- Inventory error RPS: `sum(rate(http_errors_total{namespace="optiscale-demo",app="inventory-service"}[1m]))`
- PostgreSQL-query RPS: `sum(rate(db_requests_total{namespace="optiscale-demo",app="inventory-service"}[1m]))`
- PostgreSQL-query error RPS: `sum(rate(db_errors_total{namespace="optiscale-demo",app="inventory-service"}[1m]))`

For each component, `errorRate = error RPS / total RPS` and `successful RPS = total RPS - error RPS`. Error rate is a ratio from `0` to `1`. A zero request-rate denominator leaves error rate absent/undefined, not `0`.

When target p95 exceeds its SLO and the hottest traffic-bearing replica's held-slot occupancy reaches the derived safe operating boundary, the policy may scale the target. A Ready pod is not assumed to contribute realized capacity until recent request telemetry shows meaningful traffic reaching it. If Ready replicas exceed effective serving replicas, target scaling/prescaling is held until the requested capacity is observed serving; this avoids repeated blind scale-ups. Service-level connection reuse can plausibly leave a newly Ready endpoint with little traffic, but the observed idle endpoint alone does not prove that cause. If the target is not locally saturated, dependency decisions remain unchanged: a saturated scalable dependency may be scaled; the configured non-scalable PostgreSQL bottleneck produces `CAPACITY_BLOCKED` and `HOLD`. Decisions use configured bounds, step limits, and cooldown. Missing required SLO, slot-occupancy, serving-count, or dependency telemetry remains protected; incomplete optional forecast telemetry suppresses prescaling and holds. No replica write is made for a no-op.

When prediction is enabled, it is considered only for a currently `HEALTHY` target with healthy dependencies and a HOLD from the existing policy. It cannot override target saturation, dependency saturation, `CAPACITY_BLOCKED`, protected mode, or cooldown. Stable target request/error/success rates remain based on the configured 1-minute queries and continue to drive DecisionRecord telemetry and healthy-capacity learning. A separate `metric.predictiveRequestRateQuery` supplies only the target trend signal; the sample config uses `sum(rate(http_requests_total{namespace="optiscale-demo",app="demo-api"}[30s]))`. Configure `prediction.predictiveDemandWindowSeconds: 30` to match that PromQL range. The controller derives predictive-demand observation lag as half the configured window (15 seconds); it does not parse PromQL or silently fall back to the stable rate when this signal is missing, stale, or non-finite. Such missing fast telemetry blocks prescaling but does not invalidate otherwise eligible capacity samples.

The controller retains at most 20 predictive-demand samples from the last five minutes, but fits ordinary least squares only over samples in the fixed 120-second window ending at the latest sample. Retained history is not the same as the active trend-fit window. At least five fit samples over 60 seconds, no sample gap over 45 seconds, a fresh latest sample, a positive slope, a planning horizon no longer than the fit span, R^2 >= 0.95, and normalized RMSE <= 0.10 are required; only `HIGH` quality can prescale. The recent window emphasizes a sustained current demand regime while retaining the strict fit-quality gates. The planning horizon is the sum of measured Pod creation-to-Ready time, the 15-second reconcile/control-loop allowance, and predictive-demand observation lag (half the configured rate window). With the sample's 30-second window and an example 22-second measured readiness, the horizon is 22 + 15 + 15 = 52 seconds. End-to-end `/scale`-to-Pod-creation latency is not yet learned.

Safe per-serving-replica capacity is learned only from healthy samples: target p95 within SLO, dependencies healthy, fresh complete request/success/error and slot-occupancy observations, acceptable error ratio, all requested replicas Ready and traffic-bearing, mean slot occupancy per serving replica at least 30% of the safe boundary but below it, and hottest-serving occupancy below that boundary. This prevents a second proactive scale while prior requested capacity is still converging or not receiving traffic, even when cooldown is zero. For each sample the controller computes successful RPS divided by aggregate held-slot occupancy, takes the conservative nearest-rank 20th percentile of at least three efficiencies, extrapolates to the physical 20-slot semaphore limit, and applies the `0.75` margin once. Realized safe capacity multiplies the resulting safe per-serving-replica estimate by effective serving replicas, not Kubernetes Ready replicas. This simple linear extrapolation is empirical, not a queueing-model inference. Prescaling requires forecast demand to exceed realized current safe capacity and uses the configured `maxScaleUpStep`, max replicas, and cooldown. The sample's healthy-capacity error limit remains `0.02`.

Readiness lead time uses the maximum valid `Pod creationTimestamp` → `PodReady=True.lastTransitionTime` sample from currently Ready pods owned by the current target ReplicaSet, only when every regular container has `restartCount=0`. All Ready, non-terminating selected pods still count toward Ready replicas, but restarted pods never provide startup-latency samples. The current revision is resolved from the Deployment's observed generation and its matching Deployment-owned ReplicaSet `pod-template-hash`; an unresolved rollout blocks prescaling. Fresh measurements are persisted in OptiScaler status as `learnedReadinessLeadTimeSeconds`, `learnedReadinessObservedAt`, and `learnedReadinessTemplateIdentity`. With no fresh sample, a stored measurement is reused only when its template hash matches the current revision; otherwise readiness is unavailable and prescaling is held. This prevents a host/node reboot from turning an old pod creation timestamp into a huge startup duration, while retaining same-revision startup evidence across controller restarts. Request-rate and capacity histories remain bounded per OptiScaler in controller memory and are lost when the controller restarts; no persistent forecast history is claimed.

The signal roles remain distinct: p95/SLO describes user impact; hottest held-slot occupancy describes local target constraint; request and successful-request rates describe served demand; and error ratio describes reliability/telemetry health. Request/error rates are recorded in `status.lastDecision` as capacity-model inputs only and do not change the existing scale policy. When the request-rate denominator is zero, request and successful RPS can be zero while the error ratio remains absent/undefined; missing error telemetry is never represented as zero errors.

`status.lastDecision` contains the latest analysis, evidence, qualitative confidence, action, chosen workload, and replica values. Predictive decisions separately report the fast `predictiveRequestRate`, configured `predictiveDemandWindowSeconds`, derived `demandObservationLagSeconds`, raw `readinessLeadTimeSeconds`, `readinessEvidenceSource` (`CURRENT_FRESH_SAMPLE` or `PERSISTED_LEARNED_SAMPLE`), target template identity, `controlLoopAllowanceSeconds`, and composed `forecastHorizonSeconds`. The matching learned startup measurement is persisted directly on `status`, not inferred from `lastDecision`. `status.currentReplicas` and `desiredReplicas` continue to describe the primary `demo-api` target. `status.lastScaleDecision` retains the most recent mutating decision across later HOLD or protected-mode evaluations.

## Layout

- `api/v1alpha1`: typed OptiScaler spec, dependency fields, status, and DecisionRecord.
- `cmd/optiscaler`: controller manager.
- `cmd/demo-app`, `cmd/inventory-service`: instrumented demo services.
- `internal/observation`, `internal/capacity`, `internal/forecast`, `internal/policy`: typed observations, deterministic analyzer, explainable trend/capacity model, and pure guardrail policy.
- `internal/controller`: reconciliation and Deployment scale-subresource calls.
- `deploy/`: canonical namespace, demo workloads, load generator, and scenario profiles.
- `config/`: CRD, RBAC, Prometheus, controller, and sample resource.
- `deploy/postgres/`: local PostgreSQL, schema seed, and local-only Secret helper.
- `deploy/scenarios/`: deterministic target, dependency, database bottleneck, and traffic-ramp profiles.
- `docs/architecture.md`: implemented reactive/predictive behavior and safety boundaries.

## Checks

```powershell
go fmt ./...
go test ./...
go build ./...
git diff --check
```

Passing local Go checks do not imply that either Minikube scenario has been run. Runtime results should be reported only after observing the actual workloads, Prometheus samples, `/scale` changes, and OptiScaler status in a cluster.
