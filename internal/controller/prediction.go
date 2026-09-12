package controller

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	optiscalev1alpha1 "github.com/RajRaghupatruni/Silver-Leaf/api/v1alpha1"
	"github.com/RajRaghupatruni/Silver-Leaf/internal/capacity"
	"github.com/RajRaghupatruni/Silver-Leaf/internal/forecast"
	"github.com/RajRaghupatruni/Silver-Leaf/internal/observation"
	"github.com/RajRaghupatruni/Silver-Leaf/internal/policy"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	capacityLearningUtilizationFraction = 0.3
	defaultSafeCapacityMargin           = 0.75
	maxPredictionHistories              = 128
	podTemplateHashLabel                = "pod-template-hash"
)

type predictionHistory struct {
	demandRates   []forecast.Sample
	capacityRates []forecast.CapacitySample
	lastUpdated   time.Time
}

type predictionAssessment struct {
	trend                      forecast.Result
	capacityEstimate           forecast.CapacityEstimate
	safeCapacityMargin         float64
	readinessLeadTime          time.Duration
	readinessEvidenceSource    optiscalev1alpha1.ReadinessEvidenceSource
	readinessTemplateIdentity  string
	readinessObservedAt        time.Time
	readinessSampleCount       int32
	readinessPersist           bool
	readinessUnavailableReason string
	controlLoopAllowance       time.Duration
	demandWindow               time.Duration
	demandObservationLag       time.Duration
	planningHorizon            time.Duration
	hasReadiness               bool
	hasDemandWindow            bool
	predictiveDemandValid      bool
	readyReplicas              int32
	effectiveServingReplicas   int32
	safePerReplica             float64
	hasSafeCapacity            bool
	currentSafeCapacity        float64
	hasCurrentCapacity         bool
	accepted                   bool
	rejectedReason             string
	evidence                   []string
}

type targetReadinessObservation struct {
	ReadyReplicas    int32
	TemplateIdentity string
	RevisionError    string
	FreshLeadTime    time.Duration
	FreshObservedAt  time.Time
	FreshSampleCount int32
	HasFreshSample   bool
}

type readinessLeadEvidence struct {
	Available         bool
	LeadTime          time.Duration
	Source            optiscalev1alpha1.ReadinessEvidenceSource
	TemplateIdentity  string
	ObservedAt        time.Time
	FreshSampleCount  int32
	Persist           bool
	UnavailableReason string
}

type targetTemplateRevision struct {
	Identity      string
	ReplicaSetUID types.UID
}

