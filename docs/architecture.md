# OptiScale Vertical Slice 3 architecture

```text
loadgen → demo-api → inventory-service → PostgreSQL
             /metrics         /metrics + DB-query metrics
                      ↓ Kubernetes pod discovery
                   Prometheus
                      ↓ typed, timestamped observations
                 capacity analyzer
 HEALTHY / TARGET_SATURATED / DEPENDENCY_SATURATED /
          CAPACITY_BLOCKED / UNCERTAIN
                      ↓ deterministic policy and guardrails
           Deployment /scale subresource (when useful)
                      ↓
         status.lastDecision + lastScaleDecision
```

## Services and telemetry

`demo-api` performs configurable local work and makes a bounded HTTP call to `inventory-service`. Inventory performs a real SQL query against the local PostgreSQL service. Each HTTP service exports request/error counters, in-flight request and local-work gauges, and a request-duration histogram. Inventory also exports application-observed database request/error counters, in-flight database requests, and a DB-query duration histogram. Prometheus discovers annotated pods dynamically in the demo namespace; neither service address nor a Minikube IP is embedded in scrape configuration. The target and inventory utilization queries use `http_active_work`, so dependency waiting is not counted as local work. The DB active-query gauge includes calls waiting on a database connection and calls executing queries; it is not a claim about PostgreSQL CPU or internal wait states.

The OptiScaler's target query supplies p95 latency in milliseconds; a second target query supplies averaged active local work. Each configured dependency has its own latency and utilization queries and thresholds. Request and error rates use `rate()` over the existing monotonically increasing total/error counters. The controller retains total requests per second, derives successful requests per second as total minus errors, and derives the error ratio as error RPS divided by total RPS. A zero request rate leaves the error ratio undefined; missing, malformed, non-finite, stale, or future-dated observations stay absent rather than becoming zero.

Signal roles are intentionally separate: p95/SLO asks whether users are suffering; local active-work telemetry indicates component constraint; request/successful-request rates quantify served demand; and error ratio describes failure rate and whether that reliability observation is trustworthy. Request/error telemetry is recorded in the latest DecisionRecord for capacity analysis, not used as a scaling trigger in this slice.

## Analyzer rules

- `UNCERTAIN`: required telemetry or configuration evidence is invalid/incomplete, multiple independent dependencies qualify, or an upstream component has independent utilization saturation alongside a saturated downstream dependency.
- `HEALTHY`: the target p95 is at or below its SLO. Dependency activity alone does not trigger scaling while the target SLO is satisfied.
- `TARGET_SATURATED`: target p95 exceeds its SLO and target active-work evidence exceeds its configured threshold. This local target signal takes priority over dependency latency signals.
- `DEPENDENCY_SATURATED`: target p95 exceeds its SLO, target active-work evidence is below its threshold, and one configured dependency crosses its latency or utilization threshold.
- `CAPACITY_BLOCKED`: target p95 exceeds its SLO, target is not locally saturated, and a root dependency marked non-scalable crosses a configured latency or active-query threshold. A component declared as depending on that root is not selected solely because its request latency also rose; independent utilization saturation remains ambiguous. Evidence identifies which thresholds fired, the non-scalable configuration, target utilization, and upstream topology.

The analyzer emits classification, component, evidence, reason, and a qualitative confidence level based on evidence completeness. These rules describe co-observed metrics only; they do not establish causality.

## Policy and Kubernetes writes

Target saturation selects the target Deployment; dependency saturation selects that dependency only when `scalable: true`. A verified non-scalable root bottleneck produces `CAPACITY_BLOCKED` and `HOLD`, preserving current replicas and listing `SCALE_TARGET` and `SCALE_DEPENDENCY` as rejected actions with reasons. This prevents adding API or inventory replicas when the measured database dependency is the bottleneck. Healthy evidence holds; uncertain evidence or invalid bounds enters `PROTECTED_MODE`. Upward changes use the configured max step and per-workload maximum; configured minimums are retained as safety bounds. Cooldown applies across target and dependency changes. A no-op decision never writes.

The controller supports `apps/v1 Deployment` only. It reads and updates `autoscaling/v1.Scale` through `deployments/scale`; it does not directly mutate `Deployment.spec.replicas`. RBAC is namespace-scoped and includes `deployments/scale` access in the demo namespace. The controller retains V1's latency-threshold policy path for OptiScaler objects without V2 utilization/dependency fields.

## Status and explainability

`status.lastDecision` records SLO target, observed target p95, target request/successful-request rates and error ratio when defined, compact per-dependency request/error summaries, bottleneck classification/component, evidence, qualitative confidence, chosen action/workload, rejected alternatives, current/desired replicas, and reason. Top-level status replica fields continue to describe the primary target. `status.lastScaleDecision` changes only after a real `/scale` mutation, so later HOLD or protected evaluations do not erase the most recent scaling rationale. One structured controller log is emitted per capacity evaluation with target, p95, SLO, bottleneck, action, workload, replicas, and reason.

## Scope boundary

This slice does not implement prediction, Random Forests, arbitrary dependency graphs, PostgreSQL-internal telemetry, OpenTelemetry, Grafana, Karpenter, capacity learning, replay, or causal inference. PostgreSQL is a real local workload; its deterministic delay uses a real SQL `pg_sleep` call, and observed query metrics describe application behavior rather than database internals. The configured P0 dependency list and deterministic rules are a demonstration, not a general optimizer.
