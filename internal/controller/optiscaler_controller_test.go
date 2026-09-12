package controller

import (
	"context"
	"testing"
	"time"

	optiscalev1alpha1 "github.com/RajRaghupatruni/Silver-Leaf/api/v1alpha1"
	"github.com/RajRaghupatruni/Silver-Leaf/internal/decision"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestShouldUpdateScale(t *testing.T) {
	if shouldUpdateScale(2, 2) {
		t.Fatal("expected identical replica counts to be a no-op")
	}
	if !shouldUpdateScale(2, 3) {
		t.Fatal("expected changed replica counts to require an update")
	}
}

func TestValidateSpec(t *testing.T) {
	spec := validSpec()
	if err := validateSpec(spec); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	spec.Prediction = optiscalev1alpha1.PredictionSpec{Enabled: true, SafeCapacityMargin: 0.75, MaxErrorRate: 0.02}
	if err := validateSpec(spec); err == nil {
		t.Fatal("prediction accepted without request-rate, error-rate, and utilization queries")
	}
	spec.Metric.RequestRateQuery = "requests"
	spec.Metric.ErrorRequestRateQuery = "errors"
	spec.Metric.UtilizationQuery = "active"
	spec.Metric.PhysicalConcurrencyLimit = 20
	if err := validateSpec(spec); err == nil {
		t.Fatal("prediction accepted without a separate predictive-demand query")
	}
	spec.Metric.PredictiveRequestRateQuery = "fast-requests"
	if err := validateSpec(spec); err == nil {
		t.Fatal("prediction accepted without an explicit predictive-demand window")
	}
	spec.Prediction.PredictiveDemandWindowSeconds = 30
	if err := validateSpec(spec); err != nil {
		t.Fatalf("valid prediction configuration rejected: %v", err)
	}
	spec.Prediction.PredictiveDemandWindowSeconds = 1
	if err := validateSpec(spec); err == nil {
		t.Fatal("predictive-demand window below the sensible range accepted")
	}
	spec.Prediction.PredictiveDemandWindowSeconds = 121
	if err := validateSpec(spec); err == nil {
		t.Fatal("predictive-demand window above the trend-fit window accepted")
	}
	spec.Prediction.PredictiveDemandWindowSeconds = 30
	spec.Prediction.SafeCapacityMargin = 0
	if err := validateSpec(spec); err == nil {
		t.Fatal("zero safe-capacity margin accepted")
	}
	spec.Prediction = optiscalev1alpha1.PredictionSpec{}
	spec.Metric.RequestRateQuery = ""
	spec.Metric.ErrorRequestRateQuery = ""
	spec.Metric.UtilizationQuery = ""
	spec.Metric.RequestRateQuery = "requests"
	if err := validateSpec(spec); err == nil {
		t.Fatal("request-rate query accepted without matching error-rate query")
	}
	spec.Metric.ErrorRequestRateQuery = "errors"
	spec.Metric.UtilizationQuery = "active"
	spec.Metric.PhysicalConcurrencyLimit = 20
	spec.Dependencies = []optiscalev1alpha1.DependencySpec{
		{
			Name: "inventory-service", DependsOn: "postgres",
			ScaleTargetRef: optiscalev1alpha1.ScaleTargetReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "inventory-service"},
			Metrics:        optiscalev1alpha1.DependencyMetricsSpec{LatencyQuery: "latency", UtilizationQuery: "active"},
			Thresholds:     optiscalev1alpha1.DependencyThresholdsSpec{LatencyMilliseconds: 250, Utilization: 100},
			Scalable:       true, MinReplicas: 1, MaxReplicas: 5,
		},
		{
			Name:           "postgres",
			ScaleTargetRef: optiscalev1alpha1.ScaleTargetReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "postgres"},
			Metrics:        optiscalev1alpha1.DependencyMetricsSpec{LatencyQuery: "latency", UtilizationQuery: "active"},
			Thresholds:     optiscalev1alpha1.DependencyThresholdsSpec{LatencyMilliseconds: 250, Utilization: 8},
			MinReplicas:    1, MaxReplicas: 1,
		},
	}
	if err := validateSpec(spec); err != nil {
		t.Fatalf("valid dependency topology rejected: %v", err)
	}
	spec.Dependencies[0].Metrics.RequestRateQuery = "requests"
	if err := validateSpec(spec); err == nil {
		t.Fatal("dependency request-rate query accepted without matching error-rate query")
	}
	spec.Dependencies[0].Metrics.ErrorRequestRateQuery = "errors"
	if err := validateSpec(spec); err != nil {
		t.Fatalf("valid paired dependency rate queries rejected: %v", err)
	}
	spec.Dependencies[0].DependsOn = "missing"
	if err := validateSpec(spec); err == nil {
		t.Fatal("unconfigured downstream dependency accepted")
	}
	spec.MaxReplicas = 0
	if err := validateSpec(spec); err == nil {
		t.Fatal("invalid replica bounds accepted")
	}
}

