package capacity

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/RajRaghupatruni/Silver-Leaf/internal/observation"
)

func TestAnalyze(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	metric := func(value float64) observation.Metric {
		return observation.Metric{Value: value, ObservedAt: now, Valid: true, Fresh: true}
	}
	base := observation.Snapshot{
		SLOTargetP95Milliseconds: 250,
		Target:                   analysisTarget("demo-api", 2, 2, metric(100), metric(1), 20, 2, metric),
		Dependencies:             []observation.DependencyObservation{{Name: "inventory", CurrentReplicas: 1, P95Latency: metric(30), Utilization: metric(1), LatencyThreshold: 250, UtilizationThreshold: 2, Scalable: true, MinReplicas: 1, MaxReplicas: 5}},
	}
	tests := []struct {
		name      string
		mutate    func(*observation.Snapshot)
		want      Classification
		component string
	}{
		{"healthy", func(*observation.Snapshot) {}, Healthy, "target"},
		{"target saturation", func(s *observation.Snapshot) {
			s.Target.P95Latency = metric(400)
			s.Target.HottestReplicaOccupancy = metric(4)
		}, TargetSaturated, "target"},
		{"dependency saturation", func(s *observation.Snapshot) {
			s.Target.P95Latency = metric(400)
			s.Dependencies[0].P95Latency = metric(500)
		}, DependencySaturated, "inventory"},
		{"missing target metric", func(s *observation.Snapshot) { s.Target.P95Latency = observation.Metric{Error: "no data"} }, Uncertain, "target"},
		{"stale dependency metric", func(s *observation.Snapshot) { s.Dependencies[0].Utilization.Fresh = false }, Uncertain, "inventory"},
		{"target local saturation takes priority", func(s *observation.Snapshot) {
			s.Target.P95Latency = metric(400)
			s.Target.HottestReplicaOccupancy = metric(4)
			s.Dependencies[0].P95Latency = metric(500)
		}, TargetSaturated, "target"},
		{"multiple dependency candidates", func(s *observation.Snapshot) {
			s.Target.P95Latency = metric(400)
			s.Dependencies[0].P95Latency = metric(500)
			s.Dependencies = append(s.Dependencies, s.Dependencies[0])
			s.Dependencies[1].Name = "database"
		}, Uncertain, "multiple dependencies"},
		{"dependency demand while SLO healthy", func(s *observation.Snapshot) { s.Dependencies[0].Utilization = metric(5) }, Healthy, "target"},
		{"invalid safe occupancy boundary", func(s *observation.Snapshot) { s.Target.SafeOperatingOccupancy = math.NaN() }, Uncertain, "target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := base
			input.Dependencies = append([]observation.DependencyObservation(nil), base.Dependencies...)
			tt.mutate(&input)
			got := Analyze(input)
			if got.Classification != tt.want || got.Component != tt.component {
				t.Fatalf("got classification=%s component=%q reason=%q", got.Classification, got.Component, got.Reason)
			}
			if got.Reason == "" || len(got.Evidence) == 0 || got.Confidence == "" {
				t.Fatalf("analysis lacks explanation: %+v", got)
			}
		})
	}
}

