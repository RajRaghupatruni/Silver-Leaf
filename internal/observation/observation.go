package observation

import "time"

// Metric is the controller's typed view of one Prometheus result. Raw API
// responses remain inside the Prometheus client and never enter policy code.
type Metric struct {
	Value      float64
	ObservedAt time.Time
	Valid      bool
	Fresh      bool
	Error      string
}

type TargetObservation struct {
	CurrentReplicas      int32
	P95Latency           Metric
	Utilization          Metric
	UtilizationThreshold float64
}

type DependencyObservation struct {
	Name                 string
	CurrentReplicas      int32
	P95Latency           Metric
	Utilization          Metric
	LatencyThreshold     float64
	UtilizationThreshold float64
	Scalable             bool
	MinReplicas          int32
	MaxReplicas          int32
}

type Snapshot struct {
	SLOTargetP95Milliseconds float64
	Target                   TargetObservation
	Dependencies             []DependencyObservation
}
