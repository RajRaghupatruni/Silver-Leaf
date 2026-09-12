# Metrics and policy reference

This page describes the metrics, queries, thresholds, capacity calculation, and predictive gates used by the sample OptiScaler configuration. Metric expressions can be changed per OptiScaler resource; the controller does not infer query meaning by parsing PromQL.

## Observation model

Prometheus discovers annotated pods in <code>optiscale-demo</code> and scrapes every five seconds. General instant-query observations are treated as fresh when no more than two minutes old and no more than 30 seconds ahead of controller time. The predictive trend applies a stricter latest-sample age limit of 45 seconds.

Both HTTP applications expose monotonic <code>http_requests_total</code> and <code>http_errors_total</code> counters, the <code>http_request_duration_seconds</code> histogram, and request/work gauges. The demo-api histogram has 25 ms bucket boundaries from 200 ms through 300 ms around the sample SLO. The controller derives successful RPS by subtracting the error counter rate from the total request rate; it does not assume a missing error series means zero errors.

For demo-api, <code>http_concurrency_slot_seconds_total</code> accumulates the wall-clock duration for which each request holds a concurrency semaphore slot. This includes local work and waiting on inventory, but excludes time queued before acquiring a slot. Queue duration is separately accumulated in <code>http_concurrency_queue_seconds_total</code>; <code>http_concurrency_slots_in_use</code> and <code>http_concurrency_queue_waiters</code> are instantaneous debugging gauges. For demo-api, <code>http_local_work_seconds_total</code> and <code>http_active_work</code> are not target-capacity inputs because they omit downstream wait. Inventory's <code>http_active_work</code> remains its configured dependency-utilization input.

Inventory reports measurements around its actual PostgreSQL queries using <code>db_requests_total</code>, <code>db_errors_total</code>, <code>db_active_requests</code>, and the <code>db_request_duration_seconds</code> histogram. These are application-observed query measurements, not PostgreSQL internals.

### Target queries in the sample OptiScaler

| Measurement | PromQL | Use |
| --- | --- | --- |
| Target p95, milliseconds | <code>histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="optiscale-demo",app="demo-api"}[1m]))) * 1000</code> | User impact compared with <code>slo.targetP95Milliseconds</code> (250 ms in the sample). |
| Aggregate held-slot occupancy | <code>sum(rate(http_concurrency_slot_seconds_total{namespace="optiscale-demo",app="demo-api"}[1m]))</code> | Deployment-wide concurrency-slot time, in slot-seconds/second. Used for capacity learning. |
| Hottest traffic-bearing replica occupancy | <code>max((sum by(instance) (rate(http_concurrency_slot_seconds_total{namespace="optiscale-demo",app="demo-api"}[1m]))) and on(instance) (sum by(instance) (rate(http_requests_total{namespace="optiscale-demo",app="demo-api"}[30s])) &gt; 1))</code> | Saturation is tested against the hottest instance above the meaningful-traffic cutoff, not aggregate occupancy divided by Ready replicas. |
| Effective-serving replicas | <code>count(sum by(instance) (rate(http_requests_total{namespace="optiscale-demo",app="demo-api"}[30s])) &gt; 1) or vector(0)</code> | Number of instances with more than 1 request/second over 30 seconds. |
| Stable total request rate | <code>sum(rate(http_requests_total{namespace="optiscale-demo",app="demo-api"}[1m]))</code> | DecisionRecord telemetry and capacity-learning input. |
| Stable error request rate | <code>sum(rate(http_errors_total{namespace="optiscale-demo",app="demo-api"}[1m]))</code> | Error ratio and successful-rate derivation. |
| Predictive demand rate | <code>sum(rate(http_requests_total{namespace="optiscale-demo",app="demo-api"}[30s]))</code> | Separate faster target rate used only to fit the demand trend. |

Successful request rate is stable total request rate minus stable error request rate. Error ratio is error RPS divided by total RPS. A zero denominator leaves the ratio undefined, not zero. Missing, stale, malformed, negative, NaN, or infinite observations remain unavailable.

The sample defines a physical target concurrency limit of 20, matching <code>DEMO_CONCURRENCY=20</code>. With a <code>safeCapacityMargin</code> of 0.75, its derived safe occupancy boundary is 15 slots per traffic-bearing replica. The sample target bound is 1 to 5 replicas, the maximum scale-up step is 1, and cooldown is 30 seconds.

