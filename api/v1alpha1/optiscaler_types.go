package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Action describes the controller's explainable decision.
type Action string

const (
	ActionScaleTarget     Action = "SCALE_TARGET"
	ActionScaleDependency Action = "SCALE_DEPENDENCY"
	ActionPrescaleTarget  Action = "PRESCALE_TARGET"
	ActionHold            Action = "HOLD"
	ActionProtectedMode   Action = "PROTECTED_MODE"
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

	// Dependencies are optional managed Deployment bottlenecks in the same namespace.
	Dependencies []DependencySpec `json:"dependencies,omitempty"`

	// Prometheus configures the Prometheus API used for observations.
	Prometheus PrometheusSpec `json:"prometheus"`

	// Prediction enables conservative, explainable target prescaling from observed request-rate history.
	Prediction PredictionSpec `json:"prediction,omitempty"`
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
	// UtilizationQuery supplies aggregate target concurrency-slot occupancy (slot-seconds/second).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	UtilizationQuery string `json:"utilizationQuery,omitempty"`
	// HottestReplicaUtilizationQuery returns the highest per-instance concurrency-slot occupancy.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	HottestReplicaUtilizationQuery string `json:"hottestReplicaUtilizationQuery"`
	// ServingReplicaCountQuery returns the number of instances with meaningful recent request traffic.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ServingReplicaCountQuery string `json:"servingReplicaCountQuery"`
	// PhysicalConcurrencyLimit is the actual semaphore slot limit per target pod; the configured
	// prediction safety margin derives the safe operating occupancy from this physical ceiling.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	PhysicalConcurrencyLimit float64 `json:"physicalConcurrencyLimit"`
	// RequestRateQuery returns total requests per second from a monotonically increasing counter.
	RequestRateQuery string `json:"requestRateQuery,omitempty"`
	// ErrorRequestRateQuery returns failed requests per second from a monotonically increasing counter.
	ErrorRequestRateQuery string `json:"errorRequestRateQuery,omitempty"`
	// PredictiveRequestRateQuery returns the target's faster demand-rate estimate used only for trend forecasting.
	// +kubebuilder:validation:MinLength=1
	PredictiveRequestRateQuery string `json:"predictiveRequestRateQuery,omitempty"`
}

