# OptiScale portfolio demo guide

## Prepare the dashboard and success run

Install the local stack with `make install`, then run `deploy/grafana/open-dashboard.ps1` in its own PowerShell window. Open `http://localhost:3000` and select **OptiScale Capacity Governor**. The port-forward is loopback-only.

The predictive-success k6 profile runs 65 seconds of baseline validation, 45 seconds of controller warmup, a 180-second ramp, and a 45-second plateau (335 seconds of scheduled traffic, plus startup). Start the proof before the short walkthrough:

```powershell
.\deploy\scenarios\predictable-ramp-success.ps1
```

The dashboard refreshes every five seconds. For controller state and the decision explanation, use a second terminal:

```powershell
kubectl -n optiscale-demo get optiscaler demo-api -w
```

## 60-second walkthrough

Use a completed or currently running success proof; the workload itself takes several minutes and is not a 60-second test.

- **0-10s — demand:** show the rising demo-api request rate.
- **10-20s — user impact:** compare p95 to the visible 250ms SLO line; point out that the prescale decision was made while p95 was still healthy.
- **20-30s — useful capacity:** show effective-serving replicas stepping from 2 to 3. Requested and Kubernetes Ready counts are not Prometheus series in this deployment; verify them with `kubectl get deployment demo-api -n optiscale-demo`.
- **30-40s — constrained resource:** compare aggregate and hottest held-slot occupancy to the 15-slot safe operating boundary.
- **40-50s — dependency context:** show inventory and PostgreSQL-query p95 to distinguish a target-demand prescale from a downstream bottleneck.
- **50-60s — explain the decision:** inspect `status.lastScaleDecision` in the OptiScaler YAML for predictive demand, forecast, realized safe capacity, confidence, and planning-horizon evidence. These values are not exported as Prometheus series; the dashboard's success/error RPS panel is an explicit substitute, not a fabricated forecast panel.

The supplied 2026-09-12 run showed a new `PRESCALE_TARGET` from 2 to 3 with HIGH forecast confidence, a 51-second planning horizon, and healthy p95; the third replica became both Ready and effective-serving before the SLO boundary. See the README for the recorded numeric evidence. Treat it as evidence from this controlled run, not as a universal production guarantee.

For the predictive event itself, watch the dashboard around the decision:

- **Before:** demand rises while p95 remains below 250ms; effective-serving count is 2, and inventory/PostgreSQL p95 stays healthy. Read the forecast acceptance and safe-capacity comparison from the OptiScaler status, not from Grafana.
- **During:** when a new `PRESCALE_TARGET` is recorded, note the 2 -> 3 scale decision and keep watching p95 against the SLO line. The dashboard does not ingest Kubernetes Events or controller status as Prometheus series.
- **After:** confirm the effective-serving series reaches 3 as traffic arrives at the new replica, p95 remains healthy, and the controller holds rather than requesting another target scale. Check requested/Ready counts with Kubernetes because those series are not scraped.

## Complementary scenarios

The original profile is a capacity-realization guardrail case. It retains normal long-lived connection reuse and may show 3 Kubernetes Ready replicas while only 2 have meaningful recent traffic. OptiScale should not credit the idle Ready replica or blindly continue scaling; the run may cross the SLO and is not the success proof.

```powershell
.\deploy\scenarios\predictable-ramp.ps1
```

The database bottleneck profile demonstrates `CAPACITY_BLOCKED` / `HOLD` when the non-scalable PostgreSQL dependency is the measured bottleneck:

```powershell
.\deploy\scenarios\database-bottleneck.ps1
```

## Interview narrative and limits

> requested replicas are not necessarily useful capacity

Kubernetes readiness is necessary, but OptiScale credits realized target capacity using recent traffic-bearing replicas. Connection reuse or Service traffic distribution can plausibly delay traffic reaching a new endpoint; this demo does not prove a specific networking mechanism. The dashboard is visualization only and is not a controller input. Requested/Ready replicas require Kubernetes inspection because kube-state-metrics is not deployed. Forecast demand and realized safe capacity currently live in OptiScaler status/DecisionRecord rather than Prometheus, so Grafana shows observed success/error RPS instead. The demo is local, uses a controlled k6 arrival profile with bounded periodic connection rotation in the success case, and does not establish behavior for arbitrary production traffic. PostgreSQL panels show application-observed query latency, not database-internal utilization.
