package policy

import (
	"fmt"
	"math"
	"time"
)

type Action string

const (
	ActionScaleTarget   Action = "SCALE_TARGET"
	ActionHold          Action = "HOLD"
	ActionProtectedMode Action = "PROTECTED_MODE"
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