func TestTargetSaturationUsesHottestTrafficBearingReplica(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	metric := func(value float64) observation.Metric {
		return observation.Metric{Value: value, ObservedAt: now, Valid: true, Fresh: true}
	}
	base := observation.Snapshot{
		SLOTargetP95Milliseconds: 250,
		Target:                   analysisTarget("demo-api", 3, 3, metric(400), metric(14), 20, 15, metric),
	}
	tests := []struct {
		name              string
		readyReplicas     int32
		effectiveReplicas float64
		aggregateSlots    float64
		hottest           float64
		want              Classification
	}{
		{name: "hot replica saturated while another Ready replica is idle", readyReplicas: 3, effectiveReplicas: 2, aggregateSlots: 20, hottest: 15, want: TargetSaturated},
		{name: "same aggregate with cool hottest replica is not saturated", readyReplicas: 3, effectiveReplicas: 2, aggregateSlots: 20, hottest: 14.9, want: Uncertain},
		{name: "zero effective replicas is unsafe", readyReplicas: 3, effectiveReplicas: 0, aggregateSlots: 0, hottest: 0, want: Uncertain},
		{name: "effective replicas cannot exceed Ready replicas", readyReplicas: 2, effectiveReplicas: 3, aggregateSlots: 40, hottest: 16, want: Uncertain},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := base
			input.Target.ReadyReplicas = test.readyReplicas
			input.Target.EffectiveServingReplicas = metric(test.effectiveReplicas)
			input.Target.AggregateSlotOccupancy = metric(test.aggregateSlots)
			input.Target.HottestReplicaOccupancy = metric(test.hottest)
			got := Analyze(input)
			if got.Classification != test.want {
				t.Fatalf("classification=%s, want %s (hottest=%+v reason=%s)", got.Classification, test.want, input.Target.HottestReplicaOccupancy, got.Reason)
			}
			if test.name == "hot replica saturated while another Ready replica is idle" && !strings.Contains(strings.Join(got.Evidence, " "), "hottest traffic-bearing target replica holds 15.00") {
				t.Fatalf("saturation evidence does not identify hottest serving replica: %v", got.Evidence)
			}
			if test.name == "hot replica saturated while another Ready replica is idle" && test.aggregateSlots/float64(test.readyReplicas) >= 15 {
				t.Fatalf("fixture should have a deceptively low aggregate/Ready average; got %.2f", test.aggregateSlots/float64(test.readyReplicas))
			}
		})
	}
}

func TestRequestTelemetryDoesNotChangeCapacityClassification(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	metric := func(value float64) observation.Metric {
		return observation.Metric{Value: value, ObservedAt: now, Valid: true, Fresh: true}
	}
	rates := observation.RequestRates{RequestRate: metric(100), SuccessfulRequestRate: metric(95), ErrorRate: metric(0.05)}
	tests := []struct {
		name       string
		target     observation.TargetObservation
		dependency observation.DependencyObservation
		want       Classification
	}{
		{
			name:       "healthy despite errors",
			target:     analysisTarget("demo-api", 2, 2, metric(100), metric(1), 20, 2, metric),
			dependency: observation.DependencyObservation{Name: "inventory", P95Latency: metric(30), Utilization: metric(1), LatencyThreshold: 250, UtilizationThreshold: 2, Scalable: true, RequestRates: rates},
			want:       Healthy,
		},
		{
			name:       "target saturation remains latency and local work",
			target:     analysisTarget("demo-api", 2, 2, metric(400), metric(4), 20, 2, metric),
			dependency: observation.DependencyObservation{Name: "inventory", P95Latency: metric(30), Utilization: metric(1), LatencyThreshold: 250, UtilizationThreshold: 2, Scalable: true, RequestRates: rates},
			want:       TargetSaturated,
		},
		{
			name:       "dependency saturation remains unchanged",
			target:     analysisTarget("demo-api", 2, 2, metric(400), metric(1), 20, 2, metric),
			dependency: observation.DependencyObservation{Name: "inventory", P95Latency: metric(500), Utilization: metric(1), LatencyThreshold: 250, UtilizationThreshold: 2, Scalable: true, RequestRates: rates},
			want:       DependencySaturated,
		},
		{
			name:       "blocked dependency remains unchanged",
			target:     analysisTarget("demo-api", 2, 2, metric(400), metric(1), 20, 2, metric),
			dependency: observation.DependencyObservation{Name: "postgres", P95Latency: metric(500), Utilization: metric(10), LatencyThreshold: 250, UtilizationThreshold: 8, Scalable: false, RequestRates: rates},
			want:       CapacityBlocked,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Analyze(observation.Snapshot{SLOTargetP95Milliseconds: 250, Target: tt.target, Dependencies: []observation.DependencyObservation{tt.dependency}})
			if got.Classification != tt.want {
				t.Fatalf("classification = %s, want %s", got.Classification, tt.want)
			}
		})
	}
}

