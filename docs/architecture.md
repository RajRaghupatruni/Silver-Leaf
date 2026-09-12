# OptiScale Vertical Slice 2 architecture

```text
loadgen → demo-api → inventory-service
              ↘           ↙
             /metrics endpoints
                    ↓ Kubernetes pod discovery
                 Prometheus
                    ↓ typed, timestamped instant-query observations
              capacity analyzer
       HEALTHY / TARGET_SATURATED /
       DEPENDENCY_SATURATED / UNCERTAIN
                    ↓ deterministic policy and guardrails
         Deployment /scale subresource
                    ↓
       status.lastDecision + lastScaleDecision
```

## Services and telemetry

`demo-api` performs configurable local work and makes a bounded HTTP call to `inventory-service`. Each service exports request/error counters, in-flight request and local-work gauges, and a request-duration histogram. Prometheus discovers annotated pods dynamically in the demo namespace; neither service address nor a Minikube IP is embedded in scrape configuration. The target utilization query uses `http_active_work`, so downstream waiting is not misreported as target-local saturation.

The OptiScaler's target query supplies p95 latency in milliseconds; a second target query supplies averaged active local work. Each configured dependency has its own latency and utilization queries and thresholds. A missing, malformed, non-finite, stale, or future-dated query result is invalid evidence, never zero.

## Analyzer rules

- `UNCERTAIN`: required telemetry or configuration evidence is invalid/incomplete, or target and dependency saturation conflict, or multiple dependencies qualify.
- `HEALTHY`: the target p95 is at or below its SLO. Dependency activity alone does not trigger scaling while the target SLO is satisfied.
- `TARGET_SATURATED`: target p95 exceeds its SLO and target active-work evidence exceeds its configured threshold, with no conflicting dependency classification.
- `DEPENDENCY_SATURATED`: target p95 exceeds its SLO, target active-work evidence is below its threshold, and one configured dependency crosses its latency or utilization threshold.

The analyzer emits classification, component, evidence, reason, and a qualitative confidence level based on evidence completeness. These rules describe co-observed metrics only; they do not establish causality.

## Policy and Kubernetes writes

Target saturation selects the target Deployment; dependency saturation selects that dependency only when `scalable: true`. Healthy evidence holds. Uncertain evidence, invalid bounds, or a non-scalable bottleneck enters `PROTECTED_MODE`. Upward changes use the configured max step and per-workload maximum; configured minimums are retained as safety bounds. Cooldown applies across target and dependency changes. A no-op decision never writes.

The controller supports `apps/v1 Deployment` only. It reads and updates `autoscaling/v1.Scale` through `deployments/scale`; it does not directly mutate `Deployment.spec.replicas`. RBAC is namespace-scoped and includes `deployments/scale` access in the demo namespace. The controller retains V1's latency-threshold policy path for OptiScaler objects without V2 utilization/dependency fields.

## Status and explainability

`status.lastDecision` records SLO target, observed target p95, bottleneck classification/component, evidence, qualitative confidence, chosen action/workload, rejected alternatives, current/desired replicas, and reason. Top-level status replica fields continue to describe the primary target. `status.lastScaleDecision` changes only after a real `/scale` mutation, so later HOLD or protected evaluations do not erase the most recent scaling rationale. One structured controller log is emitted per capacity evaluation with target, p95, SLO, bottleneck, action, workload, replicas, and reason.

## Scope boundary

This slice does not implement prediction, Random Forests, arbitrary dependency graphs, PostgreSQL logic, OpenTelemetry, Grafana, Karpenter, capacity learning, replay, or causal inference. The configured P0 dependency list and deterministic rules are a demonstration, not a general optimizer.
