package controller

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	optiscalev1alpha1 "github.com/RajRaghupatruni/Silver-Leaf/api/v1alpha1"
	"github.com/RajRaghupatruni/Silver-Leaf/internal/capacity"
	"github.com/RajRaghupatruni/Silver-Leaf/internal/forecast"
	"github.com/RajRaghupatruni/Silver-Leaf/internal/observation"
	"github.com/RajRaghupatruni/Silver-Leaf/internal/policy"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCapacitySampleEligibilityRequiresHealthyErrorAndLoadedEvidence(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	base := observation.Snapshot{
		Target: observation.TargetObservation{
			CurrentReplicas: 2, ReadyReplicas: 2,
			P95Latency:               observation.Metric{Value: 80, ObservedAt: now, Valid: true, Fresh: true},
			AggregateSlotOccupancy:   observation.Metric{Value: 13.8, ObservedAt: now, Valid: true, Fresh: true},
			HottestReplicaOccupancy:  observation.Metric{Value: 6.9, ObservedAt: now, Valid: true, Fresh: true},
			EffectiveServingReplicas: observation.Metric{Value: 2, ObservedAt: now, Valid: true, Fresh: true},
			PhysicalConcurrencyLimit: 20, SafeOperatingOccupancy: 15,
			RequestRates: observation.RequestRates{
				RequestRate:           observation.Metric{Value: 50, ObservedAt: now, Valid: true, Fresh: true},
				SuccessfulRequestRate: observation.Metric{Value: 49, ObservedAt: now, Valid: true, Fresh: true},
				ErrorRate:             observation.Metric{Value: 0.02, ObservedAt: now, Valid: true, Fresh: true},
			},
		},
	}
	tests := []struct {
		name   string
		mutate func(*observation.Snapshot, *capacity.Analysis, *int32)
		want   bool
		reason string
	}{
		{name: "calibrated 13.8 aggregate / 2 ready = 6.9 per replica is learnable", mutate: func(*observation.Snapshot, *capacity.Analysis, *int32) {}, want: true},
		{name: "error rate too high", mutate: func(snapshot *observation.Snapshot, _ *capacity.Analysis, _ *int32) {
			snapshot.Target.RequestRates.ErrorRate.Value = 0.03
		}, reason: "error rate"},
		{name: "missing error metric", mutate: func(snapshot *observation.Snapshot, _ *capacity.Analysis, _ *int32) {
			snapshot.Target.RequestRates.ErrorRate.Valid = false
		}, reason: "complete fresh"},
		{name: "stale request rate", mutate: func(snapshot *observation.Snapshot, _ *capacity.Analysis, _ *int32) {
			snapshot.Target.RequestRates.RequestRate.Fresh = false
		}, reason: "complete fresh"},
		{name: "underloaded", mutate: func(snapshot *observation.Snapshot, _ *capacity.Analysis, _ *int32) {
			snapshot.Target.AggregateSlotOccupancy.Value = 8.98
		}, reason: "load-evidence"},
		{name: "at saturation boundary", mutate: func(snapshot *observation.Snapshot, _ *capacity.Analysis, _ *int32) {
			snapshot.Target.HottestReplicaOccupancy.Value = 15
		}, reason: "at or above its safe operating occupancy boundary"},
		{name: "missing aggregate slot occupancy", mutate: func(snapshot *observation.Snapshot, _ *capacity.Analysis, _ *int32) {
			snapshot.Target.AggregateSlotOccupancy = observation.Metric{}
		}, reason: "aggregate constrained-slot occupancy"},
		{name: "stale aggregate slot occupancy", mutate: func(snapshot *observation.Snapshot, _ *capacity.Analysis, _ *int32) {
			snapshot.Target.AggregateSlotOccupancy.Fresh = false
		}, reason: "aggregate constrained-slot occupancy"},
		{name: "Ready but not all traffic-bearing", mutate: func(snapshot *observation.Snapshot, _ *capacity.Analysis, ready *int32) {
			*ready = 3
			snapshot.Target.ReadyReplicas = 3
			snapshot.Target.CurrentReplicas = 3
			snapshot.Target.EffectiveServingReplicas.Value = 2
		}, reason: "traffic-bearing"},
		{name: "no traffic-bearing replica", mutate: func(snapshot *observation.Snapshot, _ *capacity.Analysis, _ *int32) {
			snapshot.Target.EffectiveServingReplicas.Value = 0
		}, reason: "no traffic-bearing replicas"},
		{name: "no ready replica", mutate: func(_ *observation.Snapshot, _ *capacity.Analysis, ready *int32) { *ready = 0 }, reason: "no Ready replicas"},
		{name: "target still converging", mutate: func(snapshot *observation.Snapshot, _ *capacity.Analysis, ready *int32) {
			*ready = 1
			snapshot.Target.CurrentReplicas = 2
		}, reason: "still converging"},
		{name: "non-healthy analysis", mutate: func(_ *observation.Snapshot, analysis *capacity.Analysis, _ *int32) {
			analysis.Classification = capacity.DependencySaturated
		}, reason: "classification"},
		{name: "dependency saturated while target is healthy", mutate: func(snapshot *observation.Snapshot, _ *capacity.Analysis, _ *int32) {
			snapshot.Dependencies = []observation.DependencyObservation{{Name: "inventory", P95Latency: observation.Metric{Value: 300, ObservedAt: now, Valid: true, Fresh: true}, Utilization: observation.Metric{Value: 20, ObservedAt: now, Valid: true, Fresh: true}, LatencyThreshold: 250, UtilizationThreshold: 100}}
		}, reason: "dependency \"inventory\" is saturated"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := base
			analysis := capacity.Analysis{Classification: capacity.Healthy}
			ready := int32(2)
			test.mutate(&snapshot, &analysis, &ready)
			ok, reason := capacitySampleEligibility(snapshot, analysis, 0.02, ready)
			if ok != test.want || (test.reason != "" && !strings.Contains(reason, test.reason)) {
				t.Fatalf("eligibility=(%t,%q), want (%t, containing %q)", ok, reason, test.want, test.reason)
			}
		})
	}
}