func (r *OptiScalerReconciler) evaluatePrediction(
	ctx context.Context,
	resource *optiscalev1alpha1.OptiScaler,
	snapshot observation.Snapshot,
	analysis capacity.Analysis,
	current policy.CapacityDecision,
	deployment *appsv1.Deployment,
	now time.Time,
) (policy.CapacityDecision, predictionAssessment) {
	assessment := predictionAssessment{}
	if !resource.Spec.Prediction.Enabled {
		return current, assessment
	}
	assessment.safeCapacityMargin = resource.Spec.Prediction.SafeCapacityMargin

	var demandSample forecast.Sample
	assessment.predictiveDemandValid = metricUsable(snapshot.Target.PredictiveRequestRate)
	predictiveDemandRejection := ""
	if assessment.predictiveDemandValid {
		demandSample = forecast.Sample{Timestamp: snapshot.Target.PredictiveRequestRate.ObservedAt, RequestRate: snapshot.Target.PredictiveRequestRate.Value}
	} else {
		demandReason := snapshot.Target.PredictiveRequestRate.Error
		if demandReason == "" {
			demandReason = "fast predictive-demand metric is missing, stale, or non-finite"
		}
		predictiveDemandRejection = "predictive-demand telemetry unavailable: " + demandReason
	}
	windowSeconds := resource.Spec.Prediction.PredictiveDemandWindowSeconds
	if windowSeconds >= minimumPredictiveDemandWindowSeconds && windowSeconds <= maximumPredictiveDemandWindowSeconds {
		assessment.demandWindow = time.Duration(windowSeconds) * time.Second
		assessment.demandObservationLag = assessment.demandWindow / 2
		assessment.hasDemandWindow = assessment.demandObservationLag > 0
	}
	readiness, readinessErr := r.targetReadiness(ctx, deployment)
	readyReplicas := readiness.ReadyReplicas
	assessment.readyReplicas = readyReplicas
	effectiveServingReplicas, servingErr := observation.EffectiveReplicaCount(snapshot.Target.EffectiveServingReplicas, readyReplicas)
	if servingErr == nil {
		assessment.effectiveServingReplicas = effectiveServingReplicas
	}
	assessment.controlLoopAllowance = reconcileInterval
	readinessEvidence := readinessLeadEvidence{TemplateIdentity: readiness.TemplateIdentity}
	if readinessErr == nil {
		readinessEvidence = resolveReadinessLead(readiness, resource.Status)
	} else {
		readinessEvidence.UnavailableReason = readinessErr.Error()
	}
	assessment.readinessLeadTime = readinessEvidence.LeadTime
	assessment.readinessEvidenceSource = readinessEvidence.Source
	assessment.readinessTemplateIdentity = readinessEvidence.TemplateIdentity
	assessment.readinessObservedAt = readinessEvidence.ObservedAt
	assessment.readinessSampleCount = readinessEvidence.FreshSampleCount
	assessment.readinessPersist = readinessEvidence.Persist
	assessment.readinessUnavailableReason = readinessEvidence.UnavailableReason
	assessment.hasReadiness = readinessEvidence.Available
	if assessment.hasReadiness && assessment.hasDemandWindow {
		assessment.planningHorizon = assessment.readinessLeadTime + assessment.controlLoopAllowance + assessment.demandObservationLag
	}

	capacityAllowed, capacityRejection := capacitySampleEligibility(snapshot, analysis, resource.Spec.Prediction.MaxErrorRate, readyReplicas)

	var capacitySample forecast.CapacitySample
	if capacityAllowed {
		capacitySample = forecast.CapacitySample{
			Timestamp:      snapshot.Target.RequestRates.SuccessfulRequestRate.ObservedAt,
			RPSPerWorkUnit: snapshot.Target.RequestRates.SuccessfulRequestRate.Value / snapshot.Target.AggregateSlotOccupancy.Value,
		}
	}
	key := resource.Namespace + "/" + resource.Name
	demandHistory, capacityHistory := r.updatePredictionHistory(key, demandSample, capacitySample, now)

	forecastHorizon := assessment.planningHorizon
	if forecastHorizon <= 0 {
		// Trend quality can still be reported without using this fallback forecast for a decision.
		forecastHorizon = time.Second
	}
	assessment.trend = forecast.LinearForecast(demandHistory, now, forecastHorizon)
	if readinessErr != nil {
		capacityRejection = appendReason(capacityRejection, "readiness lead time unavailable: "+readinessErr.Error())
	} else if readyReplicas < 1 {
		capacityRejection = appendReason(capacityRejection, "readiness lead time unavailable: no Ready target pods are available")
	} else if !assessment.hasReadiness {
		capacityRejection = appendReason(capacityRejection, "readiness lead time unavailable: "+assessment.readinessUnavailableReason)
	}
	if !assessment.hasDemandWindow {
		capacityRejection = appendReason(capacityRejection, fmt.Sprintf("predictive-demand window must be configured between %d and %d seconds", minimumPredictiveDemandWindowSeconds, maximumPredictiveDemandWindowSeconds))
	}
	if !assessment.predictiveDemandValid {
		capacityRejection = appendReason(capacityRejection, predictiveDemandRejection)
	}

	if estimate, ok := forecast.SafePerReplicaCapacity(capacityHistory, snapshot.Target.PhysicalConcurrencyLimit, resource.Spec.Prediction.SafeCapacityMargin); ok {
		assessment.capacityEstimate = estimate
		assessment.safePerReplica = estimate.SafePerReplicaCapacity
		assessment.hasSafeCapacity = true
		assessment.currentSafeCapacity = assessment.safePerReplica * float64(effectiveServingReplicas)
		assessment.hasCurrentCapacity = servingErr == nil && finiteNonnegative(assessment.currentSafeCapacity)
	} else {
		capacityRejection = appendReason(capacityRejection, fmt.Sprintf("insufficient healthy loaded capacity evidence: need at least %d valid samples", forecast.MinimumCapacitySamples))
	}

	trendRejection := ""
	if assessment.trend.Quality != forecast.QualityHigh || !assessment.trend.HasRequestRate || !assessment.trend.HasTrend || !assessment.trend.HasFit {
		trendRejection = assessment.trend.Reason
	}
	if trendRejection != "" {
		capacityRejection = appendReason(capacityRejection, trendRejection)
	}
	if !assessment.hasReadiness {
		capacityRejection = appendReason(capacityRejection, "planning horizon requires valid current or matching persisted readiness evidence")
	}

	dependenciesAreHealthy, _ := dependenciesHealthy(snapshot.Dependencies)
	requestTelemetryValid := metricUsable(snapshot.Target.RequestRates.RequestRate) && metricUsable(snapshot.Target.RequestRates.SuccessfulRequestRate) && metricUsable(snapshot.Target.RequestRates.ErrorRate)
	errorRateHealthy := metricUsable(snapshot.Target.RequestRates.ErrorRate) && snapshot.Target.RequestRates.ErrorRate.Value <= resource.Spec.Prediction.MaxErrorRate
	targetUnsaturated := metricUsable(snapshot.Target.HottestReplicaOccupancy) && finiteNonnegative(snapshot.Target.SafeOperatingOccupancy) && snapshot.Target.HottestReplicaOccupancy.Value < snapshot.Target.SafeOperatingOccupancy
	capacityEvidenceValid := assessment.hasReadiness && assessment.hasSafeCapacity && assessment.hasCurrentCapacity && servingErr == nil && effectiveServingReplicas > 0 && effectiveServingReplicas == readyReplicas && readyReplicas == snapshot.Target.ReadyReplicas && readyReplicas == snapshot.Target.CurrentReplicas
	if servingErr == nil && readyReplicas > effectiveServingReplicas {
		capacityRejection = appendReason(capacityRejection, targetCapacityRealizationReason(snapshot.Target))
	}
	lastScale := (*time.Time)(nil)
	if resource.Status.LastScaleTime != nil {
		value := resource.Status.LastScaleTime.Time
		lastScale = &value
	}
	decision := policy.EvaluatePrescale(policy.PrescaleInput{
		Current: current, Classification: analysis.Classification,
		RequestTelemetryValid: requestTelemetryValid, ErrorRateHealthy: errorRateHealthy,
		PredictiveDemandValid: assessment.predictiveDemandValid,
		DependenciesHealthy:   dependenciesAreHealthy, TargetUnsaturated: targetUnsaturated,
		CapacityEvidenceValid: capacityEvidenceValid, RejectionReason: capacityRejection,
		ForecastQuality: string(assessment.trend.Quality), ForecastRequestRate: assessment.trend.RequestRate,
		RequestRateSlope: assessment.trend.Slope, SafeCapacity: assessment.currentSafeCapacity, Horizon: assessment.planningHorizon,
		Target:         policy.ComponentInput{Name: "target", CurrentReplicas: current.CurrentReplicas, MinReplicas: resource.Spec.MinReplicas, MaxReplicas: resource.Spec.MaxReplicas, Scalable: true},
		MaxScaleUpStep: resource.Spec.Policy.MaxScaleUpStep,
		Cooldown:       time.Duration(resource.Spec.Policy.CooldownSeconds) * time.Second,
		LastScaleTime:  lastScale, Now: now,
	})
	assessment.accepted = decision.Action == policy.ActionPrescaleTarget
	if assessment.accepted {
		decision.Reason = fmt.Sprintf("%s (%.1fs measured Pod creation-to-Ready + %.1fs control-loop allowance + %.1fs predictive-demand observation lag; %s)", decision.Reason, assessment.readinessLeadTime.Seconds(), assessment.controlLoopAllowance.Seconds(), assessment.demandObservationLag.Seconds(), readinessEvidenceDescription(readinessEvidence))
	} else if current.Action != policy.ActionHold {
		assessment.rejectedReason = fmt.Sprintf("higher-priority %s decision retained", current.Action)
	} else if analysis.Classification != capacity.Healthy {
		assessment.rejectedReason = fmt.Sprintf("current capacity classification is %s", analysis.Classification)
	} else {
		assessment.rejectedReason = strings.TrimPrefix(decision.Reason, "prediction rejected: ")
		if assessment.rejectedReason == "" {
			assessment.rejectedReason = capacityRejection
		}
	}
	assessment.evidence = predictionEvidence(snapshot, assessment, resource.Spec.Prediction.MaxErrorRate)
	return decision, assessment
}