func TestStatusDecisionRecordCreation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := optiscalev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resource := &optiscalev1alpha1.OptiScaler{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "optiscale-demo"}, Spec: validSpec()}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&optiscalev1alpha1.OptiScaler{}).WithObjects(resource).Build()
	r := &OptiScalerReconciler{Client: client, Scheme: scheme}
	metric := 300.0
	requestRate := 40.0
	successfulRequestRate := 38.0
	errorRate := 0.05
	dependencyRequestRate := 20.0
	forecastRate := 72.5
	predictiveRate := 71.0
	demandWindow := 30.0
	demandLag := 15.0
	requestRateSlope := 0.8
	predictionAccepted := true
	effectiveServing := int32(2)
	now := time.Now().UTC().Truncate(time.Second)
	status := resource.Status
	status.CurrentReplicas = 2
	status.DesiredReplicas = 2
	status.ControlMode = optiscalev1alpha1.ControlModeHold
	status.LearnedReadinessLeadTimeSeconds = float64Pointer(22)
	status.LearnedReadinessObservedAt = &metav1.Time{Time: now.Add(-time.Minute)}
	status.LearnedReadinessTemplateIdentity = "hash-current"
	status.LastDecision = decision.NewRecord(decision.Input{
		Action: string(optiscalev1alpha1.ActionHold), Reason: "capacity is blocked by non-scalable dependency postgres",
		ObservedMetric: &metric, CurrentReplicas: 1, DesiredReplicas: 1, ObservedAt: now,
		DetectedBottleneck: "CAPACITY_BLOCKED", BottleneckComponent: "postgres", Confidence: "HIGH",
		Evidence: []string{"database p95 exceeds threshold"}, ChosenTarget: "postgres",
		RejectedActions:   []string{"SCALE_TARGET: target is not locally saturated", "SCALE_DEPENDENCY: dependency is non-scalable"},
		TargetRequestRate: &requestRate, TargetSuccessfulRequestRate: &successfulRequestRate, TargetErrorRate: &errorRate,
		PredictiveRequestRate: &predictiveRate, PredictiveDemandWindowSeconds: &demandWindow, DemandObservationLagSeconds: &demandLag,
		ReadinessEvidenceSource: optiscalev1alpha1.ReadinessEvidencePersistedLearnedSample, ReadinessTemplateIdentity: "hash-current",
		DependencyRequestRates: []optiscalev1alpha1.DependencyRequestRate{{Name: "postgres", RequestRate: &dependencyRequestRate, ErrorRate: &errorRate}},
		ForecastRequestRate:    &forecastRate, ForecastHorizonSeconds: float64Pointer(33), RequestRateSlope: &requestRateSlope,
		ForecastConfidence: "HIGH", ForecastFitR2: float64Pointer(0.98), SafePerReplicaCapacity: float64Pointer(25),
		CurrentSafeCapacity: float64Pointer(50), ReadinessLeadTimeSeconds: float64Pointer(18), ControlLoopAllowanceSeconds: float64Pointer(15), PredictionAccepted: &predictionAccepted,
		ReadyReplicas: 3, EffectiveServingReplicas: &effectiveServing, AggregateConcurrencySlotOccupancy: float64Pointer(13.8),
		HottestReplicaConcurrencySlotOccupancy: float64Pointer(9.2), PhysicalConcurrencyLimit: float64Pointer(20), SafeOperatingOccupancy: float64Pointer(15),
	})
	if err := r.updateStatus(context.Background(), resource, status); err != nil {
		t.Fatal(err)
	}
	var got optiscalev1alpha1.OptiScaler
	if err := client.Get(context.Background(), types.NamespacedName{Name: "demo", Namespace: "optiscale-demo"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.LastDecision == nil || got.Status.LastDecision.Action != optiscalev1alpha1.ActionHold {
		t.Fatalf("unexpected status: %+v", got.Status)
	}
	if got.Status.LastDecision.DetectedBottleneck != "CAPACITY_BLOCKED" || got.Status.LastDecision.BottleneckComponent != "postgres" || got.Status.LastDecision.Confidence != "HIGH" || len(got.Status.LastDecision.RejectedActions) != 2 {
		t.Fatalf("capacity-blocked DecisionRecord fields were not persisted: %+v", got.Status.LastDecision)
	}
	if got.Status.LastDecision.TargetRequestRate == nil || *got.Status.LastDecision.TargetRequestRate != requestRate || got.Status.LastDecision.TargetSuccessfulRequestRate == nil || *got.Status.LastDecision.TargetSuccessfulRequestRate != successfulRequestRate || got.Status.LastDecision.TargetErrorRate == nil || *got.Status.LastDecision.TargetErrorRate != errorRate || len(got.Status.LastDecision.DependencyRequestRates) != 1 || got.Status.LastDecision.DependencyRequestRates[0].RequestRate == nil || *got.Status.LastDecision.DependencyRequestRates[0].RequestRate != dependencyRequestRate {
		t.Fatalf("request-rate observations were not persisted: %+v", got.Status.LastDecision)
	}
	if got.Status.LastDecision.ForecastRequestRate == nil || *got.Status.LastDecision.ForecastRequestRate != forecastRate || got.Status.LastDecision.ForecastHorizonSeconds == nil || *got.Status.LastDecision.ForecastHorizonSeconds != 33 || got.Status.LastDecision.ReadinessLeadTimeSeconds == nil || *got.Status.LastDecision.ReadinessLeadTimeSeconds != 18 || got.Status.LastDecision.ControlLoopAllowanceSeconds == nil || *got.Status.LastDecision.ControlLoopAllowanceSeconds != 15 || got.Status.LastDecision.ForecastConfidence != "HIGH" || got.Status.LastDecision.PredictionAccepted == nil || !*got.Status.LastDecision.PredictionAccepted || got.Status.LastDecision.CurrentSafeCapacity == nil || *got.Status.LastDecision.CurrentSafeCapacity != 50 {
		t.Fatalf("forecast DecisionRecord fields were not persisted: %+v", got.Status.LastDecision)
	}
	if got.Status.LastDecision.PredictiveRequestRate == nil || *got.Status.LastDecision.PredictiveRequestRate != predictiveRate || got.Status.LastDecision.PredictiveDemandWindowSeconds == nil || *got.Status.LastDecision.PredictiveDemandWindowSeconds != demandWindow || got.Status.LastDecision.DemandObservationLagSeconds == nil || *got.Status.LastDecision.DemandObservationLagSeconds != demandLag {
		t.Fatalf("predictive-demand signal/window/lag were not persisted: %+v", got.Status.LastDecision)
	}
	if got.Status.LastDecision.ReadyReplicas != 3 || got.Status.LastDecision.EffectiveServingReplicas == nil || *got.Status.LastDecision.EffectiveServingReplicas != 2 || got.Status.LastDecision.AggregateConcurrencySlotOccupancy == nil || *got.Status.LastDecision.AggregateConcurrencySlotOccupancy != 13.8 || got.Status.LastDecision.HottestReplicaConcurrencySlotOccupancy == nil || *got.Status.LastDecision.HottestReplicaConcurrencySlotOccupancy != 9.2 || got.Status.LastDecision.PhysicalConcurrencyLimit == nil || *got.Status.LastDecision.PhysicalConcurrencyLimit != 20 || got.Status.LastDecision.SafeOperatingOccupancy == nil || *got.Status.LastDecision.SafeOperatingOccupancy != 15 {
		t.Fatalf("realized target capacity evidence was not persisted: %+v", got.Status.LastDecision)
	}
	if got.Status.LearnedReadinessLeadTimeSeconds == nil || *got.Status.LearnedReadinessLeadTimeSeconds != 22 || got.Status.LearnedReadinessObservedAt == nil || !got.Status.LearnedReadinessObservedAt.Time.Equal(now.Add(-time.Minute)) || got.Status.LearnedReadinessTemplateIdentity != "hash-current" || got.Status.LastDecision.ReadinessEvidenceSource != optiscalev1alpha1.ReadinessEvidencePersistedLearnedSample || got.Status.LastDecision.ReadinessTemplateIdentity != "hash-current" {
		t.Fatalf("revision-bound learned readiness status/evidence was not persisted: %+v", got.Status)
	}
}

func float64Pointer(value float64) *float64 { return &value }

func TestLastScaleDecisionSurvivesLaterHold(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := optiscalev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scaled := &optiscalev1alpha1.DecisionRecord{ID: "scale-1", Action: optiscalev1alpha1.ActionScaleDependency, ChosenTarget: "inventory-service", Evidence: []string{"dependency p95 exceeded"}}
	resource := &optiscalev1alpha1.OptiScaler{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "optiscale-demo"}, Spec: validSpec(), Status: optiscalev1alpha1.OptiScalerStatus{LastScaleDecision: scaled}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&optiscalev1alpha1.OptiScaler{}).WithObjects(resource).Build()
	r := &OptiScalerReconciler{Client: client, Scheme: scheme}
	status := resource.Status
	status.LastDecision = &optiscalev1alpha1.DecisionRecord{ID: "hold-1", Action: optiscalev1alpha1.ActionHold, Reason: "healthy"}
	if err := r.updateStatus(context.Background(), resource, status); err != nil {
		t.Fatal(err)
	}
	var got optiscalev1alpha1.OptiScaler
	if err := client.Get(context.Background(), types.NamespacedName{Name: "demo", Namespace: "optiscale-demo"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.LastDecision == nil || got.Status.LastDecision.Action != optiscalev1alpha1.ActionHold {
		t.Fatalf("latest decision not persisted: %+v", got.Status.LastDecision)
	}
	if got.Status.LastScaleDecision == nil || got.Status.LastScaleDecision.ID != "scale-1" {
		t.Fatalf("last scaling decision was not retained: %+v", got.Status.LastScaleDecision)
	}
}

func validSpec() optiscalev1alpha1.OptiScalerSpec {
	return optiscalev1alpha1.OptiScalerSpec{
		ScaleTargetRef: optiscalev1alpha1.ScaleTargetReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "demo"},
		MinReplicas:    1, MaxReplicas: 5,
		Metric: optiscalev1alpha1.MetricSpec{
			PrometheusQuery: "up", UtilizationQuery: "slot-rate", HottestReplicaUtilizationQuery: "hot-slot-rate",
			ServingReplicaCountQuery: "serving-replicas", PhysicalConcurrencyLimit: 20,
		},
		SLO:        optiscalev1alpha1.SLOSpec{TargetP95Milliseconds: 250},
		Policy:     optiscalev1alpha1.PolicySpec{ScaleUpThreshold: 250, ScaleDownThreshold: 150, MaxScaleUpStep: 1, MaxScaleDownStep: 1},
		Prometheus: optiscalev1alpha1.PrometheusSpec{Address: "http://prometheus.example"},
	}
}
