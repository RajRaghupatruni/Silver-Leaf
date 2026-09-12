# Local operating guide

This guide runs the repository's local Kubernetes environment and explains the included workload profiles. The repository provides a reproducible local environment, not a permanently hosted public Kubernetes service.

## Prerequisites and installation

You need Docker, Minikube, kubectl, Make, and Go 1.23 or newer. Start Minikube, create the PostgreSQL Secret, build/load the local images, then install the resources:

~~~text
minikube start --cpus=4 --memory=4096
kubectl apply -f deploy/namespace.yaml
make docker-build
make minikube-load
make install
make status
~~~

Before installation, the namespace <code>optiscale-demo</code> needs Secret <code>optiscale-postgres</code> with keys <code>username</code> and <code>password</code>. The repository includes <code>deploy/postgres/create-secret.ps1</code>, a PowerShell helper for local Secret creation. Its default password is local-only; supply your own password for other environments and do not reuse the default.

<code>make install</code> applies the namespaced CRD, RBAC, Prometheus, Grafana, PostgreSQL, inventory service, demo API, controller, and sample OptiScaler. <code>make loadgen</code> starts the optional continuous curl-based load generator. The k6 profiles are launched by the PowerShell scenario helpers; no Bash launcher is included.

## Dashboard and controller state

In a PowerShell terminal, run the dashboard helper:

~~~powershell
.\deploy\grafana\open-dashboard.ps1
~~~

Open <code>http://localhost:3000</code>. The helper uses a loopback-only kubectl port-forward. Grafana is provisioned with anonymous Viewer access for the local environment, an ephemeral data directory, and a Prometheus datasource. Do not expose this configuration as a public service.

The dashboard shows demand RPS, demo-api p95 against 250 ms, effective-serving replicas, held-slot occupancy, success/error RPS, and inventory/PostgreSQL query p95. This Prometheus installation does not include kube-state-metrics, so requested and Ready replicas are checked with Kubernetes:

~~~text
kubectl -n optiscale-demo get deployment demo-api
kubectl -n optiscale-demo get optiscaler demo-api -o yaml
kubectl -n optiscale-system logs deployment/optiscaler-controller
~~~

The OptiScaler resource's <code>status.lastDecision</code> explains the latest evaluation. <code>status.lastScaleDecision</code> retains the last evaluation that made a real scale change. Forecast and realized safe-capacity measurements are in status/DecisionRecord, not Prometheus time series.

## Workload profiles

Run one profile at a time. The scenario scripts modify workload environment variables and replica settings; review each script before running it in a shared cluster.

| PowerShell helper | Purpose and expected behavior |
| --- | --- |
| <code>deploy/scenarios/target-saturation.ps1</code> | Raises demo-api local demand; expects <code>TARGET_SATURATED</code> and bounded <code>SCALE_TARGET</code>. |
| <code>deploy/scenarios/dependency-saturation.ps1</code> | Exercises a saturated scalable inventory dependency; expects <code>DEPENDENCY_SATURATED</code> and bounded <code>SCALE_DEPENDENCY</code>. |
| <code>deploy/scenarios/database-bottleneck.ps1</code> | Uses real PostgreSQL queries with deterministic <code>pg_sleep</code>; PostgreSQL is configured non-scalable, so the expected result is <code>CAPACITY_BLOCKED</code> / <code>HOLD</code> with upstream replica counts unchanged. |
| <code>deploy/scenarios/predictable-ramp-success.ps1</code> | Runs the open-loop k6 profile with moderate periodic connection rotation and validates a new <code>PRESCALE_TARGET</code>, real 2 -> 3 target scaling, third-replica readiness below SLO, and a new Kubernetes Event. |
| <code>deploy/scenarios/predictable-ramp.ps1</code> | Uses the long-lived connection profile to exercise the capacity-realization guard. Ready may exceed effective-serving count after scale-up; OptiScale should hold further target scaling. This run may cross the SLO and is not the SLO-preservation profile. |

The successful predictive profile schedules 200 RPS during baseline and controller warmup, ramps linearly from 200 to 480 RPS over 180 seconds, then holds 480 RPS for 45 seconds. Approximately every 25th iteration per VU requests connection close; this is a controlled local traffic model, not a general production assumption. The recorded successful run and its measurements are in the [README runtime proof](../README.md#runtime-proof).

## Understanding the two capacity outcomes

The predictive success profile uses moderate connection turnover so the new Ready endpoint has opportunities to receive fresh connections. In the recorded run, the third replica became Ready and traffic-bearing before the SLO boundary.

The capacity-realization profile retains normal long-lived connection reuse. A new endpoint can be Ready while recent request telemetry still shows only two effective-serving replicas. OptiScale does not credit that endpoint as realized capacity and holds further target scale-ups until traffic confirms useful service. Connection reuse or Service traffic distribution can plausibly explain delayed traffic, but the observed metrics do not prove a particular network mechanism.

The PostgreSQL profile demonstrates a different decision: latency may be caused by a non-scalable downstream bottleneck, in which case adding demo-api or inventory replicas does not create database capacity. OptiScale records the PostgreSQL bottleneck, returns <code>CAPACITY_BLOCKED</code> / <code>HOLD</code>, and reports why upstream scale actions were rejected.

## Restore or remove the local environment

Reapply workload manifests to restore their declared settings:

~~~text
kubectl apply -f deploy/inventory/deployment.yaml
kubectl apply -f deploy/demo-app/deployment.yaml
kubectl apply -f deploy/loadgen/deployment.yaml
~~~

The inventory manifest restores its declared PostgreSQL delay and baseline settings. To remove the local lab resources, use <code>make uninstall</code>; this deletes the lab namespaces and the CRD as well as the deployments. PostgreSQL uses ephemeral storage in this environment.