### Dependency queries in the sample

| Component | Latency query | Utilization query | Configured thresholds |
| --- | --- | --- | --- |
| <code>inventory-service</code> | <code>histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{namespace="optiscale-demo",app="inventory-service"}[1m]))) * 1000</code> | <code>sum(avg_over_time(http_active_work{namespace="optiscale-demo",app="inventory-service"}[1m]))</code> | p95 &gt; 250 ms or work &gt; 100; configured scalable. |
| <code>postgres</code> | <code>histogram_quantile(0.95, sum by (le) (rate(db_request_duration_seconds_bucket{namespace="optiscale-demo",app="inventory-service"}[1m]))) * 1000</code> | <code>sum(avg_over_time(db_active_requests{namespace="optiscale-demo",app="inventory-service"}[1m]))</code> | query p95 &gt; 250 ms or active DB requests &gt; 8; configured non-scalable. |

Inventory additionally reports HTTP request/error rates. Its PostgreSQL query request/error rates are based on <code>db_requests_total</code> and <code>db_errors_total</code>. <code>db_active_requests</code> includes inventory calls waiting for a database connection as well as calls executing the query. No database exporter is installed.

An experimental direct sample-SLO compliance query is:

<code>sum(rate(http_request_duration_seconds_bucket{namespace="optiscale-demo",app="demo-api",le="0.25"}[1m])) / sum(rate(http_request_duration_seconds_count{namespace="optiscale-demo",app="demo-api"}[1m]))</code>

A defined result of at least 0.95 indicates that at least 95% of requests are within 250 ms. This query is for runtime evidence and Grafana visualization; it does not replace the configured p95 query or create a hardcoded controller SLO.

## Classification and policy precedence

| Classification | Implemented rule | Resulting behavior |
| --- | --- | --- |
| <code>HEALTHY</code> | Target p95 is at or below its configured SLO. | Base policy holds; predictive evaluation may be considered. |
| <code>TARGET_SATURATED</code> | Target p95 exceeds SLO and hottest traffic-bearing target occupancy is at or above the derived safe boundary. | Bounded target scale-up, subject to cooldown and replica limits. |
| <code>DEPENDENCY_SATURATED</code> | Target p95 exceeds SLO, target is below its local saturation boundary, and one configured scalable root dependency exceeds latency or utilization threshold. | Bounded scale-up of that dependency. |
| <code>CAPACITY_BLOCKED</code> | A configured non-scalable root dependency exceeds its latency or utilization threshold while target-local saturation is absent. | <code>HOLD</code>; target and upstream dependency scaling are rejected. |
| <code>UNCERTAIN</code> | Required target/dependency measurements or configuration are invalid, incomplete, or ambiguous. | <code>PROTECTED_MODE</code>; current replica count is preserved. |

The target saturation rule is evaluated before dependency attribution. Dependency activity does not trigger scaling while target p95 satisfies its SLO. When target latency is high and a dependency is saturated, a downstream component's inflated latency is not enough to label it a bottleneck if its own utilization is below threshold. Multiple independent saturated roots or unresolved attribution produce <code>UNCERTAIN</code>. These rules interpret co-observed measurements; they do not prove causality.

The classification <code>CAPACITY_BLOCKED</code> maps to action <code>HOLD</code>; it is not itself an action. The implemented actions are <code>SCALE_TARGET</code>, <code>SCALE_DEPENDENCY</code>, <code>PRESCALE_TARGET</code>, <code>HOLD</code>, and <code>PROTECTED_MODE</code>. The controller supports Deployment targets through the Kubernetes Scale subresource only. No-op decisions do not write. Replica minimum/maximum, maximum step, and cooldown are enforced by policy.

The active capacity-analysis policy scales up by bounded steps or holds; it does not scale down. Although the API contains <code>scaleUpThreshold</code>, <code>scaleDownThreshold</code>, and <code>maxScaleDownStep</code> fields, those scalar thresholds do not drive the capacity analyzer and do not implement downscaling.

## Target capacity estimation

The physical semaphore limit and safe operating boundary have distinct meanings:

- Physical limit: <code>physicalConcurrencyLimit</code> (20 slots per sample pod).
- Safe operating occupancy: physical limit multiplied by <code>safeCapacityMargin</code> (20 x 0.75 = 15 slots).
- Capacity-learning floor: 30% of the safe operating boundary (4.5 slots per effective-serving replica in the sample).