func (r *OptiScalerReconciler) updatePredictionHistory(key string, demand forecast.Sample, capacitySample forecast.CapacitySample, now time.Time) ([]forecast.Sample, []forecast.CapacitySample) {
	r.predictionMu.Lock()
	defer r.predictionMu.Unlock()
	if r.predictionHistories == nil {
		r.predictionHistories = make(map[string]predictionHistory)
	}
	history := r.predictionHistories[key]
	history.demandRates = forecast.AppendSample(history.demandRates, demand, now)
	history.capacityRates = forecast.AppendCapacitySample(history.capacityRates, capacitySample, now)
	history.lastUpdated = now
	for existingKey, existing := range r.predictionHistories {
		if existing.lastUpdated.Before(now.Add(-10 * time.Minute)) {
			delete(r.predictionHistories, existingKey)
		}
	}
	if _, exists := r.predictionHistories[key]; !exists && len(r.predictionHistories) >= maxPredictionHistories {
		oldestKey := ""
		var oldest time.Time
		for existingKey, existing := range r.predictionHistories {
			if oldestKey == "" || existing.lastUpdated.Before(oldest) || (existing.lastUpdated.Equal(oldest) && existingKey < oldestKey) {
				oldestKey, oldest = existingKey, existing.lastUpdated
			}
		}
		delete(r.predictionHistories, oldestKey)
	}
	r.predictionHistories[key] = history
	return append([]forecast.Sample(nil), history.demandRates...), append([]forecast.CapacitySample(nil), history.capacityRates...)
}