func TestTargetConcurrencyEvidenceExplainsRealizedServingCapacity(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	metric := observation.Metric{Value: 13.8, ObservedAt: now, Valid: true, Fresh: true}
	target := observation.TargetObservation{
		Name: "demo-api", CurrentReplicas: 3, ReadyReplicas: 3,
		AggregateSlotOccupancy: metric, HottestReplicaOccupancy: observation.Metric{Value: 9, ObservedAt: now, Valid: true, Fresh: true},
		EffectiveServingReplicas: observation.Metric{Value: 2, ObservedAt: now, Valid: true, Fresh: true},
		PhysicalConcurrencyLimit: 20, SafeOperatingOccupancy: 15,
	}
	evidence := targetConcurrencyEvidence(target)
	for _, want := range []string{"requested replicas 3", "Ready replicas 3", "effective serving replicas 2", "aggregate held-slot occupancy 13.80", "hottest traffic-bearing replica 9.00", "physical concurrency ceiling 20.00", "safe operating boundary 15.00", "mean occupancy 6.90"} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("capacity evidence %q does not contain %q", evidence, want)
		}
	}
	target.EffectiveServingReplicas.Value = 4
	if unavailable := targetConcurrencyEvidence(target); !strings.Contains(unavailable, "exceed Kubernetes Ready replicas") {
		t.Fatalf("impossible serving count should explain invalid attribution, got %q", unavailable)
	}
}

func TestReadyButNotServingReplicaHoldsTargetScale(t *testing.T) {
	target := observation.TargetObservation{
		CurrentReplicas: 3, ReadyReplicas: 3,
		EffectiveServingReplicas: observation.Metric{Value: 2, ObservedAt: time.Now(), Valid: true, Fresh: true},
	}
	decision := policy.CapacityDecision{Action: policy.ActionPrescaleTarget, ChosenComponent: "target", CurrentReplicas: 3, DesiredReplicas: 4}
	got := holdTargetScaleForUnrealizedCapacity(decision, target, 3)
	if got.Action != policy.ActionHold || got.CurrentReplicas != 3 || got.DesiredReplicas != 3 || !strings.Contains(got.Reason, "3 Kubernetes Ready replicas but only 2 are traffic-bearing") {
		t.Fatalf("unrealized target capacity was not held safely: %+v", got)
	}
	dependencyDecision := policy.CapacityDecision{Action: policy.ActionScaleDependency, ChosenComponent: "inventory-service", CurrentReplicas: 1, DesiredReplicas: 2}
	if got := holdTargetScaleForUnrealizedCapacity(dependencyDecision, target, 3); got.Action != policy.ActionScaleDependency || got.DesiredReplicas != 2 {
		t.Fatalf("target realization guard unexpectedly changed dependency scaling: %+v", got)
	}
	target.EffectiveServingReplicas.Value = 3
	if got := holdTargetScaleForUnrealizedCapacity(decision, target, 3); got.Action != policy.ActionPrescaleTarget {
		t.Fatalf("guard remained active after every Ready replica became traffic-bearing: %+v", got)
	}
	target.EffectiveServingReplicas.Value = 0
	if got := holdTargetScaleForUnrealizedCapacity(decision, target, 3); got.Action != policy.ActionHold || got.DesiredReplicas != 3 {
		t.Fatalf("zero traffic-bearing replicas must not permit another target scale: %+v", got)
	}
}

