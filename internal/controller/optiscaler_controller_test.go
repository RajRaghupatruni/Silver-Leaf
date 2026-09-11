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
	now := time.Now().UTC()
	status := resource.Status
	status.CurrentReplicas = 2
	status.DesiredReplicas = 3
	status.ControlMode = optiscalev1alpha1.ControlModeAutomatic
	status.LastDecision = decision.NewRecord(decision.Input{Action: string(optiscalev1alpha1.ActionScaleTarget), Reason: "scale-up threshold exceeded", ObservedMetric: &metric, CurrentReplicas: 2, DesiredReplicas: 3, ObservedAt: now})
	if err := r.updateStatus(context.Background(), resource, status); err != nil {
		t.Fatal(err)
	}
	var got optiscalev1alpha1.OptiScaler
	if err := client.Get(context.Background(), types.NamespacedName{Name: "demo", Namespace: "optiscale-demo"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.LastDecision == nil || got.Status.LastDecision.Action != optiscalev1alpha1.ActionScaleTarget {
		t.Fatalf("unexpected status: %+v", got.Status)
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
