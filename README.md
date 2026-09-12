# OptiScale
## Explainable Kubernetes Capacity Governor

OptiScale is an explainable, SLO-aware, dependency-aware Kubernetes capacity governor that determines whether adding replicas will actually increase useful application capacity, where the bottleneck is, and the safest action under uncertainty. It is not simply an HPA with a latency trigger: it distinguishes target saturation from dependency bottlenecks, accounts for traffic-bearing replicas, and records why it scales or holds. Its optional forecast is deterministic least-squares regression over observed demand and learned healthy throughput, not opaque ML.

**Tech stack:** Go · Kubernetes · controller-runtime / Kubebuilder-style controller · CRDs · Prometheus · PromQL · Grafana · k6 · PostgreSQL · Docker · Minikube · PowerShell

Further reading: [architecture and safety model](docs/architecture.md) · [60-second demo guide](docs/demo-guide.md).

The local topology is `demo-api` → `inventory-service` → PostgreSQL. The controller observes Prometheus, classifies the target and dependencies, applies guardrails, and scales through the Kubernetes Deployment `/scale` subresource.

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

![OptiScale Capacity Governor Grafana dashboard](docs/images/optiscale-grafana-dashboard.png)

*Screenshot from a real Minikube predictive scaling run: demand ramp, p95 latency against the 250ms SLO, effective-serving replicas, concurrency occupancy against its safe boundary, success/error RPS, and inventory/PostgreSQL dependency health. Grafana is demo observability only; it does not feed controller decisions.*

## Runtime proof — latest successful Minikube/k6 run

Measured in the latest successful run on 2026-09-12:

| Evidence | Result |
| --- | --- |
| Load and action | 200 RPS -> 480 RPS; `PRESCALE_TARGET`; demo-api 2 -> 3 replicas |
| p95 at decision | 107.86 ms; 250 ms SLO |
| Demand and capacity | 369.82 RPS predictive demand; 448.21 RPS forecast at the planning horizon; 433.45 RPS realized safe capacity before scale |
| Forecast quality | HIGH; R^2 0.999869; normalized RMSE 0.004; slope +1.540 RPS/s^2 |
| Planning horizon | 51 s = 21 s measured Pod creation-to-Ready + 15 s control-loop allowance + 15 s demand-observation lag |
| Replica realization | Third replica became Ready below the SLO; final requested / Ready / effective-serving replicas: 3 / 3 / 3 |
| k6 result | 104,796 requests; 0 dropped iterations; 0 HTTP failures; overall p95 101.52 ms |

OptiScale observed a healthy demand ramp, learned useful capacity, and projected demand across the measured readiness/control-loop horizon. Forecast demand exceeded realized safe capacity, so it requested one additional replica before the 250 ms SLO was violated. The third replica then became traffic-bearing and realized capacity increased; the controller held at three rather than blindly scaling again. This is what the measured run demonstrated, not a claim that the forecast prevented an outage or guarantees the same result for arbitrary production traffic.

> **Requested replicas != useful capacity.** Kubernetes Ready is necessary, but does not prove that a replica is receiving meaningful traffic. OptiScale separately measures effective-serving replicas and blocks repeated target scale-ups while requested/Ready capacity has not become useful serving capacity. The [capacity-realization scenario](deploy/scenarios/predictable-ramp.ps1) exercises that guard with sticky/long-lived connections. Connection reuse or traffic distribution can plausibly delay useful realization; the demo does not establish a specific kube-proxy or network cause.

**Dependency-aware scaling:** the [database bottleneck scenario](deploy/scenarios/database-bottleneck.ps1) uses real PostgreSQL queries with deterministic `pg_sleep` delay and configures PostgreSQL as non-scalable. When the database is the measured bottleneck, OptiScale chooses `CAPACITY_BLOCKED` / `HOLD` and leaves target replicas unchanged rather than scaling an upstream component whose additional replicas would not relieve that dependency.

The analyzer classifies evidence as `HEALTHY`, `TARGET_SATURATED`, `DEPENDENCY_SATURATED`, `CAPACITY_BLOCKED`, or `UNCERTAIN`. Reactive and dependency decisions take precedence over prediction; incomplete forecast inputs only suppress prescaling. Database metrics are application-observed latency, errors, and in-flight query work, not database CPU or internal wait-state telemetry. Forecast and empirical capacity histories are controller-local and reset on restart. Grafana OSS is a local visualization layer only. No Random Forest, durable learning, OpenTelemetry, Karpenter, replay, or sophisticated optimization is implemented.

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

Expected decision: `CAPACITY_BLOCKED` / `HOLD`, with `postgres` as the bottleneck. The deterministic SQL `pg_sleep` workload makes PostgreSQL query latency rise while PostgreSQL is configured non-scalable. OptiScale treats inventory latency as downstream evidence rather than proof that inventory scaling helps, rejects upstream `SCALE_TARGET` and `SCALE_DEPENDENCY`, and leaves both demo-api and inventory-service replica counts unchanged.

### Predictive scenarios

The [predictive success profile](deploy/scenarios/predictable-ramp-success.ps1) is the canonical end-to-end demo:

```powershell
.\deploy\scenarios\predictable-ramp-success.ps1
```

It runs the pinned `grafana/k6:2.2.0` image with an open `ramping-arrival-rate` executor, 400 preallocated/max VUs, 200 RPS baseline/warmup, a linear 200 -> 480 RPS ramp, and a 480 RPS plateau. It normally reuses connections and sends `Connection: close` on approximately every 25th iteration per VU to provide moderate, bounded connection turnover. This is a controlled demo traffic model, not a production-general traffic assumption. The script verifies a new `PRESCALE_TARGET`, real `/scale` 2 -> 3, Ready below SLO, and a new `PreScaleTarget` Event. See the runtime proof above for the measured result.

The original profile is deliberately kept as a separate capacity-realization guardrail test, not a failed or obsolete success scenario:

```powershell
.\deploy\scenarios\predictable-ramp.ps1
```

Long-lived connections can leave a new endpoint Ready but not yet meaningfully traffic-bearing; OptiScale holds further target scaling while Ready exceeds effective-serving replicas. The profile may cross the SLO and demonstrates the guardrail, not SLO preservation. Connection reuse or traffic distribution is plausible context, not a proven root cause.

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

These automated checks were run locally; this is not a claim that CI ran them:

```powershell
go test ./...
go build ./...
go vet ./...
git diff --check
```

**Local runtime validation (2026-09-12):** the predictive Minikube/k6 proof passed; a real Kubernetes `/scale` mutation changed `demo-api` from 2 to 3. Grafana's Prometheus datasource/query path was verified, and Grafana remained healthy with 0 restarts through the full predictive run at a 768Mi memory limit. The final workload had 3 requested / 3 Ready / 3 effective-serving replicas. The latest k6 run completed 104,796 requests with 0 dropped iterations, 0 HTTP failures, and 101.52ms overall p95. These are results from that local run, not CI or a guarantee for other workloads.