func TestTargetReadinessMeasuresCreationToReadyAndUsesReadyPods(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	labels := map[string]string{"app": "demo-api"}
	deployment := readinessTestDeployment(labels)
	replicaSet := readinessTestReplicaSet(deployment, "rev-current")
	overnight := now.Add(-10*time.Hour - 17*time.Second)
	pods := []corev1.Pod{
		readyPodForRevision("demo-api-a", deployment, "rev-current", now.Add(-40*time.Second), now.Add(-18*time.Second), 0),
		readyPodForRevision("demo-api-b", deployment, "rev-current", now.Add(-30*time.Second), now.Add(-5*time.Second), 0),
		readyPodForRevision("demo-api-restarted", deployment, "rev-current", overnight, now.Add(-2*time.Second), 1),
		readyPodForRevision("demo-api-old-revision", deployment, "rev-old", now.Add(-50*time.Second), now.Add(-5*time.Second), 0),
		readyPodForRevision("demo-api-invalid-times", deployment, "rev-current", now.Add(-4*time.Second), now.Add(-5*time.Second), 0),
		readyPodForRevision("demo-api-no-creation-time", deployment, "rev-current", time.Time{}, now.Add(-time.Second), 0),
		{ObjectMeta: metav1.ObjectMeta{Name: "demo-api-not-ready", Namespace: "optiscale-demo", Labels: labels, CreationTimestamp: metav1.NewTime(now.Add(-time.Minute))}},
	}
	terminating := readyPodForRevision("demo-api-terminating", deployment, "rev-current", now.Add(-time.Minute), now.Add(-30*time.Second), 0)
	terminating.DeletionTimestamp = &metav1.Time{Time: now}
	terminating.Finalizers = []string{"test-finalizer"}
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, replicaSet, &pods[0], &pods[1], &pods[2], &pods[3], &pods[4], &pods[5], &pods[6], &terminating).Build()
	r := &OptiScalerReconciler{Client: client}
	readiness, err := r.targetReadiness(context.Background(), deployment)
	if err != nil {
		t.Fatal(err)
	}
	if readiness.ReadyReplicas != 6 {
		t.Fatalf("all Ready, non-terminating selected pods should count even with invalid timestamps/restarts; got %d", readiness.ReadyReplicas)
	}
	if !readiness.HasFreshSample || readiness.FreshLeadTime != 25*time.Second || readiness.FreshSampleCount != 2 || readiness.TemplateIdentity != "rev-current" {
		t.Fatalf("fresh current-revision readiness should use the maximum eligible 25s sample and reject the overnight/restarted sample: %+v", readiness)
	}
	learnedAt := now.Add(-time.Minute)
	status := optiscalev1alpha1.OptiScalerStatus{
		LearnedReadinessLeadTimeSeconds:  float64Pointer(99),
		LearnedReadinessObservedAt:       &metav1.Time{Time: learnedAt},
		LearnedReadinessTemplateIdentity: "rev-old",
	}
	evidence := resolveReadinessLead(readiness, status)
	if !evidence.Available || !evidence.Persist || evidence.Source != optiscalev1alpha1.ReadinessEvidenceCurrentFreshSample || evidence.LeadTime != 25*time.Second || evidence.TemplateIdentity != "rev-current" {
		t.Fatalf("fresh current-revision sample should replace mismatched learned state: %+v", evidence)
	}
	persistReadinessEvidence(&status, evidence)
	if status.LearnedReadinessLeadTimeSeconds == nil || *status.LearnedReadinessLeadTimeSeconds != 25 || status.LearnedReadinessTemplateIdentity != "rev-current" || status.LearnedReadinessObservedAt == nil || !status.LearnedReadinessObservedAt.Time.Equal(now.Add(-5*time.Second)) {
		t.Fatalf("new-revision fresh sample did not replace persisted readiness state: %+v", status)
	}

	snapshot := observation.Snapshot{Target: observation.TargetObservation{
		CurrentReplicas: 6, ReadyReplicas: readiness.ReadyReplicas,
		P95Latency:               observation.Metric{Value: 80, ObservedAt: now, Valid: true, Fresh: true},
		AggregateSlotOccupancy:   observation.Metric{Value: 41.4, ObservedAt: now, Valid: true, Fresh: true},
		HottestReplicaOccupancy:  observation.Metric{Value: 10, ObservedAt: now, Valid: true, Fresh: true},
		EffectiveServingReplicas: observation.Metric{Value: 6, ObservedAt: now, Valid: true, Fresh: true},
		PhysicalConcurrencyLimit: 20, SafeOperatingOccupancy: 15,
		RequestRates: observation.RequestRates{
			RequestRate:           observation.Metric{Value: 50, ObservedAt: now, Valid: true, Fresh: true},
			SuccessfulRequestRate: observation.Metric{Value: 49, ObservedAt: now, Valid: true, Fresh: true},
			ErrorRate:             observation.Metric{Value: 0.02, ObservedAt: now, Valid: true, Fresh: true},
		},
	}}
	if eligible, reason := capacitySampleEligibility(snapshot, capacity.Analysis{Classification: capacity.Healthy}, 0.02, readiness.ReadyReplicas); !eligible {
		t.Fatalf("restart-filtered readiness evidence must not disturb learning eligibility when all replicas are Ready: %s", reason)
	}
}