For each eligible healthy observation:

1. <code>throughputEfficiency = successful RPS / aggregate held-slot occupancy</code>.
2. The estimator takes the nearest-rank 20th percentile of at least three healthy efficiency samples.
3. <code>estimatedPhysicalRPSPerReplica = conservative efficiency x physicalConcurrencyLimit</code>.
4. <code>safeRPSPerReplica = estimatedPhysicalRPSPerReplica x safeCapacityMargin</code>.
5. <code>realizedCurrentSafeCapacity = safeRPSPerReplica x effectiveServingReplicas</code>.

This is a conservative empirical linear extrapolation from observed throughput per held-slot equivalent; it is not a queueing model. Ready replicas without meaningful request traffic do not contribute to realized capacity.

A capacity sample is eligible only when the target classification is healthy; dependencies are healthy; required target rates, errors, p95, aggregate/hottest occupancy, and serving-count observations are complete and fresh; error ratio is at or below the configured limit (0.02 in the sample); requested replicas, Ready replicas, and effective-serving replicas agree; the effective-serving count is positive; aggregate occupancy is positive; mean occupancy per serving replica is at least 30% of the safe boundary and below it; and hottest-serving occupancy remains below the boundary. An unresolved Ready-but-not-serving replica prevents learning.

## Predictive demand model

The stable 1-minute target request-rate query continues to supply request/error telemetry and capacity learning. Forecasting uses only the separately configured 30-second <code>predictiveRequestRateQuery</code>. The controller does not parse the PromQL range. <code>predictiveDemandWindowSeconds</code> must be configured to match the query, and observation lag is derived as half that window (15 seconds for the sample). Prediction requires a configured query and matching window. If that configured query returns no data, stale data, or an invalid value, the controller does not substitute the stable rate; prescaling is suppressed while capacity learning from otherwise eligible stable telemetry can continue.

Demand history is controller-local and bounded to 20 observations from the latest five minutes. Trend fitting uses only the trailing 120 seconds relative to the latest sample timestamp. A fit requires:

- at least 5 observations spanning at least 60 seconds;
- latest observation no more than 45 seconds old or 30 seconds ahead of controller time;
- no sample gap greater than 45 seconds;
- finite, nonnegative rates, positive slope, and finite nonnegative projected rate;
- planning horizon no longer than the observed fit span.

Fit quality is <code>HIGH</code> at <code>R^2 &gt;= 0.95</code> and normalized RMSE <code>&lt;= 0.10</code>. <code>MEDIUM</code> requires <code>R^2 &gt;= 0.85</code> and normalized RMSE <code>&lt;= 0.20</code>; only HIGH can prescale. The normalization uses the observed request-rate range in the fit window. Flat/falling demand, insufficient history, stale/gapped observations, or poor fit are not accepted for prescaling.

The planning horizon is:

<code>measured Pod creation-to-Ready lead + 15-second reconcile allowance + predictive-demand observation lag</code>

Readiness samples use eligible Ready, non-terminating pods from the uniquely resolved current Deployment ReplicaSet revision. Every regular application container must have restart count zero; init-container restart history is ignored. The maximum eligible creation-to-Ready duration is persisted in status with its <code>pod-template-hash</code> identity. If current fresh evidence is absent, the controller reuses learned evidence only for the same revision. Ambiguous rollout identity or unavailable matching readiness evidence blocks prescaling. This does not measure <code>/scale</code>-to-Pod-creation delay.

Prescaling is considered only after the reactive policy returns a healthy <code>HOLD</code>. It additionally requires healthy dependencies, trusted HIGH forecast, at least three eligible capacity samples, a current safe-capacity estimate, all requested replicas Ready and traffic-bearing, no cooldown, and forecast demand strictly above realized current safe capacity. An accepted action uses the existing maximum scale-up step and maximum replica bound.

## Grafana interpretation

The provisioned dashboard uses Prometheus series for request rate, p95/SLO, effective-serving replicas, held-slot occupancy, success/error RPS, and dependency p95. The current Prometheus configuration does not scrape kube-state-metrics, so requested and Kubernetes Ready replicas are checked with <code>kubectl</code>. Forecast demand and realized safe capacity are recorded in OptiScaler status/DecisionRecord, not exported as Prometheus time series; the dashboard substitutes observed success/error RPS instead of inventing forecast data. Grafana is for visualization and is not a controller input.
