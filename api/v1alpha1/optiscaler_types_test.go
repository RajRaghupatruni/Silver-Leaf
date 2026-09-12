package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeepCopyIncludesDependenciesAndScaleDecision(t *testing.T) {
	requestRate := 12.0
	learnedReadiness := 22.0
	learnedAt := metav1.NewTime(time.Date(2026, 9, 12, 6, 51, 24, 0, time.UTC))
	forecastRate := 45.0
	predictiveRate := 44.0
	demandWindow := 30.0
	demandLag := 15.0
	predictionAccepted := true
	effectiveServing := int32(2)
	aggregateSlotOccupancy := 13.8
	hottestSlotOccupancy := 9.2
	physicalSlotLimit := 20.0
	safeSlotBoundary := 15.0
	resource := &OptiScaler{
		Spec: OptiScalerSpec{Dependencies: []DependencySpec{{Name: "inventory", ScaleTargetRef: ScaleTargetReference{Name: "inventory-service"}}}, Prediction: PredictionSpec{Enabled: true, SafeCapacityMargin: 0.75, MaxErrorRate: 0.02, PredictiveDemandWindowSeconds: 30}},
		Status: OptiScalerStatus{LastScaleDecision: &DecisionRecord{
			Evidence: []string{"observed"}, RejectedActions: []string{"SCALE_TARGET"}, TargetRequestRate: &requestRate,
			Action: ActionPrescaleTarget, ForecastRequestRate: &forecastRate, PredictionAccepted: &predictionAccepted,
			PredictiveRequestRate: &predictiveRate, PredictiveDemandWindowSeconds: &demandWindow, DemandObservationLagSeconds: &demandLag,
			ReadinessEvidenceSource: ReadinessEvidencePersistedLearnedSample, ReadinessTemplateIdentity: "hash-42",
			DependencyRequestRates: []DependencyRequestRate{{Name: "postgres", RequestRate: &requestRate}},
			ReadyReplicas:          3, EffectiveServingReplicas: &effectiveServing,
			AggregateConcurrencySlotOccupancy: &aggregateSlotOccupancy, HottestReplicaConcurrencySlotOccupancy: &hottestSlotOccupancy,
			PhysicalConcurrencyLimit: &physicalSlotLimit, SafeOperatingOccupancy: &safeSlotBoundary,
		}, LearnedReadinessLeadTimeSeconds: &learnedReadiness, LearnedReadinessObservedAt: &learnedAt, LearnedReadinessTemplateIdentity: "hash-42"},
	}
	copy := resource.DeepCopy()
	copy.Spec.Dependencies[0].Name = "changed"
	copy.Status.LastScaleDecision.Evidence[0] = "changed"
	copy.Status.LastScaleDecision.RejectedActions[0] = "changed"
	*copy.Status.LastScaleDecision.TargetRequestRate = 99
	*copy.Status.LastScaleDecision.ForecastRequestRate = 99
	*copy.Status.LastScaleDecision.PredictiveRequestRate = 99
	*copy.Status.LastScaleDecision.PredictiveDemandWindowSeconds = 99
	*copy.Status.LastScaleDecision.DemandObservationLagSeconds = 99
	copy.Status.LastScaleDecision.ReadinessTemplateIdentity = "changed"
	*copy.Status.LearnedReadinessLeadTimeSeconds = 99
	copy.Status.LearnedReadinessObservedAt.Time = time.Time{}
	copy.Status.LearnedReadinessTemplateIdentity = "changed"
	*copy.Status.LastScaleDecision.PredictionAccepted = false
	*copy.Status.LastScaleDecision.EffectiveServingReplicas = 99
	*copy.Status.LastScaleDecision.AggregateConcurrencySlotOccupancy = 99
	*copy.Status.LastScaleDecision.HottestReplicaConcurrencySlotOccupancy = 99
	*copy.Status.LastScaleDecision.PhysicalConcurrencyLimit = 99
	*copy.Status.LastScaleDecision.SafeOperatingOccupancy = 99
	*copy.Status.LastScaleDecision.DependencyRequestRates[0].RequestRate = 99
	if resource.Spec.Dependencies[0].Name != "inventory" || resource.Spec.Prediction.SafeCapacityMargin != 0.75 || resource.Spec.Prediction.PredictiveDemandWindowSeconds != 30 || resource.Status.LastScaleDecision.Evidence[0] != "observed" || resource.Status.LastScaleDecision.RejectedActions[0] != "SCALE_TARGET" || *resource.Status.LastScaleDecision.TargetRequestRate != requestRate || *resource.Status.LastScaleDecision.ForecastRequestRate != forecastRate || *resource.Status.LastScaleDecision.PredictiveRequestRate != predictiveRate || *resource.Status.LastScaleDecision.PredictiveDemandWindowSeconds != demandWindow || *resource.Status.LastScaleDecision.DemandObservationLagSeconds != demandLag || !*resource.Status.LastScaleDecision.PredictionAccepted || resource.Status.LastScaleDecision.ReadinessEvidenceSource != ReadinessEvidencePersistedLearnedSample || resource.Status.LastScaleDecision.ReadinessTemplateIdentity != "hash-42" || *resource.Status.LearnedReadinessLeadTimeSeconds != learnedReadiness || !resource.Status.LearnedReadinessObservedAt.Time.Equal(learnedAt.Time) || resource.Status.LearnedReadinessTemplateIdentity != "hash-42" || *resource.Status.LastScaleDecision.DependencyRequestRates[0].RequestRate != requestRate || resource.Status.LastScaleDecision.ReadyReplicas != 3 || *resource.Status.LastScaleDecision.EffectiveServingReplicas != effectiveServing || *resource.Status.LastScaleDecision.AggregateConcurrencySlotOccupancy != aggregateSlotOccupancy || *resource.Status.LastScaleDecision.HottestReplicaConcurrencySlotOccupancy != hottestSlotOccupancy || *resource.Status.LastScaleDecision.PhysicalConcurrencyLimit != physicalSlotLimit || *resource.Status.LastScaleDecision.SafeOperatingOccupancy != safeSlotBoundary {
		t.Fatal("deep copy shares mutable dependency or decision slices")
	}
}
