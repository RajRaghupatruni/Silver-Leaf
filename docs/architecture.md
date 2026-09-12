# OptiScale architecture

OptiScale is a namespaced Kubernetes controller that evaluates application and dependency telemetry before changing a workload's replica count. Its central distinction is between Kubernetes replicas that exist, replicas that are Ready, and replicas that are demonstrably serving recent traffic.

## Components and data flow

The top-level data flow is shown in the [README architecture diagram](../README.md#architecture).

- **Demo workloads:** <code>demo-api</code> serves HTTP traffic and calls <code>inventory-service</code>. Inventory executes real queries against PostgreSQL. Application endpoints expose Prometheus metrics.
- **Prometheus:** discovers annotated pods in <code>optiscale-demo</code> through Kubernetes pod discovery. Scrape configuration does not embed a node or Minikube address.
- **OptiScaler API:** the namespaced <code>OptiScaler</code> custom resource declares a Deployment target, SLO and metric queries, scaling bounds, policy thresholds, optional dependencies, and optional prediction configuration.
- **Observation layer:** performs typed Prometheus instant queries and reads Kubernetes Deployment Scale, Deployment state, selected Pods, and ReplicaSets. Missing, stale, malformed, non-finite, or impossible observations remain invalid rather than becoming zero.
- **Capacity analyzer:** classifies target and dependency evidence. The controller evaluates the reactive capacity policy first; the optional forecast is considered only if that evaluation is a healthy HOLD.
- **Policy and scale API:** decisions are deterministic, bounded by configured minimum/maximum replicas, maximum step, and cooldown. Scaling reads and updates the Kubernetes <code>apps/v1 Deployment</code> <code>/scale</code> subresource; it does not write <code>Deployment.spec.replicas</code> directly.
- **Decision output:** status carries conditions, the latest <code>DecisionRecord</code>, and the most recent mutating decision. Actual scale changes emit Kubernetes Events and structured controller logs include the measurements and action.
- **Grafana:** provisions a Prometheus datasource and dashboard independently. It visualizes observations but is not read by the controller.

The controller supports Deployment targets only. The controller Deployment is configured to watch OptiScaler resources in <code>optiscale-demo</code>; the controller Role grants namespaced access to OptiScalers, Deployments and their Scale subresource, Pods, ReplicaSets, and Events. It does not require cluster-admin.

## Target capacity attribution

The target observation combines user impact (p95 versus SLO) with held concurrency-slot occupancy, request/error measurements, and traffic-bearing replica count. Target saturation requires both an SLO violation and the hottest traffic-bearing replica at or above the derived safe occupancy boundary. This per-replica maximum prevents a low average across the Deployment from hiding a hot serving pod.

For target capacity, the configured physical concurrency limit represents the application's actual semaphore ceiling. The configured safety margin derives a lower safe operating boundary. Healthy throughput observations estimate safe per-serving-replica throughput, while realized current safe capacity is multiplied by effective-serving replicas rather than all Ready replicas. Detailed equations and eligibility rules are in [Metrics and policy](metrics-and-policy.md).

## Dependency attribution

Dependencies are explicitly named in each OptiScaler resource and have their own latency/utilization queries, thresholds, scale bounds, and scalable flag. The analyzer evaluates the configured topology rather than discovering arbitrary service relationships.

When target p95 is above SLO but target-local saturation is not present, a single configured saturated scalable dependency can be selected for bounded scaling. When a non-scalable root dependency is the measured bottleneck, OptiScale returns <code>CAPACITY_BLOCKED</code> and <code>HOLD</code>, records why scaling the target and upstream dependency would not address that bottleneck, and leaves their replica counts unchanged. If independent bottlenecks make attribution ambiguous, the analyzer returns <code>UNCERTAIN</code>.

The database workload uses a real PostgreSQL service and deterministic <code>pg_sleep</code> queries. Database latency and active-query measurements are observed by the inventory application around its actual queries; they do not represent PostgreSQL CPU, lock, or internal wait-state metrics.

## Useful capacity realization

Kubernetes Ready is necessary for traffic service but is not sufficient evidence that a replica is receiving meaningful traffic. The sample query defines an effective-serving instance as one with request rate greater than 1 request/second over a 30-second rate window. The controller validates that the resulting count is an integer from zero through the Ready replica count.

If requested or Ready replicas exceed the effective-serving count, capacity learning is suspended and a target scale-up or prescale is held. This avoids crediting an idle Ready replica with safe capacity and prevents repeated blind increases while earlier requested capacity has not materialized. Connection reuse or traffic-distribution behavior can plausibly delay traffic to a newly Ready endpoint, but current measurements do not identify a specific networking cause.

Readiness latency is measured as Pod creation timestamp to Pod Ready transition for eligible current-revision pods whose regular containers have not restarted. Ready restarted pods still count as Ready, but do not contribute a new-startup sample. The largest eligible fresh sample is persisted in status with its Kubernetes pod-template-hash identity and can be reused only for that same target revision. An ambiguous rollout or unmatched persisted measurement blocks prescaling.

## Predictive planning

Prediction is optional and advisory. It cannot override reactive target saturation, a dependency bottleneck, <code>CAPACITY_BLOCKED</code>, invalid required observations, or an active cooldown. It may replace only a healthy <code>HOLD</code> after forecast quality, healthy-capacity evidence, dependency health, and complete capacity realization have been verified.

The planning horizon is measured Pod creation-to-Ready lead time plus the 15-second reconcile allowance plus half the configured predictive request-rate window. The sample's 30-second demand query therefore contributes a 15-second observation lag. The end-to-end interval from a <code>/scale</code> request to Pod creation is not measured or added as an assumed constant. Forecast history and healthy-capacity history are bounded in controller memory and reset when the controller restarts; learned readiness latency is separately persisted in OptiScaler status.

See [Metrics and policy](metrics-and-policy.md) for the exact trend-fit window, quality gates, capacity estimator, freshness limits, and sample PromQL.

## Status and decision records

<code>status.lastDecision</code> describes the latest evaluation; <code>status.lastScaleDecision</code> retains the last decision that caused a real scale-subresource update. The record can include:

- target p95/SLO, stable and predictive request rates, error ratio, and dependency request summaries;
- capacity classification, bottleneck component, evidence, confidence, chosen workload, and rejected actions;
- requested, Ready, and effective-serving replica counts plus aggregate and hottest held-slot occupancy;
- safe per-serving-replica throughput, realized current safe capacity, and physical/safe occupancy boundaries;
- forecast rate, fit quality, slope, planning horizon, readiness evidence source/revision, and acceptance or rejection reason.

Status updates compare semantic status content before writing so an unchanged condition/decision does not cause an update loop. Scale-related Events distinguish target scaling, dependency scaling, and prescaling. Grafana does not replace these Kubernetes-native decision records.

## Current boundaries

OptiScale does not discover arbitrary dependency graphs, persist forecast/capacity histories, coordinate histories across controller instances, infer PostgreSQL internals, provision nodes, or provide a general-purpose global optimizer. Its local environment is Minikube; the included dashboard and workload profiles are intended for local operation and observation.