func TestNonScalableDownstreamBottleneckBlocksUpstreamScaling(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	metric := func(value float64) observation.Metric {
		return observation.Metric{Value: value, ObservedAt: now, Valid: true, Fresh: true}
	}
	got := Analyze(observation.Snapshot{
		SLOTargetP95Milliseconds: 250,
		Target: observation.TargetObservation{
			Name: "demo-api", CurrentReplicas: 2, ReadyReplicas: 2, EffectiveServingReplicas: metric(2), P95Latency: metric(487), HottestReplicaOccupancy: metric(11.2), PhysicalConcurrencyLimit: 20, SafeOperatingOccupancy: 15,
		},
		Dependencies: []observation.DependencyObservation{
			{
				Name: "inventory-service", DependsOn: "postgres", CurrentReplicas: 1,
				P95Latency: metric(1919.46), Utilization: metric(7), LatencyThreshold: 250,
				UtilizationThreshold: 100, Scalable: true, MinReplicas: 1, MaxReplicas: 5,
			},
			{
				Name: "postgres", CurrentReplicas: 1, P95Latency: metric(850), Utilization: metric(12),
				LatencyThreshold: 250, UtilizationThreshold: 8, Scalable: false, MinReplicas: 1, MaxReplicas: 1,
			},
		},
	})
	if got.Classification != CapacityBlocked || got.Component != "postgres" {
		t.Fatalf("got classification=%s component=%q reason=%q", got.Classification, got.Component, got.Reason)
	}
	if got.Confidence != ConfidenceHigh {
		t.Fatalf("confidence = %s, want HIGH", got.Confidence)
	}
	for _, want := range []string{
		`dependency "postgres" latency 850.00ms exceeds threshold 250.00ms`,
		`dependency "postgres" utilization 12.00 exceeds threshold 8.00`,
		`dependency "postgres" is configured non-scalable`,
		"hottest traffic-bearing target replica occupancy 11.20 is below safe operating boundary 15.00",
		`dependency "inventory-service" depends on "postgres"; its latency is downstream evidence`,
	} {
		if !contains(got.Evidence, want) {
			t.Errorf("evidence missing %q: %#v", want, got.Evidence)
		}
	}
	if !contains(got.RejectedActions, `SCALE_TARGET: target "demo-api" is not locally saturated`) || !contains(got.RejectedActions, `SCALE_DEPENDENCY: bottleneck dependency "postgres" is non-scalable`) {
		t.Fatalf("rejected actions do not explain both blocked choices: %#v", got.RejectedActions)
	}
	if !strings.Contains(got.Reason, `scaling upstream dependency "inventory-service" would not add database capacity`) || !strings.Contains(got.Reason, `adding target replicas would not relieve the database bottleneck`) {
		t.Fatalf("reason does not explain why upstream scaling is ineffective: %q", got.Reason)
	}
}

func TestDownstreamSaturationDoesNotMaskIndependentUpstreamUtilization(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	metric := func(value float64) observation.Metric {
		return observation.Metric{Value: value, ObservedAt: now, Valid: true, Fresh: true}
	}
	got := Analyze(observation.Snapshot{
		SLOTargetP95Milliseconds: 250,
		Target: observation.TargetObservation{
			Name: "demo-api", CurrentReplicas: 2, ReadyReplicas: 2, EffectiveServingReplicas: metric(2), P95Latency: metric(500), HottestReplicaOccupancy: metric(1), PhysicalConcurrencyLimit: 20, SafeOperatingOccupancy: 15,
		},
		Dependencies: []observation.DependencyObservation{
			{Name: "inventory-service", DependsOn: "postgres", P95Latency: metric(1900), Utilization: metric(120), LatencyThreshold: 250, UtilizationThreshold: 100, Scalable: true},
			{Name: "postgres", P95Latency: metric(850), Utilization: metric(12), LatencyThreshold: 250, UtilizationThreshold: 8, Scalable: false},
		},
	})
	if got.Classification != Uncertain {
		t.Fatalf("classification = %s, want conservative UNCERTAIN when upstream utilization is independently saturated", got.Classification)
	}
}

