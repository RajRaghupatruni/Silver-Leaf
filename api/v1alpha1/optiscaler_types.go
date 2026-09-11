package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Action describes the controller's explainable decision.
type Action string

const (
	ActionScaleTarget   Action = "SCALE_TARGET"
	ActionHold          Action = "HOLD"
	ActionProtectedMode Action = "PROTECTED_MODE"
)

// ControlMode describes the safety mode used for the most recent evaluation.
type ControlMode string

const (
	ControlModeAutomatic ControlMode = "AUTOMATIC"
	ControlModeHold      ControlMode = "HOLD"
	ControlModeProtected ControlMode = "PROTECTED_MODE"
)

// OptiScalerSpec defines the desired capacity policy.
type OptiScalerSpec struct {
	// ScaleTargetRef identifies the namespaced workload to scale. Vertical Slice 1 supports Deployment only.
	// +kubebuilder:validation:Required
	ScaleTargetRef ScaleTargetReference `json:"scaleTargetRef"`

	// +kubebuilder:validation:Minimum=1
	MinReplicas int32 `json:"minReplicas"`

	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	Metric MetricSpec `json:"metric"`
	SLO    SLOSpec    `json:"slo"`
	Policy PolicySpec `json:"policy"`

	// Prometheus configures the Prometheus API used for observations.
	Prometheus PrometheusSpec `json:"prometheus"`
}

// ScaleTargetReference identifies a supported Kubernetes workload.
type ScaleTargetReference struct {
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type MetricSpec struct {
	// PrometheusQuery must return exactly one numeric instant-query result.
	// +kubebuilder:validation:MinLength=1
	PrometheusQuery string `json:"prometheusQuery"`
}

type SLOSpec struct {
	// TargetP95Milliseconds documents the SLO represented by the metric query.
	// +kubebuilder:validation:Minimum=0
	TargetP95Milliseconds float64 `json:"targetP95Milliseconds"`
}

type PolicySpec struct {
	// +kubebuilder:validation:Minimum=0
	ScaleUpThreshold float64 `json:"scaleUpThreshold"`
	// +kubebuilder:validation:Minimum=0
	ScaleDownThreshold float64 `json:"scaleDownThreshold"`
	// +kubebuilder:validation:Minimum=1
	MaxScaleUpStep int32 `json:"maxScaleUpStep"`
	// +kubebuilder:validation:Minimum=1
	MaxScaleDownStep int32 `json:"maxScaleDownStep"`
	// +kubebuilder:validation:Minimum=0
	CooldownSeconds int32 `json:"cooldownSeconds"`
}

type PrometheusSpec struct {
	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`
}

// DecisionRecord explains the latest policy evaluation.
type DecisionRecord struct {
	ID              string      `json:"id"`
	Timestamp       metav1.Time `json:"timestamp"`
	Action          Action      `json:"action"`
	Reason          string      `json:"reason"`
	ObservedMetric  *float64    `json:"observedMetric,omitempty"`
	Threshold       *float64    `json:"threshold,omitempty"`
	CurrentReplicas int32       `json:"currentReplicas"`
	DesiredReplicas int32       `json:"desiredReplicas"`
}

// OptiScalerStatus defines the observed state of an OptiScaler.
type OptiScalerStatus struct {
	CurrentReplicas int32              `json:"currentReplicas"`
	DesiredReplicas int32              `json:"desiredReplicas"`
	ObservedMetric  *float64           `json:"observedMetric,omitempty"`
	ControlMode     ControlMode        `json:"controlMode,omitempty"`
	LastScaleTime   *metav1.Time       `json:"lastScaleTime,omitempty"`
	LastDecision    *DecisionRecord    `json:"lastDecision,omitempty"`
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=osc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".spec.scaleTargetRef.name"
// +kubebuilder:printcolumn:name="Current",type="integer",JSONPath=".status.currentReplicas"
// +kubebuilder:printcolumn:name="Desired",type="integer",JSONPath=".status.desiredReplicas"
// +kubebuilder:printcolumn:name="Mode",type="string",JSONPath=".status.controlMode"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type OptiScaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              OptiScalerSpec   `json:"spec,omitempty"`
	Status            OptiScalerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type OptiScalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OptiScaler `json:"items"`
}
