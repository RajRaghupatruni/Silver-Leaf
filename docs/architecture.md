# OptiScale Vertical Slice 1 architecture

Vertical Slice 1 intentionally proves the control-loop boundary with a deterministic policy. It does not implement forecasting or dependency-aware optimization.

```text
demo-app /metrics
       ↓
Prometheus Kubernetes pod discovery
       ↓ instant query
OptiScaler controller
       ↓ validated observation
pure policy function
       ↓ guardrails and cooldown
Deployment scale subresource
       ↓
OptiScaler status.lastDecision + Conditions + Event
```

## Observation

The demo application exports a request-duration histogram. Prometheus calculates a p95 latency in milliseconds. The sample OptiScaler queries that value.

The Prometheus client requires a successful HTTP response, a successful Prometheus API response, exactly one result, a numeric value, and finite telemetry. Missing or malformed data is not converted to zero.

## Policy

The policy is a pure function. It receives current replicas, observed telemetry, bounds, thresholds, step limits, cooldown state, and telemetry freshness. It returns one of:

- `SCALE_TARGET`
- `HOLD`
- `PROTECTED_MODE`

Scale-up and scale-down thresholds are strict. Invalid or stale telemetry produces `PROTECTED_MODE`; no scale-down is allowed in that mode.

## Kubernetes mutation

The controller supports `apps/v1 Deployment` targets for this slice. It reads `autoscaling/v1.Scale` and updates the same scale subresource only when desired replicas differ from current replicas. It does not directly mutate `Deployment.spec.replicas`.

The controller has namespace-scoped permissions in the demo namespace for OptiScaler objects, status, Deployments, `deployments/scale`, and Events. It does not use cluster-admin.

## Explainability

The latest structured `DecisionRecord` is stored in `OptiScaler.status.lastDecision`. It includes a stable decision ID, timestamp, action, reason, observed metric, threshold, current replicas, and desired replicas. Conditions expose readiness and protected-mode state using Kubernetes `metav1.Condition` conventions.

## Explicit non-goals

This slice does not contain prediction, Random Forests, dependency graphs, PostgreSQL logic, OpenTelemetry, Grafana, Karpenter, capacity learning, policy replay, or multi-workload optimization.