func TestReadinessLeadResolutionUsesOnlyMatchingPersistedRevisionAfterRestart(t *testing.T) {
	observedAt := metav1.NewTime(time.Date(2026, 9, 12, 6, 51, 24, 0, time.UTC))
	status := optiscalev1alpha1.OptiScalerStatus{
		LearnedReadinessLeadTimeSeconds:  float64Pointer(22),
		LearnedReadinessObservedAt:       &observedAt,
		LearnedReadinessTemplateIdentity: "hash-current",
	}
	// A newly constructed controller has no in-memory prediction/readiness history;
	// the matching API status is sufficient to recover the measured lead.
	restartedController := &OptiScalerReconciler{}
	if restartedController.predictionHistories != nil {
		t.Fatal("test setup unexpectedly contains in-memory controller history")
	}
	readiness := targetReadinessObservation{ReadyReplicas: 2, TemplateIdentity: "hash-current"}
	evidence := resolveReadinessLead(readiness, status)
	if !evidence.Available || evidence.Persist || evidence.Source != optiscalev1alpha1.ReadinessEvidencePersistedLearnedSample || evidence.LeadTime != 22*time.Second || evidence.ObservedAt != observedAt.Time {
		t.Fatalf("matching persisted readiness should be reused after controller restart: %+v", evidence)
	}
	if text := readinessEvidenceDescription(evidence); !strings.Contains(text, "readiness lead 22.0s from persisted learned startup evidence for current target revision") {
		t.Fatalf("persisted readiness provenance is not explainable: %q", text)
	}

	readiness.TemplateIdentity = "hash-new"
	evidence = resolveReadinessLead(readiness, status)
	if evidence.Available || evidence.Source != "" || !strings.Contains(evidence.UnavailableReason, `revision "hash-current", not current revision "hash-new"`) {
		t.Fatalf("mismatched revision evidence must be rejected explicitly: %+v", evidence)
	}

	readiness.RevisionError = "current target revision is ambiguous during rollout"
	readiness.TemplateIdentity = "hash-current"
	evidence = resolveReadinessLead(readiness, status)
	if evidence.Available || !strings.Contains(evidence.UnavailableReason, "ambiguous") {
		t.Fatalf("ambiguous current revision must block otherwise matching persisted evidence: %+v", evidence)
	}
}

func TestCurrentTargetTemplateRevisionUsesObservedDeploymentReplicaSetHash(t *testing.T) {
	ctx := context.Background()
	labels := map[string]string{"app": "demo-api"}
	deployment := readinessTestDeployment(labels)
	oldReplicaSet := readinessTestReplicaSet(deployment, "hash-old")
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &OptiScalerReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, oldReplicaSet).Build()}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.currentTargetTemplateRevision(ctx, deployment, selector)
	if err != nil || got.Identity != "hash-old" || got.ReplicaSetUID != oldReplicaSet.UID {
		t.Fatalf("current target revision should come from the matching Deployment-owned ReplicaSet hash: revision=%+v err=%v", got, err)
	}

	deployment.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app", Image: "demo:v2"}}
	if _, err := r.currentTargetTemplateRevision(ctx, deployment, selector); err == nil || !strings.Contains(err.Error(), "no Deployment-owned ReplicaSet matches") {
		t.Fatalf("rollout with no ReplicaSet for the new pod template must be treated as ambiguous: %v", err)
	}
	newReplicaSet := readinessTestReplicaSet(deployment, "hash-new")
	if err := r.Client.Create(ctx, newReplicaSet); err != nil {
		t.Fatal(err)
	}
	got, err = r.currentTargetTemplateRevision(ctx, deployment, selector)
	if err != nil || got.Identity != "hash-new" || got.ReplicaSetUID != newReplicaSet.UID {
		t.Fatalf("new template must resolve to its new Kubernetes hash: revision=%+v err=%v", got, err)
	}

	deployment.Status.ObservedGeneration = deployment.Generation - 1
	if _, err := r.currentTargetTemplateRevision(ctx, deployment, selector); err == nil || !strings.Contains(err.Error(), "generation has not been observed") {
		t.Fatalf("unobserved Deployment generation must fail safely: %v", err)
	}
}

func TestRegularContainerRestartsInvalidateFreshReadinessSampleButInitRestartsDoNot(t *testing.T) {
	pod := readyPod("demo-api-multicontainer", map[string]string{"app": "demo-api"}, time.Now().Add(-time.Minute), time.Now())
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "sidecar", Image: "sidecar:v1"})
	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{Name: "sidecar", RestartCount: 1})
	if regularContainersHaveNeverRestarted(&pod) {
		t.Fatal("restart in any regular container must invalidate the pod's fresh startup sample")
	}
	pod.Status.ContainerStatuses[1].RestartCount = 0
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", RestartCount: 3}}
	if !regularContainersHaveNeverRestarted(&pod) {
		t.Fatal("init-container restart history must not invalidate a regular-container startup sample")
	}
}

