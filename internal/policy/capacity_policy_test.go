package policy

import (
	"testing"
	"time"

	"github.com/RajRaghupatruni/Silver-Leaf/internal/capacity"
)

func TestEvaluateCapacity(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	target := ComponentInput{Name: "target", CurrentReplicas: 2, MinReplicas: 1, MaxReplicas: 5, Scalable: true}
	dependency := ComponentInput{Name: "inventory", CurrentReplicas: 1, MinReplicas: 1, MaxReplicas: 4, Scalable: true}
	base := CapacityInput{Target: target, Dependencies: map[string]ComponentInput{"inventory": dependency}, MaxScaleUpStep: 2, MaxScaleDownStep: 1, Now: now}
	tests := []struct {
		name                     string
		analysis                 capacity.Analysis
		mutate                   func(*CapacityInput)
		wantAction               Action
		wantComponent            string
		wantCurrent, wantDesired int32
	}{
		{"target scales up", capacity.Analysis{Classification: capacity.TargetSaturated, Component: "target", Reason: "target overloaded"}, func(*CapacityInput) {}, ActionScaleTarget, "target", 2, 4},
		{"target max clamp becomes hold", capacity.Analysis{Classification: capacity.TargetSaturated, Component: "target", Reason: "target overloaded"}, func(in *CapacityInput) { in.Target.CurrentReplicas = 5 }, ActionHold, "target", 5, 5},
		{"dependency scales up", capacity.Analysis{Classification: capacity.DependencySaturated, Component: "inventory", Reason: "dependency overloaded"}, func(*CapacityInput) {}, ActionScaleDependency, "inventory", 1, 3},
		{"dependency max clamp becomes hold", capacity.Analysis{Classification: capacity.DependencySaturated, Component: "inventory", Reason: "dependency overloaded"}, func(in *CapacityInput) {
			c := in.Dependencies["inventory"]
			c.CurrentReplicas = 4
			in.Dependencies["inventory"] = c
		}, ActionHold, "inventory", 4, 4},
		{"healthy holds", capacity.Analysis{Classification: capacity.Healthy, Component: "target", Reason: "within SLO"}, func(*CapacityInput) {}, ActionHold, "target", 2, 2},
		{"uncertain protects", capacity.Analysis{Classification: capacity.Uncertain, Component: "inventory", Reason: "missing telemetry"}, func(*CapacityInput) {}, ActionProtectedMode, "target", 2, 2},
		{"capacity blocked holds non-scalable database", capacity.Analysis{Classification: capacity.CapacityBlocked, Component: "postgres", Reason: "database capacity is blocked"}, func(in *CapacityInput) {
			in.Dependencies["postgres"] = ComponentInput{Name: "postgres", CurrentReplicas: 1, MinReplicas: 1, MaxReplicas: 1, Scalable: false}
		}, ActionHold, "postgres", 1, 1},
		{"incomplete database telemetry protects without scaling", capacity.Analysis{Classification: capacity.Uncertain, Component: "postgres", Reason: "dependency \"postgres\" telemetry is incomplete"}, func(in *CapacityInput) {
			in.Dependencies["postgres"] = ComponentInput{Name: "postgres", CurrentReplicas: 1, MinReplicas: 1, MaxReplicas: 1, Scalable: false}
		}, ActionProtectedMode, "target", 2, 2},
		{"cooldown holds", capacity.Analysis{Classification: capacity.TargetSaturated, Component: "target", Reason: "overloaded"}, func(in *CapacityInput) {
			last := now.Add(-time.Second)
			in.LastScaleTime = &last
			in.Cooldown = time.Minute
		}, ActionHold, "target", 2, 2},
		{"non-scalable dependency protects", capacity.Analysis{Classification: capacity.DependencySaturated, Component: "inventory", Reason: "overloaded"}, func(in *CapacityInput) {
			c := in.Dependencies["inventory"]
			c.Scalable = false
			in.Dependencies["inventory"] = c
		}, ActionProtectedMode, "inventory", 1, 1},
		{"invalid dependency bounds preserve current", capacity.Analysis{Classification: capacity.DependencySaturated, Component: "inventory", Reason: "overloaded"}, func(in *CapacityInput) {
			c := in.Dependencies["inventory"]
			c.MinReplicas = 5
			in.Dependencies["inventory"] = c
		}, ActionProtectedMode, "inventory", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := base
			input.Dependencies = map[string]ComponentInput{"inventory": dependency}
			input.Analysis = tt.analysis
			tt.mutate(&input)
			got := EvaluateCapacity(input)
			if got.Action != tt.wantAction || got.ChosenComponent != tt.wantComponent || got.CurrentReplicas != tt.wantCurrent || got.DesiredReplicas != tt.wantDesired {
				t.Fatalf("got %+v", got)
			}
			if got.Reason == "" {
				t.Fatal("decision has no explanation")
			}
		})
	}
}
