package policy

import (
	"fmt"
	"math"
	"time"

	"github.com/RajRaghupatruni/Silver-Leaf/internal/capacity"
)

type Action string

const (
	ActionScaleTarget     Action = "SCALE_TARGET"
	ActionScaleDependency Action = "SCALE_DEPENDENCY"
	ActionHold            Action = "HOLD"
	ActionProtectedMode   Action = "PROTECTED_MODE"
)

type Input struct {
	CurrentReplicas     int32
	MinReplicas         int32
	MaxReplicas         int32
	ObservedMetric      float64
	TelemetryValid      bool
	TelemetryObservedAt time.Time
	MaxTelemetryAge     time.Duration
	Now                 time.Time
	ScaleUpThreshold    float64
	ScaleDownThreshold  float64
	MaxScaleUpStep      int32
	MaxScaleDownStep    int32
	Cooldown            time.Duration
	LastScaleTime       *time.Time
}

// ComponentInput contains the independent safety envelope for one scalable workload.
type ComponentInput struct {
	Name            string
	CurrentReplicas int32
	MinReplicas     int32
	MaxReplicas     int32
	Scalable        bool
}

type CapacityInput struct {
	Analysis         capacity.Analysis
	Target           ComponentInput
	Dependencies     map[string]ComponentInput
	MaxScaleUpStep   int32
	MaxScaleDownStep int32
	Cooldown         time.Duration
	LastScaleTime    *time.Time
	Now              time.Time
}

type CapacityDecision struct {
	Action          Action
	Reason          string
	ChosenComponent string
	CurrentReplicas int32
	DesiredReplicas int32
	Threshold       float64
	HasThreshold    bool
}

// EvaluateCapacity maps an explainable bottleneck classification to one bounded mutation.
// It never derives a replica value from invalid bounds and treats uncertain evidence as protected.
func EvaluateCapacity(in CapacityInput) CapacityDecision {
	decision := CapacityDecision{Action: ActionProtectedMode, ChosenComponent: in.Target.Name, CurrentReplicas: in.Target.CurrentReplicas, DesiredReplicas: in.Target.CurrentReplicas}
	if in.Analysis.Classification == capacity.Uncertain {
		decision.Reason = in.Analysis.Reason
		return decision
	}
	if err := validateComponent(in.Target, in.MaxScaleUpStep, in.MaxScaleDownStep); err != nil {
		decision.Reason = "invalid target configuration: " + err.Error()
		return decision
	}
	if in.Analysis.Classification == capacity.CapacityBlocked {
		candidate, ok := in.Dependencies[in.Analysis.Component]
		if !ok || candidate.Scalable {
			decision.Reason = "capacity-blocked analysis does not identify a configured non-scalable dependency"
			return decision
		}
		if err := validateComponent(candidate, in.MaxScaleUpStep, in.MaxScaleDownStep); err != nil {
			decision.Reason = "invalid blocked dependency configuration: " + err.Error()
			return decision
		}
		decision.Action = ActionHold
		decision.ChosenComponent = candidate.Name
		decision.CurrentReplicas = candidate.CurrentReplicas
		decision.DesiredReplicas = candidate.CurrentReplicas
		decision.Reason = in.Analysis.Reason
		return decision
	}
	if in.LastScaleTime != nil && in.Cooldown > 0 && in.Now.Before(in.LastScaleTime.Add(in.Cooldown)) {
		decision.Action = ActionHold
		decision.Reason = "cooldown active"
		return decision
	}
	if in.Analysis.Classification == capacity.Healthy {
		decision.Action = ActionHold
		decision.Reason = in.Analysis.Reason
		return decision
	}

	component := in.Target
	if in.Analysis.Classification == capacity.DependencySaturated {
		candidate, ok := in.Dependencies[in.Analysis.Component]
		if !ok {
			decision.Reason = fmt.Sprintf("dependency %q is not configured", in.Analysis.Component)
			return decision
		}
		if !candidate.Scalable {
			decision.ChosenComponent = candidate.Name
			decision.CurrentReplicas = candidate.CurrentReplicas
			decision.DesiredReplicas = candidate.CurrentReplicas
			decision.Reason = fmt.Sprintf("dependency %q is saturated but not scalable", candidate.Name)
			return decision
		}
		if err := validateComponent(candidate, in.MaxScaleUpStep, in.MaxScaleDownStep); err != nil {
			decision.ChosenComponent = candidate.Name
			decision.CurrentReplicas = candidate.CurrentReplicas
			decision.DesiredReplicas = candidate.CurrentReplicas
			decision.Reason = "invalid dependency configuration: " + err.Error()
			return decision
		}
		component = candidate
	}
	decision.ChosenComponent = component.Name
	decision.CurrentReplicas = component.CurrentReplicas
	decision.DesiredReplicas = component.CurrentReplicas
	if in.Analysis.Classification != capacity.TargetSaturated && in.Analysis.Classification != capacity.DependencySaturated {
		decision.Action = ActionProtectedMode
		decision.Reason = "unsupported capacity classification"
		return decision
	}
	desired := component.CurrentReplicas + in.MaxScaleUpStep
	if desired > component.MaxReplicas {
		desired = component.MaxReplicas
	}
	if desired == component.CurrentReplicas {
		decision.Action = ActionHold
		decision.Reason = fmt.Sprintf("%s saturation signal present; maximum replicas already reached", component.Name)
		return decision
	}
	decision.Action = ActionScaleTarget
	if component.Name != in.Target.Name {
		decision.Action = ActionScaleDependency
	}
	decision.DesiredReplicas = desired
	decision.Reason = fmt.Sprintf("%s saturation signal present; scaling by bounded step", component.Name)
	return decision
}