func TestEvaluatePredictionBlocksWithoutMatchingReadinessEvidenceButKeepsCapacityLearning(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	labels := map[string]string{"app": "demo-api"}
	deployment := readinessTestDeployment(labels)
	replicaSet := readinessTestReplicaSet(deployment, "hash-current")
	pods := []corev1.Pod{
		readyPodForRevision("demo-api-a", deployment, "hash-current", now.Add(-10*time.Hour), now.Add(-time.Second), 1),
		readyPodForRevision("demo-api-b", deployment, "hash-current", now.Add(-10*time.Hour), now.Add(-time.Second), 1),
	}
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, replicaSet, &pods[0], &pods[1]).Build()
	r := &OptiScalerReconciler{Client: client}
	key := "optiscale-demo/demo-api"
	for i, rate := range []float64{100, 120, 140, 160} {
		r.updatePredictionHistory(key, forecast.Sample{Timestamp: now.Add(-time.Duration(60-15*i) * time.Second), RequestRate: rate}, forecast.CapacitySample{}, now)
	}
	for i, efficiency := range []float64{2, 2.1, 2.2} {
		r.updatePredictionHistory(key, forecast.Sample{}, forecast.CapacitySample{Timestamp: now.Add(-time.Duration(30-10*i) * time.Second), RPSPerWorkUnit: efficiency}, now)
	}
	metric := func(value float64) observation.Metric {
		return observation.Metric{Value: value, ObservedAt: now, Valid: true, Fresh: true}
	}
	resource := &optiscalev1alpha1.OptiScaler{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-api", Namespace: "optiscale-demo"},
		Spec: optiscalev1alpha1.OptiScalerSpec{
			MinReplicas: 1, MaxReplicas: 5,
			Policy:     optiscalev1alpha1.PolicySpec{MaxScaleUpStep: 1},
			Prediction: optiscalev1alpha1.PredictionSpec{Enabled: true, SafeCapacityMargin: 0.75, MaxErrorRate: 0.02, PredictiveDemandWindowSeconds: 30},
		},
		Status: optiscalev1alpha1.OptiScalerStatus{
			LearnedReadinessLeadTimeSeconds:  float64Pointer(22),
			LearnedReadinessObservedAt:       &metav1.Time{Time: now.Add(-time.Hour)},
			LearnedReadinessTemplateIdentity: "hash-old",
		},
	}
	snapshot := observation.Snapshot{Target: observation.TargetObservation{
		CurrentReplicas: 2, ReadyReplicas: 2, P95Latency: metric(100), AggregateSlotOccupancy: metric(13.8),
		HottestReplicaOccupancy: metric(8), EffectiveServingReplicas: metric(2), PhysicalConcurrencyLimit: 20, SafeOperatingOccupancy: 15,
		RequestRates:          observation.RequestRates{RequestRate: metric(170), SuccessfulRequestRate: metric(169), ErrorRate: metric(1.0 / 170.0)},
		PredictiveRequestRate: metric(175),
	}}
	current := policy.CapacityDecision{Action: policy.ActionHold, ChosenComponent: "target", CurrentReplicas: 2, DesiredReplicas: 2}
	decision, assessment := r.evaluatePrediction(context.Background(), resource, snapshot, capacity.Analysis{Classification: capacity.Healthy}, current, deployment, now)
	if decision.Action != policy.ActionHold || assessment.hasReadiness || assessment.planningHorizon != 0 || !strings.Contains(assessment.rejectedReason, "persisted readiness evidence is for target revision") {
		t.Fatalf("restarted pods and mismatched learned revision must block prescaling: decision=%+v assessment=%+v", decision, assessment)
	}
	if !assessment.hasCurrentCapacity || len(r.predictionHistories[key].capacityRates) != 4 {
		t.Fatalf("readiness provenance failure should not disrupt healthy capacity learning: assessment=%+v history=%+v", assessment, r.predictionHistories[key].capacityRates)
	}

	resource.Status.LearnedReadinessTemplateIdentity = "hash-current"
	recoveredController := &OptiScalerReconciler{Client: client}
	for i, rate := range []float64{100, 120, 140, 160} {
		recoveredController.updatePredictionHistory(key, forecast.Sample{Timestamp: now.Add(-time.Duration(60-15*i) * time.Second), RequestRate: rate}, forecast.CapacitySample{}, now)
	}
	for i, efficiency := range []float64{2, 2.1, 2.2} {
		recoveredController.updatePredictionHistory(key, forecast.Sample{}, forecast.CapacitySample{Timestamp: now.Add(-time.Duration(30-10*i) * time.Second), RPSPerWorkUnit: efficiency}, now)
	}
	decision, recovered := recoveredController.evaluatePrediction(context.Background(), resource, snapshot, capacity.Analysis{Classification: capacity.Healthy}, current, deployment, now)
	if decision.Action != policy.ActionPrescaleTarget || !recovered.hasReadiness || recovered.readinessEvidenceSource != optiscalev1alpha1.ReadinessEvidencePersistedLearnedSample || recovered.planningHorizon != 52*time.Second {
		t.Fatalf("matching persisted readiness should restore the composed planning horizon when all current pods have restarted: decision=%+v assessment=%+v", decision, recovered)
	}
}

