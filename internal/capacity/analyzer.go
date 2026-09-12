package capacity

import (
	"fmt"
	"math"
	"strings"

	"github.com/RajRaghupatruni/Silver-Leaf/internal/observation"
)

type Classification string

const (
	Healthy             Classification = "HEALTHY"
	TargetSaturated     Classification = "TARGET_SATURATED"
	DependencySaturated Classification = "DEPENDENCY_SATURATED"
	CapacityBlocked     Classification = "CAPACITY_BLOCKED"
	Uncertain           Classification = "UNCERTAIN"
)

type Confidence string

const (
	ConfidenceLow    Confidence = "LOW"
	ConfidenceMedium Confidence = "MEDIUM"
	ConfidenceHigh   Confidence = "HIGH"
)

type Analysis struct {
	Classification  Classification
	Component       string
	Evidence        []string
	RejectedActions []string
	Reason          string
	Confidence      Confidence
}

func Analyze(snapshot observation.Snapshot) Analysis {
	if !finiteNonnegative(snapshot.SLOTargetP95Milliseconds) {
		return uncertain("invalid SLO target", "slo.targetP95Milliseconds is invalid")
	}
	if !usable(snapshot.Target.P95Latency) || !usable(snapshot.Target.Utilization) {
		return uncertain("target", metricReason("target telemetry is incomplete", snapshot.Target.P95Latency, snapshot.Target.Utilization))
	}
	if !finiteNonnegative(snapshot.Target.UtilizationThreshold) {
		return uncertain("target", "target utilization threshold is invalid")
	}

	dependencySaturated := make(map[string]observation.DependencyObservation)
	for _, dependency := range snapshot.Dependencies {
		if !usable(dependency.P95Latency) || !usable(dependency.Utilization) {
			return uncertain(dependency.Name, fmt.Sprintf("dependency %q telemetry is incomplete", dependency.Name))
		}
		if !finiteNonnegative(dependency.LatencyThreshold) || !finiteNonnegative(dependency.UtilizationThreshold) {
			return uncertain(dependency.Name, fmt.Sprintf("dependency %q threshold is invalid", dependency.Name))
		}
		if dependency.P95Latency.Value > dependency.LatencyThreshold || dependency.Utilization.Value > dependency.UtilizationThreshold {
			dependencySaturated[dependency.Name] = dependency
		}
	}

	targetSaturated := snapshot.Target.P95Latency.Value > snapshot.SLOTargetP95Milliseconds && snapshot.Target.Utilization.Value > snapshot.Target.UtilizationThreshold
	if snapshot.Target.P95Latency.Value <= snapshot.SLOTargetP95Milliseconds {
		return Analysis{
			Classification: Healthy,
			Component:      "target",
			Evidence:       []string{fmt.Sprintf("target p95 %.2fms is within SLO %.2fms", snapshot.Target.P95Latency.Value, snapshot.SLOTargetP95Milliseconds)},
			Reason:         "target SLO is currently satisfied",
			Confidence:     ConfidenceHigh,
		}
	}
	if targetSaturated {
		return Analysis{
			Classification: TargetSaturated,
			Component:      "target",
			Evidence: []string{
				fmt.Sprintf("target p95 %.2fms exceeds SLO %.2fms", snapshot.Target.P95Latency.Value, snapshot.SLOTargetP95Milliseconds),
				fmt.Sprintf("target utilization %.2f exceeds threshold %.2f", snapshot.Target.Utilization.Value, snapshot.Target.UtilizationThreshold),
			},
			Reason:     "target latency is above SLO and target saturation evidence is present",
			Confidence: ConfidenceHigh,
		}
	}
	rootCandidates := make([]observation.DependencyObservation, 0, len(dependencySaturated))
	for _, dependency := range snapshot.Dependencies {
		if _, saturated := dependencySaturated[dependency.Name]; !saturated {
			continue
		}
		if dependency.DependsOn != "" {
			if _, downstreamSaturated := dependencySaturated[dependency.DependsOn]; downstreamSaturated {
				// Downstream latency can inflate this component's request latency.
				// Its independent utilization signal may still indicate a separate
				// bottleneck, in which case attribution must remain conservative.
				if dependency.Utilization.Value <= dependency.UtilizationThreshold {
					continue
				}
			}
		}
		rootCandidates = append(rootCandidates, dependency)
	}
	if len(rootCandidates) > 1 {
		return uncertain("multiple dependencies", "multiple independent dependencies show saturation evidence; bottleneck attribution is uncertain")
	}
	if len(rootCandidates) == 1 {
		dependency := rootCandidates[0]
		confidence := ConfidenceHigh
		latencyExceeded := dependency.P95Latency.Value > dependency.LatencyThreshold
		utilizationExceeded := dependency.Utilization.Value > dependency.UtilizationThreshold
		evidence := make([]string, 0, 2)
		if latencyExceeded {
			evidence = append(evidence, fmt.Sprintf("dependency %q latency %.2fms exceeds threshold %.2fms", dependency.Name, dependency.P95Latency.Value, dependency.LatencyThreshold))
		}
		if utilizationExceeded {
			evidence = append(evidence, fmt.Sprintf("dependency %q utilization %.2f exceeds threshold %.2f", dependency.Name, dependency.Utilization.Value, dependency.UtilizationThreshold))
		}
		if latencyExceeded != utilizationExceeded {
			confidence = ConfidenceMedium
		}
		if !dependency.Scalable {
			evidence = append(evidence, fmt.Sprintf("dependency %q is configured non-scalable", dependency.Name))
			if snapshot.Target.Utilization.Value <= snapshot.Target.UtilizationThreshold {
				evidence = append(evidence, fmt.Sprintf("target utilization %.2f is at or below threshold %.2f", snapshot.Target.Utilization.Value, snapshot.Target.UtilizationThreshold))
			}
			upstream := make([]string, 0, 1)
			for _, candidate := range snapshot.Dependencies {
				if candidate.DependsOn == dependency.Name {
					upstream = append(upstream, candidate.Name)
					evidence = append(evidence, fmt.Sprintf("dependency %q depends on %q; its latency is downstream evidence", candidate.Name, dependency.Name))
				}
			}
			targetName := snapshot.Target.Name
			if targetName == "" {
				targetName = "target"
			}
			reason := fmt.Sprintf("capacity is blocked by non-scalable dependency %q; target %q is not locally saturated", dependency.Name, targetName)
			if len(upstream) > 0 {
				reason += fmt.Sprintf(", and scaling upstream dependency %q would not add database capacity", strings.Join(upstream, ", "))
			}
			reason += "; adding target replicas would not relieve the database bottleneck"
			return Analysis{
				Classification: CapacityBlocked,
				Component:      dependency.Name,
				Evidence:       evidence,
				Reason:         reason,
				Confidence:     confidence,
				RejectedActions: []string{
					fmt.Sprintf("SCALE_TARGET: target %q is not locally saturated", targetName),
					fmt.Sprintf("SCALE_DEPENDENCY: bottleneck dependency %q is non-scalable", dependency.Name),
				},
			}
		}
		return Analysis{
			Classification: DependencySaturated,
			Component:      dependency.Name,
			Evidence:       evidence,
			Reason:         strings.Join(evidence, "; "),
			Confidence:     confidence,
		}
	}
	if snapshot.Target.P95Latency.Value > snapshot.SLOTargetP95Milliseconds {
		return uncertain("target", "target SLO is violated but saturation evidence is incomplete")
	}
	return Analysis{
		Classification: Healthy,
		Component:      "target",
		Evidence: []string{
			fmt.Sprintf("target p95 %.2fms is within SLO %.2fms", snapshot.Target.P95Latency.Value, snapshot.SLOTargetP95Milliseconds),
		},
		Reason:     "target and observed dependencies are within configured bounds",
		Confidence: ConfidenceHigh,
	}
}

func usable(metric observation.Metric) bool {
	return metric.Valid && metric.Fresh && finiteNonnegative(metric.Value) && !metric.ObservedAt.IsZero()
}

func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func uncertain(component, reason string) Analysis {
	return Analysis{Classification: Uncertain, Component: component, Reason: reason, Confidence: ConfidenceLow, Evidence: []string{reason}}
}

func metricReason(prefix string, metrics ...observation.Metric) string {
	for _, metric := range metrics {
		if metric.Error != "" {
			return fmt.Sprintf("%s: %s", prefix, metric.Error)
		}
	}
	return prefix
}
