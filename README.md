# OptiScale — Vertical Slice 1

OptiScale is being developed as an explainable, SLO-aware, dependency-aware Kubernetes Capacity Governor.

This repository currently implements only Vertical Slice 1:

```text
demo HTTP app
  → Prometheus telemetry
  → OptiScaler v1alpha1 resource
  → Go controller
  → deterministic policy and guardrails
  → Deployment /scale subresource
  → explainable status.lastDecision
```

Forecasting, Random Forests, dependency graphs, PostgreSQL bottleneck logic, OpenTelemetry, Grafana, Karpenter, capacity learning, policy replay, and sophisticated optimization are not implemented.

## Project layout

- `api/v1alpha1`: typed OptiScaler API and status schema.
- `cmd/optiscaler`: controller binary.
- `cmd/demo-app`: small instrumented HTTP workload.
- `internal/policy`: pure deterministic scaling policy and tests.
- `internal/prometheus`: typed Prometheus instant-query client and tests.
- `internal/controller`: reconciliation, scale-subresource mutation, Events, and status updates.
- `config/`: CRD, RBAC, controller, Prometheus, and sample resources.
- `docs/architecture.md`: Vertical Slice 1 architecture.

## Prerequisites

- Go 1.23 or newer
- Docker
- kubectl
- Minikube

The controller and demo images use explicit image tags. For local Minikube testing, images are loaded directly into the Minikube runtime.

## Run locally with Minikube

From the repository root:

```sh
minikube start
make docker-build
make minikube-load
make install
```

To generate load, apply the included load generator:

```sh
make loadgen
kubectl -n optiscale-demo get optiscaler demo-app -w
kubectl -n optiscale-demo get deployment demo-app -w
kubectl -n optiscale-demo describe optiscaler demo-app
```

To inspect controller logs:

```sh
kubectl -n optiscale-system logs deployment/optiscaler-controller -f
```

To inspect Prometheus locally:

```sh
kubectl -n optiscale-system port-forward service/prometheus 9090:9090
```

The sample policy observes p95 request latency in milliseconds. Values above `250` scale up by at most one replica per decision; values below `150` scale down by at most one replica, subject to the 30-second cooldown and replica bounds.

The Makefile targets are `fmt`, `test`, `build`, `docker-build`, `minikube-load`, `install`, `loadgen`, `status`, `uninstall`, and `generate`. On Windows, run the equivalent commands directly in PowerShell if GNU Make is unavailable; each target is composed only of `go`, `docker`, `minikube`, and `kubectl` commands without Unix-specific shell pipelines.

## Safety behavior

- Missing, malformed, stale, NaN, or infinite Prometheus data enters `PROTECTED_MODE`.
- Protected mode never scales down.
- Policy thresholds are strict: equality holds.
- Replica changes are clamped to `minReplicas` and `maxReplicas`.
- Scale step limits prevent large one-reconcile jumps.
- Cooldown suppresses repeated scale writes.
- Identical current and desired replica counts do not update the Kubernetes scale subresource.
- `status.lastDecision` records the action, reason, metric, threshold, and replica counts.

## Verification

Run:

```sh
gofmt -w api cmd internal
go test ./...
go build ./...
```

The repository does not claim a successful Kubernetes deployment validation unless a reachable cluster is available.
