package policy

import (
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	observed := now.Add(-5 * time.Second)
	base := Input{CurrentReplicas: 3, MinReplicas: 1, MaxReplicas: 5, TelemetryValid: true, TelemetryObservedAt: observed, MaxTelemetryAge: time.Minute, Now: now, ScaleUpThreshold: 250, ScaleDownThreshold: 150, MaxScaleUpStep: 2, MaxScaleDownStep: 1}
	tests := []struct {
		name    string
		input   Input
		action  Action
		desired int32
		reason  string
	}{
		{"scale up by bounded step", func() Input { x := base; x.ObservedMetric = 300; return x }(), ActionScaleTarget, 5, "scale-up threshold exceeded"},
		{"scale up clamps at max", func() Input { x := base; x.CurrentReplicas = 4; x.ObservedMetric = 300; return x }(), ActionScaleTarget, 5, "scale-up threshold exceeded"},
		{"scale down by bounded step", func() Input { x := base; x.ObservedMetric = 100; return x }(), ActionScaleTarget, 2, "scale-down threshold crossed"},
		{"scale down clamps at min", func() Input { x := base; x.CurrentReplicas = 1; x.ObservedMetric = 100; return x }(), ActionHold, 1, "scale-down threshold crossed; minimum replicas already reached"},
		{"scale up holds at max", func() Input { x := base; x.CurrentReplicas = 5; x.ObservedMetric = 300; return x }(), ActionHold, 5, "scale-up threshold exceeded; maximum replicas already reached"},
		{"equal upper boundary holds", func() Input { x := base; x.ObservedMetric = 250; return x }(), ActionHold, 3, "metric within policy band"},
		{"equal lower boundary holds", func() Input { x := base; x.ObservedMetric = 150; return x }(), ActionHold, 3, "metric within policy band"},
		{"cooldown holds", func() Input {
			x := base
			x.ObservedMetric = 300
			last := now.Add(-10 * time.Second)
			x.LastScaleTime = &last
			x.Cooldown = time.Minute
			return x
		}(), ActionHold, 3, "cooldown active"},
		{"invalid telemetry protects", func() Input { x := base; x.TelemetryValid = false; x.ObservedMetric = 0; return x }(), ActionProtectedMode, 3, "telemetry invalid or unavailable"},
		{"stale telemetry protects", func() Input {
			x := base
			x.TelemetryObservedAt = now.Add(-2 * time.Minute)
			x.ObservedMetric = 0
			return x
		}(), ActionProtectedMode, 3, "telemetry is stale"},
		{"invalid bounds protect", func() Input { x := base; x.MaxReplicas = 0; x.ObservedMetric = 300; return x }(), ActionProtectedMode, 3, "invalid replica bounds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.input)
			if got.Action != tt.action || got.DesiredReplicas != tt.desired || got.Reason != tt.reason {
				t.Fatalf("got action=%s desired=%d reason=%q", got.Action, got.DesiredReplicas, got.Reason)
			}
		})
	}
}
