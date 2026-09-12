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
		Target:                   observation.TargetObservation{Name: "demo-api", CurrentReplicas: 2, P95Latency: metric(100), Utilization: metric(1), UtilizationThreshold: 2},
		Dependencies:             []observation.DependencyObservation{{Name: "inventory", CurrentReplicas: 1, P95Latency: metric(30), Utilization: metric(1), LatencyThreshold: 250, UtilizationThreshold: 2, Scalable: true, MinReplicas: 1, MaxReplicas: 5}},
	}
	tests := []struct {
		name      string
		mutate    func(*observation.Snapshot)
		want      Classification
		component string
	}{
		{"healthy", func(*observation.Snapshot) {}, Healthy, "target"},
		{"target saturation", func(s *observation.Snapshot) { s.Target.P95Latency = metric(400); s.Target.Utilization = metric(4) }, TargetSaturated, "target"},
		{"dependency saturation", func(s *observation.Snapshot) {
			s.Target.P95Latency = metric(400)
			s.Dependencies[0].P95Latency = metric(500)
		}, DependencySaturated, "inventory"},
		{"missing target metric", func(s *observation.Snapshot) { s.Target.P95Latency = observation.Metric{Error: "no data"} }, Uncertain, "target"},
		{"stale dependency metric", func(s *observation.Snapshot) { s.Dependencies[0].Utilization.Fresh = false }, Uncertain, "inventory"},
		{"target local saturation takes priority", func(s *observation.Snapshot) {
			s.Target.P95Latency = metric(400)
			s.Target.Utilization = metric(4)
			s.Dependencies[0].P95Latency = metric(500)
		}, TargetSaturated, "target"},
		{"multiple dependency candidates", func(s *observation.Snapshot) {
			s.Target.P95Latency = metric(400)
			s.Dependencies[0].P95Latency = metric(500)
			s.Dependencies = append(s.Dependencies, s.Dependencies[0])
			s.Dependencies[1].Name = "database"
		}, Uncertain, "multiple dependencies"},
		{"dependency demand while SLO healthy", func(s *observation.Snapshot) { s.Dependencies[0].Utilization = metric(5) }, Healthy, "target"},
		{"invalid utilization threshold", func(s *observation.Snapshot) { s.Target.UtilizationThreshold = math.NaN() }, Uncertain, "target"},
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

func TestNonScalableDownstreamBottleneckBlocksUpstreamScaling(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	metric := func(value float64) observation.Metric {
		return observation.Metric{Value: value, ObservedAt: now, Valid: true, Fresh: true}
	}
	got := Analyze(observation.Snapshot{
		SLOTargetP95Milliseconds: 250,
		Target: observation.TargetObservation{
			Name: "demo-api", CurrentReplicas: 2, P95Latency: metric(487), Utilization: metric(11.2), UtilizationThreshold: 30,
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
		"target utilization 11.20 is at or below threshold 30.00",
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
			Name: "demo-api", P95Latency: metric(500), Utilization: metric(1), UtilizationThreshold: 30,
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
		Target:                   observation.TargetObservation{Name: "demo-api", P95Latency: metric(500), Utilization: metric(1), UtilizationThreshold: 30},
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
				Target: observation.TargetObservation{
					P95Latency: metric(400), Utilization: metric(1), UtilizationThreshold: 2,
				},
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