func validateComponent(component ComponentInput, up, down int32) error {
	if component.MinReplicas < 1 || component.MaxReplicas < component.MinReplicas || component.CurrentReplicas < component.MinReplicas || component.CurrentReplicas > component.MaxReplicas {
		return fmt.Errorf("invalid replica bounds")
	}
	if up < 1 || down < 1 {
		return fmt.Errorf("scale steps must be at least 1")
	}
	return nil
}

type Decision struct {
	Action            Action
	Reason            string
	ObservedMetric    float64
	HasObservedMetric bool
	Threshold         float64
	HasThreshold      bool
	CurrentReplicas   int32
	DesiredReplicas   int32
}

func Evaluate(in Input) Decision {
	decision := Decision{CurrentReplicas: in.CurrentReplicas, DesiredReplicas: in.CurrentReplicas}
	if err := validate(in); err != nil {
		return protectedDecision(in.CurrentReplicas, err.Error())
	}
	decision.ObservedMetric = in.ObservedMetric
	decision.HasObservedMetric = true
	if in.LastScaleTime != nil && in.Cooldown > 0 && in.Now.Before(in.LastScaleTime.Add(in.Cooldown)) {
		decision.Action = ActionHold
		decision.Reason = "cooldown active"
		return decision
	}
	if in.ObservedMetric > in.ScaleUpThreshold {
		decision.Action = ActionScaleTarget
		decision.Threshold = in.ScaleUpThreshold
		decision.HasThreshold = true
		desired := in.CurrentReplicas + in.MaxScaleUpStep
		if desired > in.MaxReplicas {
			desired = in.MaxReplicas
		}
		if desired == in.CurrentReplicas {
			decision.Action = ActionHold
			decision.Reason = "scale-up threshold exceeded; maximum replicas already reached"
		} else {
			decision.Reason = "scale-up threshold exceeded"
		}
		decision.DesiredReplicas = desired
		return decision
	}
	if in.ObservedMetric < in.ScaleDownThreshold {
		decision.Action = ActionScaleTarget
		decision.Threshold = in.ScaleDownThreshold
		decision.HasThreshold = true
		desired := in.CurrentReplicas - in.MaxScaleDownStep
		if desired < in.MinReplicas {
			desired = in.MinReplicas
		}
		if desired == in.CurrentReplicas {
			decision.Action = ActionHold
			decision.Reason = "scale-down threshold crossed; minimum replicas already reached"
		} else {
			decision.Reason = "scale-down threshold crossed"
		}
		decision.DesiredReplicas = desired
		return decision
	}
	decision.Action = ActionHold
	decision.Reason = "metric within policy band"
	return decision
}

func protectedDecision(currentReplicas int32, reason string) Decision {
	return Decision{Action: ActionProtectedMode, Reason: reason, CurrentReplicas: currentReplicas, DesiredReplicas: currentReplicas}
}

func validate(in Input) error {
	if in.MinReplicas < 1 || in.MaxReplicas < in.MinReplicas || in.CurrentReplicas < in.MinReplicas || in.CurrentReplicas > in.MaxReplicas {
		return fmt.Errorf("invalid replica bounds")
	}
	if math.IsNaN(in.ScaleDownThreshold) || math.IsInf(in.ScaleDownThreshold, 0) || math.IsNaN(in.ScaleUpThreshold) || math.IsInf(in.ScaleUpThreshold, 0) || in.ScaleDownThreshold < 0 || in.ScaleUpThreshold < 0 {
		return fmt.Errorf("policy thresholds must be finite and nonnegative")
	}
	if in.ScaleDownThreshold > in.ScaleUpThreshold {
		return fmt.Errorf("scaleDownThreshold must not exceed scaleUpThreshold")
	}
	if in.MaxScaleUpStep < 1 || in.MaxScaleDownStep < 1 {
		return fmt.Errorf("scale steps must be at least 1")
	}
	if !in.TelemetryValid {
		return fmt.Errorf("telemetry invalid or unavailable")
	}
	if math.IsNaN(in.ObservedMetric) || math.IsInf(in.ObservedMetric, 0) {
		return fmt.Errorf("telemetry value is not finite")
	}
	if in.TelemetryObservedAt.IsZero() {
		return fmt.Errorf("telemetry timestamp missing")
	}
	if in.Now.IsZero() {
		return fmt.Errorf("policy evaluation time missing")
	}
	if in.MaxTelemetryAge > 0 && in.Now.Sub(in.TelemetryObservedAt) > in.MaxTelemetryAge {
		return fmt.Errorf("telemetry is stale")
	}
	if in.TelemetryObservedAt.After(in.Now.Add(30 * time.Second)) {
		return fmt.Errorf("telemetry timestamp is in the future")
	}
	return nil
}
