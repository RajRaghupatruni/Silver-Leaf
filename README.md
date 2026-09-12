# OptiScale — Vertical Slice 2

OptiScale is an explainable, SLO-aware, dependency-aware Kubernetes Capacity Governor. This repository currently implements a bounded two-Deployment demonstration: `demo-api` calls `inventory-service`, both export Prometheus telemetry, and a namespaced OptiScaler can scale either Deployment through the Kubernetes `/scale` subresource.

```text
load generator → demo-api → inventory-service
                     ↓              ↓
                  Prometheus Kubernetes pod discovery
                     ↓
             typed observations → capacity analyzer → guarded policy
                     ↓                               ↓
                DecisionRecord ← Kubernetes Deployment /scale
```

The analyzer classifies evidence as `HEALTHY`, `TARGET_SATURATED`, `DEPENDENCY_SATURATED`, or `UNCERTAIN`. These are deterministic rules over the configured latency and active-request metrics, not causal inference. Missing, stale, malformed, or conflicting evidence is protected and cannot cause scale-down. Forecasting, machine learning, dependency graphs beyond the configured P0 dependency list, PostgreSQL bottleneck logic, OpenTelemetry, Grafana, Karpenter, capacity learning, replay, and sophisticated optimization are not implemented.

## Run locally with Minikube

Prerequisites: Go 1.23+, Docker, kubectl, Minikube, and Make (or run the commands in the Makefile directly in PowerShell).

```powershell
minikube start
make docker-build
make minikube-load
make install
make loadgen
make status
```

The install applies the namespace, CRD, namespaced RBAC, Prometheus, both demo services, controller, and sample OptiScaler in dependency order. To inspect the explainable decision and replica counts:

```powershell
kubectl -n optiscale-demo get optiscaler demo-api -w
kubectl -n optiscale-demo get deployment demo-api inventory-service -w
kubectl -n optiscale-system logs deployment/optiscaler-controller -f
```

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

Restore baseline settings by reapplying the deployment manifests and restarting load generation:

```powershell
kubectl apply -f deploy/inventory/deployment.yaml
kubectl apply -f deploy/demo-app/deployment.yaml
kubectl apply -f deploy/loadgen/deployment.yaml
```

## Metrics and policy

Both services export `http_requests_total`, `http_errors_total`, `http_active_requests`, `http_active_work`, and `http_request_duration_seconds` on `/metrics`. The API utilization signal specifically measures local API work, not time blocked on the dependency. The sample uses these actual metric names:

- Target p95: `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="optiscale-demo",app="demo-api"}[1m]))) * 1000`
- Target active work: `sum(avg_over_time(http_active_work{namespace="optiscale-demo",app="demo-api"}[1m]))` (threshold `30`)
- Inventory p95: `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="optiscale-demo",app="inventory-service"}[1m]))) * 1000`
- Inventory active work: `sum(avg_over_time(http_active_work{namespace="optiscale-demo",app="inventory-service"}[1m]))` (threshold `100`; latency is the dependency scenario signal)

When the target p95 is above its SLO and target active-work evidence exceeds its threshold, the policy may scale the target. When target saturation is absent and a configured dependency crosses its latency or utilization threshold, it may scale that dependency if `scalable: true`. Both decisions use configured min/max bounds, step limits, and cooldown. Healthy evidence holds; invalid bounds, unavailable telemetry, conflicting bottlenecks, or an unscalable dependency enter protected mode. No replica write is made for a no-op.

`status.lastDecision` contains the latest analysis, evidence, qualitative confidence, action, chosen workload, and replica values. `status.currentReplicas` and `desiredReplicas` continue to describe the primary `demo-api` target. `status.lastScaleDecision` retains the most recent mutating decision across later HOLD or protected-mode evaluations.

## Layout

- `api/v1alpha1`: typed OptiScaler spec, dependency fields, status, and DecisionRecord.
- `cmd/optiscaler`: controller manager.
- `cmd/demo-app`, `cmd/inventory-service`: instrumented demo services.
- `internal/observation`, `internal/capacity`, `internal/policy`: typed observations, deterministic analyzer, and pure guardrail policy.
- `internal/controller`: reconciliation and Deployment scale-subresource calls.
- `deploy/`: canonical namespace, demo workloads, load generator, and scenario profiles.
- `config/`: CRD, RBAC, Prometheus, controller, and sample resource.
- `docs/architecture.md`: implemented V2 behavior and safety boundaries.

## Checks

```powershell
go fmt ./...
go test ./...
go build ./...
git diff --check
```

Passing local Go checks do not imply that either Minikube scenario has been run. Runtime results should be reported only after observing the actual workloads, Prometheus samples, `/scale` changes, and OptiScaler status in a cluster.