func (r *OptiScalerReconciler) targetReadiness(ctx context.Context, deployment *appsv1.Deployment) (targetReadinessObservation, error) {
	if deployment.Spec.Selector == nil {
		return targetReadinessObservation{}, fmt.Errorf("target Deployment has no label selector")
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil || selector.Empty() {
		return targetReadinessObservation{}, fmt.Errorf("target Deployment has no valid label selector")
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(deployment.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return targetReadinessObservation{}, fmt.Errorf("list target pods: %w", err)
	}
	readiness := targetReadinessObservation{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		if podIsReady(pod) {
			readiness.ReadyReplicas++
		}
	}

	revision, revisionErr := r.currentTargetTemplateRevision(ctx, deployment, selector)
	if revisionErr != nil {
		readiness.RevisionError = revisionErr.Error()
		return readiness, nil
	}
	readiness.TemplateIdentity = revision.Identity
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || !podIsReady(pod) || pod.Labels[podTemplateHashLabel] != revision.Identity || !podOwnedByReplicaSet(pod, revision.ReplicaSetUID) {
			continue
		}
		readyAt, hasReadyTimestamp := podReadyTransition(pod)
		if !hasReadyTimestamp || pod.CreationTimestamp.IsZero() || readyAt.Before(pod.CreationTimestamp.Time) || !regularContainersHaveNeverRestarted(pod) {
			continue
		}
		lead := readyAt.Sub(pod.CreationTimestamp.Time)
		readiness.FreshSampleCount++
		if !readiness.HasFreshSample || lead > readiness.FreshLeadTime || (lead == readiness.FreshLeadTime && readyAt.After(readiness.FreshObservedAt)) {
			readiness.FreshLeadTime = lead
			readiness.FreshObservedAt = readyAt
			readiness.HasFreshSample = true
		}
	}
	return readiness, nil
}

func (r *OptiScalerReconciler) currentTargetTemplateRevision(ctx context.Context, deployment *appsv1.Deployment, selector labels.Selector) (targetTemplateRevision, error) {
	if deployment.UID == "" || deployment.Generation == 0 || deployment.Status.ObservedGeneration < deployment.Generation {
		return targetTemplateRevision{}, fmt.Errorf("Deployment generation has not been observed; current target revision is ambiguous")
	}
	var replicaSets appsv1.ReplicaSetList
	if err := r.List(ctx, &replicaSets, client.InNamespace(deployment.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return targetTemplateRevision{}, fmt.Errorf("list target ReplicaSets: %w", err)
	}
	var match *appsv1.ReplicaSet
	for i := range replicaSets.Items {
		replicaSet := &replicaSets.Items[i]
		owner := metav1.GetControllerOf(replicaSet)
		if owner == nil || owner.Kind != "Deployment" || owner.Name != deployment.Name || owner.UID != deployment.UID {
			continue
		}
		if !replicaSetMatchesDeploymentTemplate(deployment, replicaSet) {
			continue
		}
		hash := replicaSet.Labels[podTemplateHashLabel]
		if hash == "" || replicaSet.Spec.Template.Labels[podTemplateHashLabel] != hash {
			return targetTemplateRevision{}, fmt.Errorf("current Deployment ReplicaSet has no consistent %s identity", podTemplateHashLabel)
		}
		if match != nil && (match.Labels[podTemplateHashLabel] != hash || match.UID != replicaSet.UID) {
			return targetTemplateRevision{}, fmt.Errorf("multiple Deployment ReplicaSets match the current pod template; target revision is ambiguous")
		}
		match = replicaSet
	}
	if match == nil {
		return targetTemplateRevision{}, fmt.Errorf("no Deployment-owned ReplicaSet matches the current pod template")
	}
	return targetTemplateRevision{Identity: match.Labels[podTemplateHashLabel], ReplicaSetUID: match.UID}, nil
}

func replicaSetMatchesDeploymentTemplate(deployment *appsv1.Deployment, replicaSet *appsv1.ReplicaSet) bool {
	desired := deployment.Spec.Template.DeepCopy()
	actual := replicaSet.Spec.Template.DeepCopy()
	delete(desired.Labels, podTemplateHashLabel)
	delete(actual.Labels, podTemplateHashLabel)
	return apiequality.Semantic.DeepEqual(desired, actual)
}

func podIsReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func podReadyTransition(pod *corev1.Pod) (time.Time, bool) {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue && !condition.LastTransitionTime.IsZero() {
			return condition.LastTransitionTime.Time, true
		}
	}
	return time.Time{}, false
}

func regularContainersHaveNeverRestarted(pod *corev1.Pod) bool {
	if len(pod.Spec.Containers) == 0 {
		return false
	}
	statuses := make(map[string]corev1.ContainerStatus, len(pod.Status.ContainerStatuses))
	for _, status := range pod.Status.ContainerStatuses {
		statuses[status.Name] = status
	}
	for _, container := range pod.Spec.Containers {
		status, found := statuses[container.Name]
		if !found || status.RestartCount != 0 {
			return false
		}
	}
	return true
}

func podOwnedByReplicaSet(pod *corev1.Pod, replicaSetUID types.UID) bool {
	owner := metav1.GetControllerOf(pod)
	return owner != nil && owner.Kind == "ReplicaSet" && owner.UID == replicaSetUID
}

func resolveReadinessLead(readiness targetReadinessObservation, status optiscalev1alpha1.OptiScalerStatus) readinessLeadEvidence {
	evidence := readinessLeadEvidence{
		TemplateIdentity: readiness.TemplateIdentity,
		FreshSampleCount: readiness.FreshSampleCount,
	}
	if readiness.ReadyReplicas <= 0 {
		evidence.UnavailableReason = "no Ready target pods are available"
		return evidence
	}
	if readiness.RevisionError != "" || readiness.TemplateIdentity == "" {
		evidence.UnavailableReason = readiness.RevisionError
		if evidence.UnavailableReason == "" {
			evidence.UnavailableReason = "current target revision identity is unavailable"
		}
		return evidence
	}
	if readiness.HasFreshSample && readiness.FreshLeadTime >= 0 && !readiness.FreshObservedAt.IsZero() {
		evidence.Available = true
		evidence.LeadTime = readiness.FreshLeadTime
		evidence.Source = optiscalev1alpha1.ReadinessEvidenceCurrentFreshSample
		evidence.ObservedAt = readiness.FreshObservedAt
		evidence.Persist = true
		return evidence
	}
	if status.LearnedReadinessLeadTimeSeconds != nil && status.LearnedReadinessObservedAt != nil &&
		!status.LearnedReadinessObservedAt.IsZero() && status.LearnedReadinessTemplateIdentity == readiness.TemplateIdentity &&
		finiteNonnegative(*status.LearnedReadinessLeadTimeSeconds) {
		evidence.Available = true
		evidence.LeadTime = time.Duration(*status.LearnedReadinessLeadTimeSeconds * float64(time.Second))
		evidence.Source = optiscalev1alpha1.ReadinessEvidencePersistedLearnedSample
		evidence.ObservedAt = status.LearnedReadinessObservedAt.Time
		return evidence
	}
	if status.LearnedReadinessTemplateIdentity != "" && status.LearnedReadinessTemplateIdentity != readiness.TemplateIdentity {
		evidence.UnavailableReason = fmt.Sprintf("persisted readiness evidence is for target revision %q, not current revision %q, and no fresh startup sample is available", status.LearnedReadinessTemplateIdentity, readiness.TemplateIdentity)
	} else {
		evidence.UnavailableReason = fmt.Sprintf("no fresh startup sample or matching persisted learned readiness evidence for current target revision %q", readiness.TemplateIdentity)
	}
	return evidence
}

func persistReadinessEvidence(status *optiscalev1alpha1.OptiScalerStatus, evidence readinessLeadEvidence) {
	if status == nil || !evidence.Available || !evidence.Persist || evidence.Source != optiscalev1alpha1.ReadinessEvidenceCurrentFreshSample || evidence.TemplateIdentity == "" || evidence.ObservedAt.IsZero() {
		return
	}
	status.LearnedReadinessLeadTimeSeconds = float64Ptr(evidence.LeadTime.Seconds())
	status.LearnedReadinessObservedAt = timePtr(evidence.ObservedAt)
	status.LearnedReadinessTemplateIdentity = evidence.TemplateIdentity
}

func readinessEvidenceDescription(evidence readinessLeadEvidence) string {
	switch evidence.Source {
	case optiscalev1alpha1.ReadinessEvidenceCurrentFreshSample:
		return fmt.Sprintf("readiness lead %.1fs from CURRENT_FRESH_SAMPLE for current target revision %q (%d eligible Ready pod sample(s))", evidence.LeadTime.Seconds(), evidence.TemplateIdentity, evidence.FreshSampleCount)
	case optiscalev1alpha1.ReadinessEvidencePersistedLearnedSample:
		return fmt.Sprintf("readiness lead %.1fs from persisted learned startup evidence for current target revision %q (PERSISTED_LEARNED_SAMPLE, observed %s)", evidence.LeadTime.Seconds(), evidence.TemplateIdentity, evidence.ObservedAt.UTC().Format(time.RFC3339))
	default:
		return "readiness lead unavailable: " + evidence.UnavailableReason
	}
}

func dependenciesHealthy(dependencies []observation.DependencyObservation) (bool, string) {
	for _, dependency := range dependencies {
		if !metricUsable(dependency.P95Latency) || !metricUsable(dependency.Utilization) {
			return false, fmt.Sprintf("dependency %q telemetry is incomplete", dependency.Name)
		}
		if dependency.P95Latency.Value > dependency.LatencyThreshold || dependency.Utilization.Value > dependency.UtilizationThreshold {
			return false, fmt.Sprintf("dependency %q is saturated; prediction cannot override dependency evidence", dependency.Name)
		}
	}
	return true, ""
}

func capacitySampleEligibility(snapshot observation.Snapshot, analysis capacity.Analysis, maxErrorRate float64, readyReplicas int32) (bool, string) {
	if analysis.Classification != capacity.Healthy {
		return false, fmt.Sprintf("current capacity classification is %s", analysis.Classification)
	}
	if healthy, reason := dependenciesHealthy(snapshot.Dependencies); !healthy {
		return false, reason
	}
	if !metricUsable(snapshot.Target.HottestReplicaOccupancy) || !finite(snapshot.Target.SafeOperatingOccupancy) || snapshot.Target.SafeOperatingOccupancy <= 0 {
		return false, "target hottest-replica slot occupancy or safe operating boundary is unavailable"
	}
	if !metricUsable(snapshot.Target.AggregateSlotOccupancy) || snapshot.Target.AggregateSlotOccupancy.Value <= 0 {
		return false, "positive, fresh aggregate constrained-slot occupancy is required for capacity estimation"
	}
	if snapshot.Target.HottestReplicaOccupancy.Value >= snapshot.Target.SafeOperatingOccupancy {
		return false, "hottest traffic-bearing target replica is at or above its safe operating occupancy boundary"
	}
	if !metricUsable(snapshot.Target.RequestRates.RequestRate) || !metricUsable(snapshot.Target.RequestRates.SuccessfulRequestRate) || !metricUsable(snapshot.Target.RequestRates.ErrorRate) {
		return false, "complete fresh request, successful-request, and error-rate telemetry is required"
	}
	if snapshot.Target.RequestRates.ErrorRate.Value > maxErrorRate {
		return false, fmt.Sprintf("target error rate %.4f exceeds healthy-capacity learning limit %.4f", snapshot.Target.RequestRates.ErrorRate.Value, maxErrorRate)
	}
	if readyReplicas < 1 {
		return false, "target has no Ready replicas for serving-replica capacity measurement"
	}
	if readyReplicas != snapshot.Target.ReadyReplicas || readyReplicas != snapshot.Target.CurrentReplicas {
		return false, fmt.Sprintf("target Deployment is still converging: %d ready of %d requested replicas", readyReplicas, snapshot.Target.CurrentReplicas)
	}
	effectiveReplicas, err := observation.EffectiveReplicaCount(snapshot.Target.EffectiveServingReplicas, readyReplicas)
	if err != nil {
		return false, err.Error()
	}
	if effectiveReplicas == 0 {
		return false, "no traffic-bearing replicas are available for per-replica capacity measurement"
	}
	if effectiveReplicas != readyReplicas {
		return false, targetCapacityRealizationReason(snapshot.Target)
	}
	meanOccupancy := snapshot.Target.AggregateSlotOccupancy.Value / float64(effectiveReplicas)
	if meanOccupancy < snapshot.Target.SafeOperatingOccupancy*capacityLearningUtilizationFraction {
		return false, fmt.Sprintf("mean held-slot occupancy %.2f per serving replica is below the %.0f%% load-evidence floor %.2f", meanOccupancy, capacityLearningUtilizationFraction*100, snapshot.Target.SafeOperatingOccupancy*capacityLearningUtilizationFraction)
	}
	return true, ""
}

func predictionEvidence(snapshot observation.Snapshot, assessment predictionAssessment, maxErrorRate float64) []string {
	evidence := make([]string, 0, 10)
	readinessEvidence := readinessLeadEvidence{
		Available: assessment.hasReadiness, LeadTime: assessment.readinessLeadTime,
		Source: assessment.readinessEvidenceSource, TemplateIdentity: assessment.readinessTemplateIdentity,
		ObservedAt: assessment.readinessObservedAt, FreshSampleCount: assessment.readinessSampleCount,
		UnavailableReason: assessment.readinessUnavailableReason,
	}
	evidence = append(evidence, readinessEvidenceDescription(readinessEvidence))
	if metricUsable(snapshot.Target.RequestRates.RequestRate) {
		evidence = append(evidence, fmt.Sprintf("stable target request rate %.2f requests/s", snapshot.Target.RequestRates.RequestRate.Value))
	}
	if metricUsable(snapshot.Target.PredictiveRequestRate) {
		evidence = append(evidence, fmt.Sprintf("predictive demand rate %.2f requests/s from %ds rate window", snapshot.Target.PredictiveRequestRate.Value, int(assessment.demandWindow.Seconds())))
	} else {
		reason := snapshot.Target.PredictiveRequestRate.Error
		if reason == "" {
			reason = "missing, stale, or non-finite telemetry"
		}
		evidence = append(evidence, "predictive demand rate unavailable: "+reason)
	}
	if assessment.trend.HasTrend {
		evidence = append(evidence, fmt.Sprintf("predictive-demand trend slope %+0.3f requests/s^2 across %s", assessment.trend.Slope, assessment.trend.Span.Round(time.Second)))
	}
	if assessment.trend.HasFit {
		evidence = append(evidence, fmt.Sprintf("forecast quality %s (R^2 %.3f, normalized RMSE %.3f)", assessment.trend.Quality, assessment.trend.R2, assessment.trend.NormalizedRMSE))
	}
	if assessment.controlLoopAllowance > 0 {
		if assessment.hasReadiness && assessment.hasDemandWindow {
			evidence = append(evidence, fmt.Sprintf("%.1fs planning horizon = %.1fs measured Pod creation-to-Ready + %.1fs control-loop allowance + %.1fs predictive-demand observation lag (half of %.0fs rate window)", assessment.planningHorizon.Seconds(), assessment.readinessLeadTime.Seconds(), assessment.controlLoopAllowance.Seconds(), assessment.demandObservationLag.Seconds(), assessment.demandWindow.Seconds()))
		} else if assessment.hasReadiness {
			evidence = append(evidence, fmt.Sprintf("measured Pod creation-to-Ready lead time %.1fs; control-loop allowance %.1fs; predictive-demand window/lag or planning horizon unavailable", assessment.readinessLeadTime.Seconds(), assessment.controlLoopAllowance.Seconds()))
		} else if assessment.hasDemandWindow {
			evidence = append(evidence, fmt.Sprintf("measured Pod creation-to-Ready lead time unavailable; control-loop allowance %.1fs; predictive-demand observation lag %.1fs (half of %.0fs rate window); planning horizon unavailable", assessment.controlLoopAllowance.Seconds(), assessment.demandObservationLag.Seconds(), assessment.demandWindow.Seconds()))
		} else {
			evidence = append(evidence, fmt.Sprintf("measured Pod creation-to-Ready lead time unavailable; control-loop allowance %.1fs; predictive-demand window/lag and planning horizon unavailable", assessment.controlLoopAllowance.Seconds()))
		}
	}
	if assessment.trend.HasRequestRate && assessment.hasReadiness && assessment.hasDemandWindow && assessment.predictiveDemandValid {
		evidence = append(evidence, fmt.Sprintf("forecast demand %.2f requests/s at %.1fs planning horizon", assessment.trend.RequestRate, assessment.planningHorizon.Seconds()))
	}
	if assessment.hasSafeCapacity {
		evidence = append(evidence, fmt.Sprintf("conservative throughput efficiency %.2f successful requests/s per held-slot occupancy unit; physical limit %.2f slots, safe operating occupancy %.2f slots after %.2f margin; estimated physical capacity %.2f RPS and safe capacity %.2f RPS per serving replica", assessment.capacityEstimate.ConservativeRPSPerWorkUnit, snapshot.Target.PhysicalConcurrencyLimit, snapshot.Target.SafeOperatingOccupancy, assessment.safeCapacityMargin, assessment.capacityEstimate.EstimatedPerReplicaCapacity, assessment.safePerReplica))
	}
	if assessment.hasCurrentCapacity {
		evidence = append(evidence, fmt.Sprintf("requested replicas %d; Kubernetes Ready replicas %d; effective serving replicas %d; realized current safe capacity %.2f requests/s = %.2f per serving replica x %d traffic-bearing replicas", snapshot.Target.CurrentReplicas, assessment.readyReplicas, assessment.effectiveServingReplicas, assessment.currentSafeCapacity, assessment.safePerReplica, assessment.effectiveServingReplicas))
	}
	if metricUsable(snapshot.Target.RequestRates.ErrorRate) {
		evidence = append(evidence, fmt.Sprintf("target error ratio %.4f; healthy-capacity learning limit %.4f", snapshot.Target.RequestRates.ErrorRate.Value, maxErrorRate))
	}
	if assessment.accepted {
		evidence = append(evidence, "prediction accepted: forecast demand exceeds current safe capacity within the planning horizon")
	} else if assessment.rejectedReason != "" {
		evidence = append(evidence, "prediction rejected: "+assessment.rejectedReason)
	}
	return evidence
}

func metricUsable(metric observation.Metric) bool {
	return metric.Valid && metric.Fresh && !metric.ObservedAt.IsZero() && finiteNonnegative(metric.Value)
}

func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func appendReason(current, next string) string {
	if next == "" {
		return current
	}
	if current == "" {
		return next
	}
	return current + "; " + next
}

func boolPointer(value bool) *bool { return &value }
func floatPointer(value float64) *float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return &value
}
