package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	optiscalev1alpha1 "github.com/RajRaghupatruni/Silver-Leaf/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Input struct {
	Action                                 string
	Reason                                 string
	ObservedMetric                         *float64
	Threshold                              *float64
	CurrentReplicas                        int32
	DesiredReplicas                        int32
	ObservedAt                             time.Time
	SLOTargetP95Milliseconds               float64
	ObservedTargetP95Milliseconds          *float64
	TargetRequestRate                      *float64
	TargetSuccessfulRequestRate            *float64
	TargetErrorRate                        *float64
	PredictiveRequestRate                  *float64
	PredictiveDemandWindowSeconds          *float64
	DemandObservationLagSeconds            *float64
	DependencyRequestRates                 []optiscalev1alpha1.DependencyRequestRate
	ForecastRequestRate                    *float64
	ForecastHorizonSeconds                 *float64
	RequestRateSlope                       *float64
	ForecastConfidence                     string
	ForecastFitR2                          *float64
	SafePerReplicaCapacity                 *float64
	CurrentSafeCapacity                    *float64
	ReadyReplicas                          int32
	EffectiveServingReplicas               *int32
	AggregateConcurrencySlotOccupancy      *float64
	HottestReplicaConcurrencySlotOccupancy *float64
	PhysicalConcurrencyLimit               *float64
	SafeOperatingOccupancy                 *float64
	ReadinessLeadTimeSeconds               *float64
	ReadinessEvidenceSource                optiscalev1alpha1.ReadinessEvidenceSource
	ReadinessTemplateIdentity              string
	ControlLoopAllowanceSeconds            *float64
	PredictionAccepted                     *bool
	PredictionRejectedReason               string
	DetectedBottleneck                     string
	BottleneckComponent                    string
	Confidence                             string
	Evidence                               []string
	ChosenTarget                           string
	RejectedActions                        []string
}

func NewRecord(in Input) *optiscalev1alpha1.DecisionRecord {
	return &optiscalev1alpha1.DecisionRecord{
		ID:                                     ID(in),
		Timestamp:                              metav1.NewTime(time.Now().UTC()),
		Action:                                 optiscalev1alpha1.Action(in.Action),
		Reason:                                 in.Reason,
		ObservedMetric:                         in.ObservedMetric,
		Threshold:                              in.Threshold,
		CurrentReplicas:                        in.CurrentReplicas,
		DesiredReplicas:                        in.DesiredReplicas,
		SLOTargetP95Milliseconds:               in.SLOTargetP95Milliseconds,
		ObservedTargetP95Milliseconds:          cloneFloat64(in.ObservedTargetP95Milliseconds),
		TargetRequestRate:                      cloneFloat64(in.TargetRequestRate),
		TargetSuccessfulRequestRate:            cloneFloat64(in.TargetSuccessfulRequestRate),
		TargetErrorRate:                        cloneFloat64(in.TargetErrorRate),
		PredictiveRequestRate:                  cloneFloat64(in.PredictiveRequestRate),
		PredictiveDemandWindowSeconds:          cloneFloat64(in.PredictiveDemandWindowSeconds),
		DemandObservationLagSeconds:            cloneFloat64(in.DemandObservationLagSeconds),
		DependencyRequestRates:                 cloneDependencyRates(in.DependencyRequestRates),
		ForecastRequestRate:                    cloneFloat64(in.ForecastRequestRate),
		ForecastHorizonSeconds:                 cloneFloat64(in.ForecastHorizonSeconds),
		RequestRateSlope:                       cloneFloat64(in.RequestRateSlope),
		ForecastConfidence:                     in.ForecastConfidence,
		ForecastFitR2:                          cloneFloat64(in.ForecastFitR2),
		SafePerReplicaCapacity:                 cloneFloat64(in.SafePerReplicaCapacity),
		CurrentSafeCapacity:                    cloneFloat64(in.CurrentSafeCapacity),
		ReadyReplicas:                          in.ReadyReplicas,
		EffectiveServingReplicas:               cloneInt32(in.EffectiveServingReplicas),
		AggregateConcurrencySlotOccupancy:      cloneFloat64(in.AggregateConcurrencySlotOccupancy),
		HottestReplicaConcurrencySlotOccupancy: cloneFloat64(in.HottestReplicaConcurrencySlotOccupancy),
		PhysicalConcurrencyLimit:               cloneFloat64(in.PhysicalConcurrencyLimit),
		SafeOperatingOccupancy:                 cloneFloat64(in.SafeOperatingOccupancy),
		ReadinessLeadTimeSeconds:               cloneFloat64(in.ReadinessLeadTimeSeconds),
		ReadinessEvidenceSource:                in.ReadinessEvidenceSource,
		ReadinessTemplateIdentity:              in.ReadinessTemplateIdentity,
		ControlLoopAllowanceSeconds:            cloneFloat64(in.ControlLoopAllowanceSeconds),
		PredictionAccepted:                     cloneBool(in.PredictionAccepted),
		PredictionRejectedReason:               in.PredictionRejectedReason,
		DetectedBottleneck:                     in.DetectedBottleneck,
		BottleneckComponent:                    in.BottleneckComponent,
		Confidence:                             in.Confidence,
		Evidence:                               append([]string(nil), in.Evidence...),
		ChosenTarget:                           in.ChosenTarget,
		RejectedActions:                        append([]string(nil), in.RejectedActions...),
	}
}

func ID(in Input) string {
	value := fmt.Sprintf("%s|%s|%v|%v|%d|%d|%s|%.4f|%v|%s|%s|%s|%s", in.Action, in.Reason, pointerValue(in.ObservedMetric), pointerValue(in.Threshold), in.CurrentReplicas, in.DesiredReplicas, in.ObservedAt.UTC().Format(time.RFC3339Nano), in.SLOTargetP95Milliseconds, pointerValue(in.ObservedTargetP95Milliseconds), in.DetectedBottleneck, in.BottleneckComponent, in.Confidence, in.ChosenTarget)
	hash := sha256.Sum256([]byte(value))
	return "dec-" + hex.EncodeToString(hash[:8]) + "-" + strconv.FormatInt(in.ObservedAt.Unix(), 10)
}

func pointerValue(value *float64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatFloat(*value, 'g', -1, 64)
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt32(value *int32) *int32 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneDependencyRates(values []optiscalev1alpha1.DependencyRequestRate) []optiscalev1alpha1.DependencyRequestRate {
	if values == nil {
		return nil
	}
	copy := make([]optiscalev1alpha1.DependencyRequestRate, len(values))
	for i, value := range values {
		copy[i] = value
		copy[i].RequestRate = cloneFloat64(value.RequestRate)
		copy[i].SuccessfulRequestRate = cloneFloat64(value.SuccessfulRequestRate)
		copy[i].ErrorRate = cloneFloat64(value.ErrorRate)
	}
	return copy
}