func TestIncompleteDatabaseTelemetryIsUncertain(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	metric := func(value float64) observation.Metric {
		return observation.Metric{Value: value, ObservedAt: now, Valid: true, Fresh: true}
	}
	got := Analyze(observation.Snapshot{
		SLOTargetP95Milliseconds: 250,
		Target:                   observation.TargetObservation{Name: "demo-api", CurrentReplicas: 2, ReadyReplicas: 2, EffectiveServingReplicas: metric(2), P95Latency: metric(500), HottestReplicaOccupancy: metric(1), PhysicalConcurrencyLimit: 20, SafeOperatingOccupancy: 15},
		Dependencies: []observation.DependencyObservation{{
			Name: "postgres", P95Latency: metric(850), Utilization: observation.Metric{Error: "no database utilization sample"},
			LatencyThreshold: 250, UtilizationThreshold: 8, Scalable: false,
		}},
	})
	if got.Classification != Uncertain || got.Component != "postgres" {
		t.Fatalf("got classification=%s component=%q", got.Classification, got.Component)
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func analysisTarget(name string, requested, ready int32, p95, hottest observation.Metric, physicalLimit, safeBoundary float64, metric func(float64) observation.Metric) observation.TargetObservation {
	return observation.TargetObservation{
		Name: name, CurrentReplicas: requested, ReadyReplicas: ready,
		P95Latency: p95, HottestReplicaOccupancy: hottest,
		EffectiveServingReplicas: metric(float64(ready)),
		PhysicalConcurrencyLimit: physicalLimit, SafeOperatingOccupancy: safeBoundary,
	}
}

func TestDependencySaturationEvidenceNamesFiredThresholds(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	metric := func(value float64) observation.Metric {
		return observation.Metric{Value: value, ObservedAt: now, Valid: true, Fresh: true}
	}
	tests := []struct {
		name    string
		latency float64
		util    float64
		want    []string
	}{
		{
			name:    "latency only",
			latency: 1919.46,
			util:    7,
			want:    []string{`dependency "inventory-service" latency 1919.46ms exceeds threshold 250.00ms`},
		},
		{
			name:    "utilization only",
			latency: 120,
			util:    120,
			want:    []string{`dependency "inventory-service" utilization 120.00 exceeds threshold 100.00`},
		},
		{
			name:    "both signals",
			latency: 1919.46,
			util:    120,
			want: []string{
				`dependency "inventory-service" latency 1919.46ms exceeds threshold 250.00ms`,
				`dependency "inventory-service" utilization 120.00 exceeds threshold 100.00`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Analyze(observation.Snapshot{
				SLOTargetP95Milliseconds: 250,
				Target:                   analysisTarget("demo-api", 2, 2, metric(400), metric(1), 20, 2, metric),
				Dependencies: []observation.DependencyObservation{{
					Name: "inventory-service", P95Latency: metric(tt.latency), Utilization: metric(tt.util),
					LatencyThreshold: 250, UtilizationThreshold: 100, Scalable: true,
				}},
			})
			if got.Classification != DependencySaturated {
				t.Fatalf("classification = %s, want %s", got.Classification, DependencySaturated)
			}
			if len(got.Evidence) != len(tt.want) {
				t.Fatalf("evidence = %#v, want %#v", got.Evidence, tt.want)
			}
			for i := range tt.want {
				if got.Evidence[i] != tt.want[i] {
					t.Fatalf("evidence[%d] = %q, want %q", i, got.Evidence[i], tt.want[i])
				}
			}
			wantReason := strings.Join(tt.want, "; ")
			if got.Reason != wantReason {
				t.Fatalf("reason = %q, want %q", got.Reason, wantReason)
			}
		})
	}
}
