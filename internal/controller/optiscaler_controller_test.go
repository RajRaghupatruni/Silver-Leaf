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
	spec.Metric.RequestRateQuery = "requests"
	if err := validateSpec(spec); err == nil {
		t.Fatal("request-rate query accepted without matching error-rate query")
	}
	spec.Metric.ErrorRequestRateQuery = "errors"
	spec.Metric.UtilizationQuery = "active"
	spec.Metric.UtilizationThreshold = 30
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
	now := time.Now().UTC()
	status := resource.Status
	status.CurrentReplicas = 2
	status.DesiredReplicas = 2
	status.ControlMode = optiscalev1alpha1.ControlModeHold
	status.LastDecision = decision.NewRecord(decision.Input{
		Action: string(optiscalev1alpha1.ActionHold), Reason: "capacity is blocked by non-scalable dependency postgres",
		ObservedMetric: &metric, CurrentReplicas: 1, DesiredReplicas: 1, ObservedAt: now,
		DetectedBottleneck: "CAPACITY_BLOCKED", BottleneckComponent: "postgres", Confidence: "HIGH",
		Evidence: []string{"database p95 exceeds threshold"}, ChosenTarget: "postgres",
		RejectedActions:   []string{"SCALE_TARGET: target is not locally saturated", "SCALE_DEPENDENCY: dependency is non-scalable"},
		TargetRequestRate: &requestRate, TargetSuccessfulRequestRate: &successfulRequestRate, TargetErrorRate: &errorRate,
		DependencyRequestRates: []optiscalev1alpha1.DependencyRequestRate{{Name: "postgres", RequestRate: &dependencyRequestRate, ErrorRate: &errorRate}},
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
}

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
		Metric:     optiscalev1alpha1.MetricSpec{PrometheusQuery: "up"},
		SLO:        optiscalev1alpha1.SLOSpec{TargetP95Milliseconds: 250},
		Policy:     optiscalev1alpha1.PolicySpec{ScaleUpThreshold: 250, ScaleDownThreshold: 150, MaxScaleUpStep: 1, MaxScaleDownStep: 1},
		Prometheus: optiscalev1alpha1.PrometheusSpec{Address: "http://prometheus.example"},
	}
}