func TestEvaluatePredictionPrescalesWithMeasuredCapacityAndReadiness(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	labels := map[string]string{"app": "demo-api"}
	deployment := readinessTestDeployment(labels)
	replicaSet := readinessTestReplicaSet(deployment, "rev-current")
	readyPods := []corev1.Pod{
		readyPodForRevision("demo-api-a", deployment, "rev-current", now.Add(-40*time.Second), now.Add(-18*time.Second), 0),
		readyPodForRevision("demo-api-b", deployment, "rev-current", now.Add(-32*time.Second), now.Add(-10*time.Second), 0),
	}
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, replicaSet, &readyPods[0], &readyPods[1]).Build()
	r := &OptiScalerReconciler{Client: client}
	key := "optiscale-demo/demo-api"
	for i, rate := range []float64{100, 120, 140, 160} {
		r.updatePredictionHistory(key, forecast.Sample{Timestamp: now.Add(-time.Duration(60-15*i) * time.Second), RequestRate: rate}, forecast.CapacitySample{}, now)
	}
	for i, efficiency := range []float64{5, 5, 5} {
		r.updatePredictionHistory(key, forecast.Sample{}, forecast.CapacitySample{Timestamp: now.Add(-time.Duration(30-10*i) * time.Second), RPSPerWorkUnit: efficiency}, now)
	}

	metric := func(value float64) observation.Metric {
		return observation.Metric{Value: value, ObservedAt: now, Valid: true, Fresh: true}
	}
	snapshot := observation.Snapshot{
		SLOTargetP95Milliseconds: 250,
		Target: observation.TargetObservation{
			Name: "demo-api", CurrentReplicas: 2, ReadyReplicas: 2, P95Latency: metric(100), AggregateSlotOccupancy: metric(13.8), HottestReplicaOccupancy: metric(8), EffectiveServingReplicas: metric(2), PhysicalConcurrencyLimit: 20, SafeOperatingOccupancy: 15,
			RequestRates:          observation.RequestRates{RequestRate: metric(70), SuccessfulRequestRate: metric(69), ErrorRate: metric(1.0 / 70.0)},
			PredictiveRequestRate: metric(175),
		},
	}
	resource := &optiscalev1alpha1.OptiScaler{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-api", Namespace: "optiscale-demo"},
		Spec: optiscalev1alpha1.OptiScalerSpec{
			MinReplicas: 1, MaxReplicas: 5,
			Policy:     optiscalev1alpha1.PolicySpec{MaxScaleUpStep: 1, CooldownSeconds: 30},
			Prediction: optiscalev1alpha1.PredictionSpec{Enabled: true, SafeCapacityMargin: 0.75, MaxErrorRate: 0.02, PredictiveDemandWindowSeconds: 30},
		},
	}
	baseDecision := policy.CapacityDecision{Action: policy.ActionHold, Reason: "target SLO is currently satisfied", ChosenComponent: "target", CurrentReplicas: 2, DesiredReplicas: 2}
	got, assessment := r.evaluatePrediction(context.Background(), resource, snapshot, capacity.Analysis{Classification: capacity.Healthy}, baseDecision, deployment, now)
	if got.Action != policy.ActionPrescaleTarget || got.CurrentReplicas != 2 || got.DesiredReplicas != 3 {
		t.Fatalf("expected bounded proactive scale from 2 to 3, got %+v", got)
	}
	if !assessment.accepted || assessment.trend.Quality != forecast.QualityHigh || assessment.readinessLeadTime != 22*time.Second || assessment.controlLoopAllowance != 15*time.Second || assessment.demandWindow != 30*time.Second || assessment.demandObservationLag != 15*time.Second || assessment.planningHorizon != 52*time.Second || !assessment.hasCurrentCapacity {
		t.Fatalf("forecast evidence incomplete: %+v", assessment)
	}
	history := r.predictionHistories[key].demandRates
	if history[len(history)-1].RequestRate != snapshot.Target.PredictiveRequestRate.Value || history[len(history)-1].RequestRate == snapshot.Target.RequestRates.RequestRate.Value {
		t.Fatalf("forecast history must use the fast predictive-demand metric, not stable request rate: %+v", history)
	}
	wantForecast := forecast.LinearForecast(history, now, 52*time.Second)
	controlLoopOnlyForecast := forecast.LinearForecast(history, now, 37*time.Second)
	readinessOnlyForecast := forecast.LinearForecast(history, now, 22*time.Second)
	if math.Abs(assessment.trend.RequestRate-wantForecast.RequestRate) > 1e-9 || math.Abs(assessment.trend.RequestRate-controlLoopOnlyForecast.RequestRate) < 1e-9 || math.Abs(assessment.trend.RequestRate-readinessOnlyForecast.RequestRate) < 1e-9 {
		t.Fatalf("forecast must use full 52s planning horizon: got %.3f, 52s=%.3f, 37s=%.3f, readiness-only 22s=%.3f", assessment.trend.RequestRate, wantForecast.RequestRate, controlLoopOnlyForecast.RequestRate, readinessOnlyForecast.RequestRate)
	}
	if !strings.Contains(got.Reason, "within 52.0s planning horizon (22.0s measured Pod creation-to-Ready + 15.0s control-loop allowance + 15.0s predictive-demand observation lag; readiness lead 22.0s from CURRENT_FRESH_SAMPLE for current target revision \"rev-current\"") || strings.Contains(got.Reason, "within 37.0s planning horizon") {
		t.Fatalf("accepted decision reason does not explain composed horizon: %q", got.Reason)
	}
	if assessment.safePerReplica != 75 || assessment.currentSafeCapacity != 150 {
		t.Fatalf("capacity should extrapolate to 20 physical slots, apply 0.75 once, and credit two serving replicas: %+v", assessment)
	}
	evidence := strings.Join(predictionEvidence(snapshot, assessment, 0.02), " | ")
	for _, want := range []string{"predictive demand rate 175.00 requests/s from 30s rate window", "conservative throughput efficiency 5.00", "physical limit 20.00 slots", "safe operating occupancy 15.00 slots", "estimated physical capacity 100.00 RPS", "safe capacity 75.00 RPS per serving replica", "effective serving replicas 2", "realized current safe capacity 150.00 requests/s", "52.0s planning horizon = 22.0s measured Pod creation-to-Ready + 15.0s control-loop allowance + 15.0s predictive-demand observation lag (half of 30s rate window)"} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("capacity evidence %q does not contain %q", evidence, want)
		}
	}
	capacityHistory := r.predictionHistories[key].capacityRates
	if len(capacityHistory) != 4 || math.Abs(capacityHistory[len(capacityHistory)-1].RPSPerWorkUnit-5) > 1e-9 {
		t.Fatalf("capacity sample should be successful RPS / aggregate held-slot occupancy = 69 / 13.8 = 5, got %+v", capacityHistory)
	}
	if err := r.Client.Create(context.Background(), func() *corev1.Pod {
		pod := readyPodForRevision("demo-api-c", deployment, "rev-current", now.Add(-35*time.Second), now.Add(-10*time.Second), 0)
		return &pod
	}()); err != nil {
		t.Fatal(err)
	}
	snapshot.Target.CurrentReplicas = 3
	snapshot.Target.ReadyReplicas = 3
	snapshot.Target.AggregateSlotOccupancy = metric(13.8)
	snapshot.Target.HottestReplicaOccupancy = metric(6.9)
	threeReplicaDecision := policy.CapacityDecision{Action: policy.ActionHold, Reason: "healthy", ChosenComponent: "target", CurrentReplicas: 3, DesiredReplicas: 3}
	snapshot.Target.EffectiveServingReplicas = metric(2)
	idleReadyDecision, idleReadyAssessment := r.evaluatePrediction(context.Background(), resource, snapshot, capacity.Analysis{Classification: capacity.Healthy}, threeReplicaDecision, deployment, now)
	if idleReadyDecision.Action != policy.ActionHold || idleReadyAssessment.accepted || !idleReadyAssessment.hasCurrentCapacity || idleReadyAssessment.currentSafeCapacity != 150 || !strings.Contains(idleReadyAssessment.rejectedReason, "3 Kubernetes Ready replicas but only 2 are traffic-bearing") {
		t.Fatalf("Ready-but-idle capacity must not receive credit or permit another prescale: decision=%+v assessment=%+v", idleReadyDecision, idleReadyAssessment)
	}
	snapshot.Target.EffectiveServingReplicas = metric(3)
	resumedDecision, threeReplicaAssessment := r.evaluatePrediction(context.Background(), resource, snapshot, capacity.Analysis{Classification: capacity.Healthy}, threeReplicaDecision, deployment, now)
	if !threeReplicaAssessment.hasCurrentCapacity || threeReplicaAssessment.currentSafeCapacity != 225 || !threeReplicaAssessment.accepted || resumedDecision.Action != policy.ActionPrescaleTarget || strings.Contains(threeReplicaAssessment.rejectedReason, "not yet been realized") {
		t.Fatalf("current safe capacity should scale with three traffic-bearing replicas: %+v", threeReplicaAssessment)
	}
	snapshot.Dependencies = []observation.DependencyObservation{{Name: "inventory", P95Latency: observation.Metric{Value: 300, ObservedAt: now, Valid: true, Fresh: true}, Utilization: observation.Metric{Value: 20, ObservedAt: now, Valid: true, Fresh: true}, LatencyThreshold: 250, UtilizationThreshold: 100}}
	blocked, rejected := r.evaluatePrediction(context.Background(), resource, snapshot, capacity.Analysis{Classification: capacity.Healthy}, baseDecision, deployment, now)
	if blocked.Action != policy.ActionHold || rejected.accepted || !strings.Contains(rejected.rejectedReason, "inventory") {
		t.Fatalf("forecast overrode saturated dependency: decision=%+v assessment=%+v", blocked, rejected)
	}

	demandHistoryCount := len(r.predictionHistories[key].demandRates)
	capacityHistoryCount := len(r.predictionHistories[key].capacityRates)
	for _, test := range []struct {
		name   string
		metric observation.Metric
		offset time.Duration
	}{
		{name: "missing", metric: observation.Metric{Error: "Prometheus returned no predictive-demand series"}, offset: 15 * time.Second},
		{name: "stale", metric: observation.Metric{Value: 80, ObservedAt: now.Add(-3 * time.Minute), Valid: true, Fresh: false, Error: "predictive-demand telemetry is stale"}, offset: 30 * time.Second},
	} {
		t.Run("unavailable predictive demand blocks prescale but not capacity learning/"+test.name, func(t *testing.T) {
			at := now.Add(test.offset)
			missingSnapshot := snapshot
			missingSnapshot.Dependencies = nil
			missingSnapshot.Target.PredictiveRequestRate = test.metric
			missingSnapshot.Target.RequestRates.RequestRate.ObservedAt = at
			missingSnapshot.Target.RequestRates.SuccessfulRequestRate.ObservedAt = at
			missingSnapshot.Target.RequestRates.ErrorRate.ObservedAt = at
			decision, assessment := r.evaluatePrediction(context.Background(), resource, missingSnapshot, capacity.Analysis{Classification: capacity.Healthy}, policy.CapacityDecision{Action: policy.ActionHold, CurrentReplicas: 3, DesiredReplicas: 3}, deployment, at)
			if decision.Action != policy.ActionHold || assessment.accepted || !strings.Contains(assessment.rejectedReason, "predictive-demand telemetry unavailable") {
				t.Fatalf("%s predictive metric did not block prescaling: decision=%+v assessment=%+v", test.name, decision, assessment)
			}
			if len(r.predictionHistories[key].demandRates) != demandHistoryCount {
				t.Fatalf("%s predictive metric was appended to forecast history: %+v", test.name, r.predictionHistories[key].demandRates)
			}
			if len(r.predictionHistories[key].capacityRates) != capacityHistoryCount+1 || math.Abs(r.predictionHistories[key].capacityRates[len(r.predictionHistories[key].capacityRates)-1].RPSPerWorkUnit-5) > 1e-9 {
				t.Fatalf("%s predictive metric incorrectly blocked stable successful-RPS capacity learning: %+v", test.name, r.predictionHistories[key].capacityRates)
			}
			capacityHistoryCount++
		})
	}
}

