package policy

import (
	"math"
	"testing"
	"time"

	"github.com/RajRaghupatruni/Silver-Leaf/internal/capacity"
)

func TestEvaluatePrescale(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		mutate     func(*PrescaleInput)
		wantAction Action
		want       int32
		wantReason string
	}{
		{name: "accepted rising demand", mutate: func(*PrescaleInput) {}, wantAction: ActionPrescaleTarget, want: 3, wantReason: "planning horizon"},
		{name: "insufficient telemetry", mutate: func(in *PrescaleInput) {
			in.RequestTelemetryValid = false
			in.RejectionReason = "request-rate history is incomplete"
		}, wantAction: ActionHold, want: 2, wantReason: "incomplete"},
		{name: "missing fast predictive demand", mutate: func(in *PrescaleInput) {
			in.PredictiveDemandValid = false
			in.RejectionReason = "predictive-demand telemetry is stale"
		}, wantAction: ActionHold, want: 2, wantReason: "predictive-demand telemetry"},
		{name: "unhealthy errors", mutate: func(in *PrescaleInput) {
			in.ErrorRateHealthy = false
			in.RejectionReason = "error ratio exceeds learning limit"
		}, wantAction: ActionHold, want: 2, wantReason: "error ratio"},
		{name: "dependency evidence wins despite healthy classification", mutate: func(in *PrescaleInput) {
			in.DependenciesHealthy = false
			in.RejectionReason = "inventory dependency is saturated"
		}, wantAction: ActionHold, want: 2, wantReason: "inventory"},
		{name: "local saturation blocks prescaling", mutate: func(in *PrescaleInput) { in.TargetUnsaturated = false }, wantAction: ActionHold, want: 2, wantReason: "local utilization"},
		{name: "missing capacity evidence", mutate: func(in *PrescaleInput) {
			in.CapacityEvidenceValid = false
			in.RejectionReason = "safe capacity evidence missing"
		}, wantAction: ActionHold, want: 2, wantReason: "safe capacity"},
		{name: "poor forecast quality", mutate: func(in *PrescaleInput) { in.ForecastQuality = "MEDIUM" }, wantAction: ActionHold, want: 2, wantReason: "not HIGH"},
		{name: "missing planning horizon", mutate: func(in *PrescaleInput) { in.Horizon = 0 }, wantAction: ActionHold, want: 2, wantReason: "planning horizon"},
		{name: "non-finite forecast", mutate: func(in *PrescaleInput) { in.ForecastRequestRate = math.NaN() }, wantAction: ActionHold, want: 2, wantReason: "invalid"},
		{name: "zero safe capacity evidence", mutate: func(in *PrescaleInput) { in.SafeCapacity = 0 }, wantAction: ActionHold, want: 2, wantReason: "invalid"},
		{name: "capacity not exceeded", mutate: func(in *PrescaleInput) { in.ForecastRequestRate = 60 }, wantAction: ActionHold, want: 2, wantReason: "does not exceed"},
		{name: "max replicas", mutate: func(in *PrescaleInput) {
			in.Target.CurrentReplicas = 5
			in.Current.CurrentReplicas = 5
			in.Current.DesiredReplicas = 5
		}, wantAction: ActionHold, want: 5, wantReason: "maximum replicas"},
		{name: "cooldown", mutate: func(in *PrescaleInput) {
			last := now.Add(-time.Second)
			in.LastScaleTime = &last
			in.Cooldown = time.Minute
		}, wantAction: ActionHold, want: 2, wantReason: "cooldown"},
		{name: "invalid bounds", mutate: func(in *PrescaleInput) { in.Target.MinReplicas = 4 }, wantAction: ActionHold, want: 2, wantReason: "invalid target"},
		{name: "stale or unhealthy dependencies", mutate: func(in *PrescaleInput) {
			in.Classification = capacity.DependencySaturated
			in.Current.Action = ActionScaleDependency
			in.Current.Reason = "scale dependency"
		}, wantAction: ActionScaleDependency, want: 2, wantReason: "dependency"},
		{name: "target reactive scaling wins", mutate: func(in *PrescaleInput) {
			in.Classification = capacity.TargetSaturated
			in.Current.Action = ActionScaleTarget
			in.Current.DesiredReplicas = 3
			in.Current.Reason = "reactive scale"
		}, wantAction: ActionScaleTarget, want: 3, wantReason: "reactive"},
		{name: "blocked capacity holds", mutate: func(in *PrescaleInput) {
			in.Classification = capacity.CapacityBlocked
			in.Current.Reason = "non-scalable postgres"
		}, wantAction: ActionHold, want: 2, wantReason: "postgres"},
		{name: "protected mode wins", mutate: func(in *PrescaleInput) {
			in.Classification = capacity.Uncertain
			in.Current.Action = ActionProtectedMode
			in.Current.Reason = "protected by invalid telemetry"
		}, wantAction: ActionProtectedMode, want: 2, wantReason: "protected"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := PrescaleInput{
				Current:        CapacityDecision{Action: ActionHold, Reason: "target currently healthy", ChosenComponent: "target", CurrentReplicas: 2, DesiredReplicas: 2},
				Classification: capacity.Healthy, RequestTelemetryValid: true, PredictiveDemandValid: true, ErrorRateHealthy: true, DependenciesHealthy: true,
				TargetUnsaturated: true, CapacityEvidenceValid: true, ForecastQuality: "HIGH", ForecastRequestRate: 80,
				RequestRateSlope: 1, SafeCapacity: 60, Horizon: 12 * time.Second,
				Target:         ComponentInput{Name: "target", CurrentReplicas: 2, MinReplicas: 1, MaxReplicas: 5, Scalable: true},
				MaxScaleUpStep: 1, Now: now,
			}
			test.mutate(&input)
			got := EvaluatePrescale(input)
			if got.Action != test.wantAction || got.DesiredReplicas != test.want {
				t.Fatalf("got %+v", got)
			}
			if !containsReason(got.Reason, test.wantReason) {
				t.Fatalf("reason %q does not contain %q", got.Reason, test.wantReason)
			}
		})
	}
}

func containsReason(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