type DependencySpec struct {
	// Name is the explainable dependency identifier.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// DependsOn identifies a configured downstream component that can explain this component's latency.
	DependsOn      string                   `json:"dependsOn,omitempty"`
	ScaleTargetRef ScaleTargetReference     `json:"scaleTargetRef"`
	Metrics        DependencyMetricsSpec    `json:"metrics"`
	Thresholds     DependencyThresholdsSpec `json:"thresholds"`
	Scalable       bool                     `json:"scalable"`
	// Dependency-specific safety bounds. The controller supports Deployment targets only.
	// +kubebuilder:validation:Minimum=1
	MinReplicas int32 `json:"minReplicas"`
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`
}

type DependencyMetricsSpec struct {
	// +kubebuilder:validation:MinLength=1
	LatencyQuery string `json:"latencyQuery"`
	// +kubebuilder:validation:MinLength=1
	UtilizationQuery string `json:"utilizationQuery"`
	// RequestRateQuery returns total requests or database queries per second.
	RequestRateQuery string `json:"requestRateQuery,omitempty"`
	// ErrorRequestRateQuery returns failed requests or database queries per second.
	ErrorRequestRateQuery string `json:"errorRequestRateQuery,omitempty"`
}

type DependencyThresholdsSpec struct {
	// +kubebuilder:validation:Minimum=0
	LatencyMilliseconds float64 `json:"latencyMilliseconds"`
	// +kubebuilder:validation:Minimum=0
	Utilization float64 `json:"utilization"`
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

// PredictionSpec configures the optional P0 predictive capacity advisory.
type PredictionSpec struct {
	Enabled bool `json:"enabled,omitempty"`
	// SafeCapacityMargin is a fraction in (0,1] applied to empirically observed per-replica throughput.
	// +kubebuilder:validation:Minimum=0.000001
	// +kubebuilder:validation:Maximum=1
	SafeCapacityMargin float64 `json:"safeCapacityMargin,omitempty"`
	// MaxErrorRate is the largest observed error ratio accepted for healthy-capacity learning.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	MaxErrorRate float64 `json:"maxErrorRate,omitempty"`
	// PredictiveDemandWindowSeconds is the rate() window used by PredictiveRequestRateQuery.
	// The forecast model derives observation lag as half this configured window.
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=120
	PredictiveDemandWindowSeconds int32 `json:"predictiveDemandWindowSeconds,omitempty"`
}

// DecisionRecord explains the latest policy evaluation.
type DecisionRecord struct {
	ID                            string      `json:"id"`
	Timestamp                     metav1.Time `json:"timestamp"`
	Action                        Action      `json:"action"`
	Reason                        string      `json:"reason"`
	ObservedMetric                *float64    `json:"observedMetric,omitempty"`
	Threshold                     *float64    `json:"threshold,omitempty"`
	CurrentReplicas               int32       `json:"currentReplicas"`
	DesiredReplicas               int32       `json:"desiredReplicas"`
	SLOTargetP95Milliseconds      float64     `json:"sloTargetP95Milliseconds"`
	ObservedTargetP95Milliseconds *float64    `json:"observedTargetP95Milliseconds,omitempty"`
	// TargetRequestRate is total requests per second; absent means unavailable.
	TargetRequestRate *float64 `json:"targetRequestRate,omitempty"`
	// TargetSuccessfulRequestRate is successful requests per second, derived from total minus errors.
	TargetSuccessfulRequestRate *float64 `json:"targetSuccessfulRequestRate,omitempty"`
	// TargetErrorRate is error RPS / total RPS, a ratio in [0,1]; absent when undefined or unavailable.
	TargetErrorRate *float64 `json:"targetErrorRate,omitempty"`
	// PredictiveRequestRate is the current target rate observed from the separately configured fast demand query.
	PredictiveRequestRate         *float64                `json:"predictiveRequestRate,omitempty"`
	PredictiveDemandWindowSeconds *float64                `json:"predictiveDemandWindowSeconds,omitempty"`
	DemandObservationLagSeconds   *float64                `json:"demandObservationLagSeconds,omitempty"`
	DependencyRequestRates        []DependencyRequestRate `json:"dependencyRequestRates,omitempty"`
	ForecastRequestRate           *float64                `json:"forecastRequestRate,omitempty"`
	// ForecastHorizonSeconds is measured readiness + control-loop allowance + predictive-demand observation lag.
	ForecastHorizonSeconds *float64 `json:"forecastHorizonSeconds,omitempty"`
	RequestRateSlope       *float64 `json:"requestRateSlope,omitempty"`
	ForecastConfidence     string   `json:"forecastConfidence,omitempty"`
	ForecastFitR2          *float64 `json:"forecastFitR2,omitempty"`
	// SafePerReplicaCapacity is safe throughput per traffic-bearing replica, after the configured margin.
	SafePerReplicaCapacity *float64 `json:"safePerReplicaCapacity,omitempty"`
	// CurrentSafeCapacity credits only traffic-bearing replicas, not every Kubernetes Ready pod.
	CurrentSafeCapacity *float64 `json:"currentSafeCapacity,omitempty"`
	// ReadyReplicas counts Kubernetes Ready target pods; readiness alone is not realized capacity.
	ReadyReplicas int32 `json:"readyReplicas"`
	// EffectiveServingReplicas is the Prometheus-derived count of traffic-bearing target instances.
	EffectiveServingReplicas               *int32   `json:"effectiveServingReplicas,omitempty"`
	AggregateConcurrencySlotOccupancy      *float64 `json:"aggregateConcurrencySlotOccupancy,omitempty"`
	HottestReplicaConcurrencySlotOccupancy *float64 `json:"hottestReplicaConcurrencySlotOccupancy,omitempty"`
	PhysicalConcurrencyLimit               *float64 `json:"physicalConcurrencyLimit,omitempty"`
	SafeOperatingOccupancy                 *float64 `json:"safeOperatingOccupancy,omitempty"`
	// ReadinessLeadTimeSeconds is the measured Pod creation-to-Ready duration.
	ReadinessLeadTimeSeconds *float64 `json:"readinessLeadTimeSeconds,omitempty"`
	// ReadinessEvidenceSource indicates whether prediction used a current fresh sample or persisted learned evidence.
	// +kubebuilder:validation:Enum=CURRENT_FRESH_SAMPLE;PERSISTED_LEARNED_SAMPLE
	ReadinessEvidenceSource ReadinessEvidenceSource `json:"readinessEvidenceSource,omitempty"`
	// ReadinessTemplateIdentity is the Kubernetes pod-template-hash associated with the readiness evidence.
	ReadinessTemplateIdentity string `json:"readinessTemplateIdentity,omitempty"`
	// ControlLoopAllowanceSeconds is the reconcile interval included in the planning horizon.
	ControlLoopAllowanceSeconds *float64 `json:"controlLoopAllowanceSeconds,omitempty"`
	PredictionAccepted          *bool    `json:"predictionAccepted,omitempty"`
	PredictionRejectedReason    string   `json:"predictionRejectedReason,omitempty"`
	DetectedBottleneck          string   `json:"detectedBottleneck,omitempty"`
	BottleneckComponent         string   `json:"bottleneckComponent,omitempty"`
	Confidence                  string   `json:"confidence,omitempty"`
	Evidence                    []string `json:"evidence,omitempty"`
	ChosenTarget                string   `json:"chosenTarget,omitempty"`
	RejectedActions             []string `json:"rejectedActions,omitempty"`
}

// DependencyRequestRate summarizes rate observations for a configured dependency.
// ErrorRate is a ratio in [0,1]; absent values indicate incomplete or undefined telemetry.
type DependencyRequestRate struct {
	Name                  string   `json:"name"`
	RequestRate           *float64 `json:"requestRate,omitempty"`
	SuccessfulRequestRate *float64 `json:"successfulRequestRate,omitempty"`
	ErrorRate             *float64 `json:"errorRate,omitempty"`
}

// ReadinessEvidenceSource identifies which revision-bound startup measurement informed prediction.
type ReadinessEvidenceSource string

const (
	ReadinessEvidenceCurrentFreshSample     ReadinessEvidenceSource = "CURRENT_FRESH_SAMPLE"
	ReadinessEvidencePersistedLearnedSample ReadinessEvidenceSource = "PERSISTED_LEARNED_SAMPLE"
)

// OptiScalerStatus defines the observed state of an OptiScaler.
type OptiScalerStatus struct {
	CurrentReplicas   int32           `json:"currentReplicas"`
	DesiredReplicas   int32           `json:"desiredReplicas"`
	ObservedMetric    *float64        `json:"observedMetric,omitempty"`
	ControlMode       ControlMode     `json:"controlMode,omitempty"`
	LastScaleTime     *metav1.Time    `json:"lastScaleTime,omitempty"`
	LastDecision      *DecisionRecord `json:"lastDecision,omitempty"`
	LastScaleDecision *DecisionRecord `json:"lastScaleDecision,omitempty"`
	// LearnedReadinessLeadTimeSeconds is the last valid conservative startup sample for the identified target revision.
	LearnedReadinessLeadTimeSeconds *float64 `json:"learnedReadinessLeadTimeSeconds,omitempty"`
	// LearnedReadinessObservedAt is the Ready transition time of the sample that established the learned lead time.
	LearnedReadinessObservedAt *metav1.Time `json:"learnedReadinessObservedAt,omitempty"`
	// LearnedReadinessTemplateIdentity is the Kubernetes pod-template-hash for the revision that produced the sample.
	LearnedReadinessTemplateIdentity string             `json:"learnedReadinessTemplateIdentity,omitempty"`
	Conditions                       []metav1.Condition `json:"conditions,omitempty"`
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