func TestEvaluatePredictionRejectsUnavailableMeasuredReadiness(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	labels := map[string]string{"app": "demo-api"}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-api", Namespace: "optiscale-demo"},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
	}
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &OptiScalerReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()}
	key := "optiscale-demo/demo-api"
	for i, rate := range []float64{10, 25, 40, 55} {
		r.updatePredictionHistory(key, forecast.Sample{Timestamp: now.Add(-time.Duration(60-15*i) * time.Second), RequestRate: rate}, forecast.CapacitySample{}, now)
	}
	for i, efficiency := range []float64{2, 2.2, 2.1} {
		r.updatePredictionHistory(key, forecast.Sample{}, forecast.CapacitySample{Timestamp: now.Add(-time.Duration(30-10*i) * time.Second), RPSPerWorkUnit: efficiency}, now)
	}
	metric := func(value float64) observation.Metric {
		return observation.Metric{Value: value, ObservedAt: now, Valid: true, Fresh: true}
	}
	snapshot := observation.Snapshot{Target: observation.TargetObservation{
		Name: "demo-api", CurrentReplicas: 2, ReadyReplicas: 0, P95Latency: metric(100), AggregateSlotOccupancy: metric(13.8),
		HottestReplicaOccupancy: metric(6.9), EffectiveServingReplicas: metric(0), PhysicalConcurrencyLimit: 20, SafeOperatingOccupancy: 15,
		RequestRates:          observation.RequestRates{RequestRate: metric(70), SuccessfulRequestRate: metric(69), ErrorRate: metric(1.0 / 70.0)},
		PredictiveRequestRate: metric(70),
	}}
	resource := &optiscalev1alpha1.OptiScaler{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-api", Namespace: "optiscale-demo"},
		Spec: optiscalev1alpha1.OptiScalerSpec{
			MinReplicas: 1, MaxReplicas: 5,
			Policy:     optiscalev1alpha1.PolicySpec{MaxScaleUpStep: 1},
			Prediction: optiscalev1alpha1.PredictionSpec{Enabled: true, SafeCapacityMargin: 0.75, MaxErrorRate: 0.02, PredictiveDemandWindowSeconds: 30},
		},
	}
	current := policy.CapacityDecision{Action: policy.ActionHold, ChosenComponent: "target", CurrentReplicas: 2, DesiredReplicas: 2}
	got, assessment := r.evaluatePrediction(context.Background(), resource, snapshot, capacity.Analysis{Classification: capacity.Healthy}, current, deployment, now)
	if got.Action != policy.ActionHold || got.DesiredReplicas != 2 || assessment.accepted || assessment.hasReadiness || assessment.planningHorizon != 0 {
		t.Fatalf("missing measured readiness must preserve HOLD with no planning horizon: decision=%+v assessment=%+v", got, assessment)
	}
	if !strings.Contains(assessment.rejectedReason, "readiness lead time unavailable") {
		t.Fatalf("missing-readiness rejection should be explicit, got %q", assessment.rejectedReason)
	}
}

func readyPod(name string, labels map[string]string, created, readyAt time.Time) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "optiscale-demo", Labels: labels, CreationTimestamp: metav1.NewTime(created)},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "demo"}}},
		Status: corev1.PodStatus{
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(readyAt)}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", RestartCount: 0}},
		},
	}
}

func readinessTestDeployment(labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-api", Namespace: "optiscale-demo", UID: types.UID("deployment-demo-api"), Generation: 3},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: copyLabels(labels)}},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 3},
	}
}

func readinessTestReplicaSet(deployment *appsv1.Deployment, revision string) *appsv1.ReplicaSet {
	labels := copyLabels(deployment.Spec.Template.Labels)
	labels[podTemplateHashLabel] = revision
	template := *deployment.Spec.Template.DeepCopy()
	template.Labels = labels
	controller := true
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo-api-" + revision, Namespace: deployment.Namespace,
			UID: types.UID("replicaset-" + revision), Labels: copyLabels(labels),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, UID: deployment.UID, Controller: &controller}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: template},
	}
}

func readyPodForRevision(name string, deployment *appsv1.Deployment, revision string, created, readyAt time.Time, restartCount int32) corev1.Pod {
	labels := copyLabels(deployment.Spec.Template.Labels)
	labels[podTemplateHashLabel] = revision
	pod := readyPod(name, labels, created, readyAt)
	pod.Status.ContainerStatuses[0].RestartCount = restartCount
	controller := true
	pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "demo-api-" + revision, UID: types.UID("replicaset-" + revision), Controller: &controller}}
	return pod
}

func copyLabels(labels map[string]string) map[string]string {
	copy := make(map[string]string, len(labels))
	for key, value := range labels {
		copy[key] = value
	}
	return copy
}
