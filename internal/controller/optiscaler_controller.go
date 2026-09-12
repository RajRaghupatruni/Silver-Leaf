package controller

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	optiscalev1alpha1 "github.com/RajRaghupatruni/Silver-Leaf/api/v1alpha1"
	"github.com/RajRaghupatruni/Silver-Leaf/internal/capacity"
	"github.com/RajRaghupatruni/Silver-Leaf/internal/decision"
	"github.com/RajRaghupatruni/Silver-Leaf/internal/observation"
	"github.com/RajRaghupatruni/Silver-Leaf/internal/policy"
	promclient "github.com/RajRaghupatruni/Silver-Leaf/internal/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	ConditionReady                             = "Ready"
	ConditionProtected                         = "ProtectedMode"
	reconcileInterval                          = 15 * time.Second
	maxTelemetryAge                            = 2 * time.Minute
	minimumSafeCapacityMargin                  = 0.000001
	minimumPredictiveDemandWindowSeconds int32 = 2
	maximumPredictiveDemandWindowSeconds int32 = 120
)

type OptiScalerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	predictionMu        sync.Mutex
	predictionHistories map[string]predictionHistory
}

type scaleEntry struct {
	deployment *appsv1.Deployment
	scale      autoscalingv1.Scale
	key        types.NamespacedName
}

func (r *OptiScalerReconciler) reconcileCapacity(ctx context.Context, resource *optiscalev1alpha1.OptiScaler, status optiscalev1alpha1.OptiScalerStatus, targetDeployment *appsv1.Deployment, targetScale autoscalingv1.Scale, prom *promclient.Client) (reconcile.Result, error) {
	now := time.Now().UTC()
	targetEntry := scaleEntry{deployment: targetDeployment, scale: targetScale, key: types.NamespacedName{Namespace: targetDeployment.Namespace, Name: targetDeployment.Name}}
	targetCurrent := targetScale.Spec.Replicas
	status.CurrentReplicas = targetCurrent
	status.DesiredReplicas = targetCurrent
	targetDetails := &appsv1.Deployment{}
	if err := r.Get(ctx, targetEntry.key, targetDetails); err != nil {
		return reconcile.Result{}, fmt.Errorf("get target Deployment %s/%s: %w", targetEntry.key.Namespace, targetEntry.key.Name, err)
	}
	aggregateSlotOccupancy := metricObservation(ctx, prom, resource.Spec.Metric.UtilizationQuery, now)
	hottestReplicaOccupancy := metricObservation(ctx, prom, resource.Spec.Metric.HottestReplicaUtilizationQuery, now)
	effectiveServingReplicas := metricObservation(ctx, prom, resource.Spec.Metric.ServingReplicaCountQuery, now)
	if effectiveServingReplicas.Valid && effectiveServingReplicas.Value == 0 &&
		metricUsable(aggregateSlotOccupancy) && aggregateSlotOccupancy.Value == 0 && !hottestReplicaOccupancy.Valid {
		// With no traffic-bearing instances and no held-slot work, an empty max()
		// vector has a well-defined zero hottest occupancy for this observation.
		hottestReplicaOccupancy = observation.Metric{Value: 0, ObservedAt: aggregateSlotOccupancy.ObservedAt, Valid: true, Fresh: aggregateSlotOccupancy.Fresh}
	}
	physicalConcurrencyLimit := resource.Spec.Metric.PhysicalConcurrencyLimit
	safeOperatingOccupancy := physicalConcurrencyLimit * configuredSafeCapacityMargin(resource.Spec.Prediction.SafeCapacityMargin)
	predictiveDemandRate := observation.Metric{}
	if resource.Spec.Prediction.Enabled {
		predictiveDemandRate = metricObservation(ctx, prom, resource.Spec.Metric.PredictiveRequestRateQuery, now)
	}
	snapshot := observation.Snapshot{
		SLOTargetP95Milliseconds: resource.Spec.SLO.TargetP95Milliseconds,
		Target: observation.TargetObservation{
			Name:                     resource.Spec.ScaleTargetRef.Name,
			CurrentReplicas:          targetCurrent,
			ReadyReplicas:            targetDetails.Status.ReadyReplicas,
			P95Latency:               metricObservation(ctx, prom, resource.Spec.Metric.PrometheusQuery, now),
			AggregateSlotOccupancy:   aggregateSlotOccupancy,
			HottestReplicaOccupancy:  hottestReplicaOccupancy,
			EffectiveServingReplicas: effectiveServingReplicas,
			PhysicalConcurrencyLimit: physicalConcurrencyLimit,
			SafeOperatingOccupancy:   safeOperatingOccupancy,
			RequestRates:             requestRatesObservation(ctx, prom, resource.Spec.Metric.RequestRateQuery, resource.Spec.Metric.ErrorRequestRateQuery, now),
			PredictiveRequestRate:    predictiveDemandRate,
		},
	}
	components := make(map[string]policy.ComponentInput, len(resource.Spec.Dependencies))
	entries := map[string]scaleEntry{"target": targetEntry}
	for _, dependency := range resource.Spec.Dependencies {
		depDeployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: dependency.ScaleTargetRef.Name, Namespace: resource.Namespace}}
		depKey := types.NamespacedName{Namespace: resource.Namespace, Name: dependency.ScaleTargetRef.Name}
		var depScale autoscalingv1.Scale
		depErr := r.SubResource("scale").Get(ctx, depDeployment, &depScale)
		latency := metricObservation(ctx, prom, dependency.Metrics.LatencyQuery, now)
		utilization := metricObservation(ctx, prom, dependency.Metrics.UtilizationQuery, now)
		requestRates := requestRatesObservation(ctx, prom, dependency.Metrics.RequestRateQuery, dependency.Metrics.ErrorRequestRateQuery, now)
		current := depScale.Spec.Replicas
		if depErr != nil {
			latency.Valid, latency.Fresh = false, false
			utilization.Valid, utilization.Fresh = false, false
			latency.Error = fmt.Sprintf("get dependency scale %s/%s: %v", depKey.Namespace, depKey.Name, depErr)
		}
		snapshot.Dependencies = append(snapshot.Dependencies, observation.DependencyObservation{
			Name: dependency.Name, DependsOn: dependency.DependsOn, CurrentReplicas: current, P95Latency: latency, Utilization: utilization,
			LatencyThreshold: dependency.Thresholds.LatencyMilliseconds, UtilizationThreshold: dependency.Thresholds.Utilization,
			Scalable: dependency.Scalable, MinReplicas: dependency.MinReplicas, MaxReplicas: dependency.MaxReplicas,
			RequestRates: requestRates,
		})
		components[dependency.Name] = policy.ComponentInput{Name: dependency.Name, CurrentReplicas: current, MinReplicas: dependency.MinReplicas, MaxReplicas: dependency.MaxReplicas, Scalable: dependency.Scalable}
		entries[dependency.Name] = scaleEntry{deployment: depDeployment, scale: depScale, key: depKey}
	}

	analysis := capacity.Analyze(snapshot)
	var lastScale *time.Time
	if resource.Status.LastScaleTime != nil {
		value := resource.Status.LastScaleTime.Time
		lastScale = &value
	}
	policyDecision := policy.EvaluateCapacity(policy.CapacityInput{
		Analysis:         analysis,
		Target:           policy.ComponentInput{Name: "target", CurrentReplicas: targetCurrent, MinReplicas: resource.Spec.MinReplicas, MaxReplicas: resource.Spec.MaxReplicas, Scalable: true},
		Dependencies:     components,
		MaxScaleUpStep:   resource.Spec.Policy.MaxScaleUpStep,
		MaxScaleDownStep: resource.Spec.Policy.MaxScaleDownStep,
		Cooldown:         time.Duration(resource.Spec.Policy.CooldownSeconds) * time.Second,
		LastScaleTime:    lastScale,
		Now:              now,
	})
	var prediction predictionAssessment
	if resource.Spec.Prediction.Enabled {
		policyDecision, prediction = r.evaluatePrediction(ctx, resource, snapshot, analysis, policyDecision, targetDetails, now)
		persistReadinessEvidence(&status, readinessLeadEvidence{
			Available: prediction.hasReadiness, LeadTime: prediction.readinessLeadTime,
			Source: prediction.readinessEvidenceSource, TemplateIdentity: prediction.readinessTemplateIdentity,
			ObservedAt: prediction.readinessObservedAt, Persist: prediction.readinessPersist,
		})
	}
	preGuardAction := policyDecision.Action
	policyDecision = holdTargetScaleForUnrealizedCapacity(policyDecision, snapshot.Target, targetCurrent)
	if preGuardAction != policyDecision.Action && resource.Spec.Prediction.Enabled {
		prediction.rejectedReason = targetCapacityRealizationReason(snapshot.Target)
		prediction.evidence = predictionEvidence(snapshot, prediction, resource.Spec.Prediction.MaxErrorRate)
	}

	entry := entries[policyDecision.ChosenComponent]
	chosenTarget := entry.key.Name
	var observedP95 *float64
	var observedAt time.Time
	if snapshot.Target.P95Latency.Valid {
		observedP95 = float64Ptr(snapshot.Target.P95Latency.Value)
		observedAt = snapshot.Target.P95Latency.ObservedAt
	}
	if observedAt.IsZero() {
		observedAt = now
	}
	status.ObservedMetric = observedP95
	if policyDecision.ChosenComponent == "target" {
		status.CurrentReplicas = policyDecision.CurrentReplicas
		status.DesiredReplicas = policyDecision.DesiredReplicas
	}
	status.ControlMode = controlMode(policyDecision.Action)
	rejected := analysis.RejectedActions
	if len(rejected) == 0 {
		rejected = rejectedActions(policyDecision.Action)
	}
	evidence := append([]string(nil), analysis.Evidence...)
	evidence = append(evidence, targetConcurrencyEvidence(snapshot.Target))
	evidence = append(evidence, prediction.evidence...)
	recordInput := decision.Input{
		Action: string(policyDecision.Action), Reason: policyDecision.Reason,
		ObservedMetric: observedP95, CurrentReplicas: policyDecision.CurrentReplicas,
		DesiredReplicas: policyDecision.DesiredReplicas, ObservedAt: observedAt,
		SLOTargetP95Milliseconds:      resource.Spec.SLO.TargetP95Milliseconds,
		ObservedTargetP95Milliseconds: observedP95,
		DetectedBottleneck:            string(analysis.Classification), BottleneckComponent: analysis.Component,
		Confidence: string(analysis.Confidence),
		Evidence:   evidence, ChosenTarget: chosenTarget, RejectedActions: rejected,
		TargetRequestRate:                      metricValuePointer(snapshot.Target.RequestRates.RequestRate),
		TargetSuccessfulRequestRate:            metricValuePointer(snapshot.Target.RequestRates.SuccessfulRequestRate),
		TargetErrorRate:                        metricValuePointer(snapshot.Target.RequestRates.ErrorRate),
		PredictiveRequestRate:                  metricValuePointer(snapshot.Target.PredictiveRequestRate),
		ReadyReplicas:                          snapshot.Target.ReadyReplicas,
		EffectiveServingReplicas:               effectiveServingReplicaCountPointer(snapshot.Target),
		AggregateConcurrencySlotOccupancy:      metricValuePointer(snapshot.Target.AggregateSlotOccupancy),
		HottestReplicaConcurrencySlotOccupancy: metricValuePointer(snapshot.Target.HottestReplicaOccupancy),
		PhysicalConcurrencyLimit:               floatPointer(snapshot.Target.PhysicalConcurrencyLimit),
		SafeOperatingOccupancy:                 floatPointer(snapshot.Target.SafeOperatingOccupancy),
		ReadinessEvidenceSource:                prediction.readinessEvidenceSource,
		ReadinessTemplateIdentity:              prediction.readinessTemplateIdentity,
		DependencyRequestRates:                 dependencyRequestRateRecords(snapshot.Dependencies),
		ForecastRequestRate:                    nil,
	}
	if resource.Spec.Prediction.Enabled {
		if prediction.demandWindow > 0 {
			recordInput.PredictiveDemandWindowSeconds = floatPointer(prediction.demandWindow.Seconds())
		}
		if prediction.demandObservationLag > 0 {
			recordInput.DemandObservationLagSeconds = floatPointer(prediction.demandObservationLag.Seconds())
		}
		recordInput.ForecastConfidence = string(prediction.trend.Quality)
		recordInput.PredictionAccepted = boolPointer(prediction.accepted)
		recordInput.PredictionRejectedReason = prediction.rejectedReason
		if prediction.trend.HasRequestRate && prediction.hasReadiness && prediction.hasDemandWindow && prediction.predictiveDemandValid {
			recordInput.ForecastRequestRate = floatPointer(prediction.trend.RequestRate)
		}
		if prediction.hasReadiness && prediction.hasDemandWindow {
			recordInput.ForecastHorizonSeconds = floatPointer(prediction.planningHorizon.Seconds())
		}
		if prediction.trend.HasTrend {
			recordInput.RequestRateSlope = floatPointer(prediction.trend.Slope)
		}
		if prediction.trend.HasFit {
			recordInput.ForecastFitR2 = floatPointer(prediction.trend.R2)
		}
		if prediction.hasSafeCapacity {
			recordInput.SafePerReplicaCapacity = floatPointer(prediction.safePerReplica)
		}
		if prediction.hasCurrentCapacity {
			recordInput.CurrentSafeCapacity = floatPointer(prediction.currentSafeCapacity)
		}
		if prediction.hasReadiness {
			recordInput.ReadinessLeadTimeSeconds = floatPointer(prediction.readinessLeadTime.Seconds())
		}
		if prediction.controlLoopAllowance > 0 {
			recordInput.ControlLoopAllowanceSeconds = floatPointer(prediction.controlLoopAllowance.Seconds())
		}
	}
	if policyDecision.HasThreshold {
		recordInput.Threshold = float64Ptr(policyDecision.Threshold)
	}
	record := decision.NewRecord(recordInput)
	status.LastDecision = record
	setConditions(&status, resource.Generation, policyDecision.Action != policy.ActionProtectedMode, policyDecision.Action == policy.ActionProtectedMode, conditionReason(policy.Decision{Action: policyDecision.Action}), policyDecision.Reason)

	if (policyDecision.Action == policy.ActionScaleTarget || policyDecision.Action == policy.ActionScaleDependency || policyDecision.Action == policy.ActionPrescaleTarget) && shouldUpdateScale(policyDecision.CurrentReplicas, policyDecision.DesiredReplicas) {
		entry.scale.Spec.Replicas = policyDecision.DesiredReplicas
		if err := r.SubResource("scale").Update(ctx, entry.deployment, client.WithSubResourceBody(&entry.scale)); err != nil {
			return reconcile.Result{}, fmt.Errorf("update %s/%s scale: %w", entry.key.Namespace, entry.key.Name, err)
		}
		if policyDecision.ChosenComponent == "target" {
			status.CurrentReplicas = policyDecision.DesiredReplicas
			status.DesiredReplicas = policyDecision.DesiredReplicas
		}
		status.LastScaleTime = timePtr(now)
		status.LastScaleDecision = record.DeepCopy()
		if r.Recorder != nil {
			reason := "ScaleTarget"
			message := "Scaled %s/%s from %d to %d: %s"
			if policyDecision.Action == policy.ActionPrescaleTarget {
				reason = "PreScaleTarget"
				message = "Prescaled %s/%s from %d to %d: %s"
			} else if policyDecision.Action == policy.ActionScaleDependency {
				reason = "ScaleDependency"
			}
			r.Recorder.Eventf(resource, corev1.EventTypeNormal, reason, message, entry.key.Namespace, entry.key.Name, policyDecision.CurrentReplicas, policyDecision.DesiredReplicas, policyDecision.Reason)
		}
	}
	var forecastRate, safeCapacity any
	if prediction.trend.HasRequestRate && prediction.hasReadiness && prediction.hasDemandWindow && prediction.predictiveDemandValid {
		forecastRate = prediction.trend.RequestRate
	}
	if prediction.hasCurrentCapacity {
		safeCapacity = prediction.currentSafeCapacity
	}
	log.FromContext(ctx).Info("capacity decision", "target", resource.Spec.ScaleTargetRef.Name, "targetP95Milliseconds", metricLogValue(snapshot.Target.P95Latency), "sloTargetP95Milliseconds", resource.Spec.SLO.TargetP95Milliseconds, "bottleneck", analysis.Classification, "component", analysis.Component, "action", policyDecision.Action, "chosenWorkload", chosenTarget, "requestedReplicas", targetCurrent, "readyReplicas", snapshot.Target.ReadyReplicas, "effectiveServingReplicas", effectiveServingReplicaCountLogValue(snapshot.Target), "aggregateSlotOccupancy", metricLogValue(snapshot.Target.AggregateSlotOccupancy), "hottestReplicaSlotOccupancy", metricLogValue(snapshot.Target.HottestReplicaOccupancy), "physicalConcurrencyLimit", snapshot.Target.PhysicalConcurrencyLimit, "safeOperatingOccupancy", snapshot.Target.SafeOperatingOccupancy, "currentReplicas", policyDecision.CurrentReplicas, "desiredReplicas", policyDecision.DesiredReplicas, "stableRequestRate", metricLogValue(snapshot.Target.RequestRates.RequestRate), "predictiveRequestRate", metricLogValue(snapshot.Target.PredictiveRequestRate), "predictiveDemandWindow", prediction.demandWindow.String(), "demandObservationLag", prediction.demandObservationLag.String(), "forecastRequestRate", forecastRate, "forecastQuality", prediction.trend.Quality, "safePerServingReplicaCapacity", prediction.safePerReplica, "realizedCurrentSafeCapacity", safeCapacity, "readinessLeadTime", prediction.readinessLeadTime.String(), "readinessEvidenceSource", prediction.readinessEvidenceSource, "readinessTemplateIdentity", prediction.readinessTemplateIdentity, "controlLoopAllowance", prediction.controlLoopAllowance.String(), "planningHorizon", prediction.planningHorizon.String(), "predictionAccepted", prediction.accepted, "reason", policyDecision.Reason)
	return reconcile.Result{RequeueAfter: reconcileInterval}, r.updateStatus(ctx, resource, status)
}

func requestRatesObservation(ctx context.Context, prom *promclient.Client, requestRateQuery, errorRequestRateQuery string, now time.Time) observation.RequestRates {
	requestRate := metricObservation(ctx, prom, requestRateQuery, now)
	errorRequestRate := metricObservation(ctx, prom, errorRequestRateQuery, now)
	return observation.DeriveRequestRates(requestRate, errorRequestRate)
}

func targetConcurrencyEvidence(target observation.TargetObservation) string {
	if !metricUsable(target.AggregateSlotOccupancy) {
		reason := target.AggregateSlotOccupancy.Error
		if reason == "" {
			reason = "aggregate constrained-slot occupancy telemetry is unavailable"
		}
		return "target constrained-slot evidence unavailable: " + reason
	}
	effective, err := observation.EffectiveReplicaCount(target.EffectiveServingReplicas, target.ReadyReplicas)
	if err != nil {
		return fmt.Sprintf("requested replicas %d, Kubernetes Ready replicas %d; effective serving replicas unavailable: %s", target.CurrentReplicas, target.ReadyReplicas, err)
	}
	meanOccupancy := 0.0
	if effective > 0 {
		meanOccupancy = target.AggregateSlotOccupancy.Value / float64(effective)
	}
	if !metricUsable(target.HottestReplicaOccupancy) {
		reason := target.HottestReplicaOccupancy.Error
		if reason == "" {
			reason = "hottest traffic-bearing replica occupancy is unavailable"
		}
		return fmt.Sprintf("requested replicas %d, Ready replicas %d, effective serving replicas %d; aggregate slot occupancy %.2f; hottest replica occupancy unavailable: %s", target.CurrentReplicas, target.ReadyReplicas, effective, target.AggregateSlotOccupancy.Value, reason)
	}
	return fmt.Sprintf("requested replicas %d; Ready replicas %d; effective serving replicas %d; aggregate held-slot occupancy %.2f; hottest traffic-bearing replica %.2f slots; physical concurrency ceiling %.2f slots/replica; safe operating boundary %.2f slots/replica; mean occupancy %.2f slots per serving replica", target.CurrentReplicas, target.ReadyReplicas, effective, target.AggregateSlotOccupancy.Value, target.HottestReplicaOccupancy.Value, target.PhysicalConcurrencyLimit, target.SafeOperatingOccupancy, meanOccupancy)
}

func targetCapacityRealizationPending(target observation.TargetObservation) bool {
	effective, err := observation.EffectiveReplicaCount(target.EffectiveServingReplicas, target.ReadyReplicas)
	return err == nil && target.ReadyReplicas > effective
}

func targetCapacityRealizationReason(target observation.TargetObservation) string {
	effective, _ := observation.EffectiveReplicaCount(target.EffectiveServingReplicas, target.ReadyReplicas)
	return fmt.Sprintf("%d Kubernetes Ready replicas but only %d are traffic-bearing; newly requested capacity has not yet been realized", target.ReadyReplicas, effective)
}

func holdTargetScaleForUnrealizedCapacity(decision policy.CapacityDecision, target observation.TargetObservation, requestedReplicas int32) policy.CapacityDecision {
	if !targetCapacityRealizationPending(target) || decision.ChosenComponent != "target" ||
		(decision.Action != policy.ActionScaleTarget && decision.Action != policy.ActionPrescaleTarget) {
		return decision
	}
	decision.Action = policy.ActionHold
	decision.CurrentReplicas = requestedReplicas
	decision.DesiredReplicas = requestedReplicas
	decision.Reason = targetCapacityRealizationReason(target)
	return decision
}

func effectiveServingReplicaCountPointer(target observation.TargetObservation) *int32 {
	effective, err := observation.EffectiveReplicaCount(target.EffectiveServingReplicas, target.ReadyReplicas)
	if err != nil {
		return nil
	}
	return &effective
}

func effectiveServingReplicaCountLogValue(target observation.TargetObservation) any {
	if count := effectiveServingReplicaCountPointer(target); count != nil {
		return *count
	}
	return nil
}

func configuredSafeCapacityMargin(value float64) float64 {
	if finite(value) && value > 0 && value <= 1 {
		return value
	}
	return defaultSafeCapacityMargin
}

func metricValuePointer(metric observation.Metric) *float64 {
	if !metric.Valid || !metric.Fresh || metric.ObservedAt.IsZero() || math.IsNaN(metric.Value) || math.IsInf(metric.Value, 0) || metric.Value < 0 {
		return nil
	}
	return float64Ptr(metric.Value)
}

func dependencyRequestRateRecords(dependencies []observation.DependencyObservation) []optiscalev1alpha1.DependencyRequestRate {
	if len(dependencies) == 0 {
		return nil
	}
	records := make([]optiscalev1alpha1.DependencyRequestRate, 0, len(dependencies))
	for _, dependency := range dependencies {
		records = append(records, optiscalev1alpha1.DependencyRequestRate{
			Name:                  dependency.Name,
			RequestRate:           metricValuePointer(dependency.RequestRates.RequestRate),
			SuccessfulRequestRate: metricValuePointer(dependency.RequestRates.SuccessfulRequestRate),
			ErrorRate:             metricValuePointer(dependency.RequestRates.ErrorRate),
		})
	}
	return records
}

func metricObservation(ctx context.Context, prom *promclient.Client, query string, now time.Time) observation.Metric {
	if strings.TrimSpace(query) == "" {
		return observation.Metric{Error: "Prometheus query is missing"}
	}
	value, err := prom.Query(ctx, query)
	if err != nil {
		return observation.Metric{Error: err.Error()}
	}
	metric := observation.Metric{Value: value.Value, ObservedAt: value.Timestamp, Valid: true}
	metric.Fresh = !value.Timestamp.After(now.Add(30*time.Second)) && now.Sub(value.Timestamp) <= maxTelemetryAge
	if !metric.Fresh {
		metric.Error = "telemetry is stale or timestamp is in the future"
	}
	return metric
}

func metricLogValue(metric observation.Metric) any {
	if !metric.Valid {
		return nil
	}
	return metric.Value
}

func rejectedActions(chosen policy.Action) []string {
	switch chosen {
	case policy.ActionScaleTarget:
		return []string{string(policy.ActionScaleDependency)}
	case policy.ActionScaleDependency:
		return []string{string(policy.ActionScaleTarget)}
	case policy.ActionPrescaleTarget:
		return []string{"SCALE_DEPENDENCY: dependencies are healthy; no dependency bottleneck is present"}
	default:
		return []string{string(policy.ActionScaleTarget), string(policy.ActionScaleDependency)}
	}
}

func (r *OptiScalerReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("optiscaler", req.NamespacedName)
	var resource optiscalev1alpha1.OptiScaler
	if err := r.Get(ctx, req.NamespacedName, &resource); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	status := resource.Status
	status.DesiredReplicas = status.CurrentReplicas
	if err := validateSpec(resource.Spec); err != nil {
		logger.Info("protecting due to invalid configuration", "error", err.Error())
		status.ControlMode = optiscalev1alpha1.ControlModeProtected
		status.LastDecision = decision.NewRecord(decision.Input{Action: string(optiscalev1alpha1.ActionProtectedMode), Reason: err.Error(), CurrentReplicas: status.CurrentReplicas, DesiredReplicas: status.CurrentReplicas})
		setConditions(&status, resource.Generation, false, true, "InvalidConfiguration", err.Error())
		return reconcile.Result{RequeueAfter: reconcileInterval}, r.updateStatus(ctx, &resource, status)
	}

	var scale autoscalingv1.Scale
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: resource.Spec.ScaleTargetRef.Name, Namespace: resource.Namespace}}
	targetKey := types.NamespacedName{Namespace: deployment.Namespace, Name: deployment.Name}
	if err := r.SubResource("scale").Get(ctx, deployment, &scale); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, fmt.Errorf("get scale target %s/%s: %w", targetKey.Namespace, targetKey.Name, err)
		}
		return reconcile.Result{}, err
	}
	current := scale.Spec.Replicas
	status.CurrentReplicas = current
	status.DesiredReplicas = current

	prom, err := promclient.NewClient(resource.Spec.Prometheus.Address, 10*time.Second)
	if err != nil {
		return reconcile.Result{RequeueAfter: reconcileInterval}, r.protectedStatus(ctx, &resource, status, err.Error())
	}
	if resource.Spec.Metric.UtilizationQuery != "" || len(resource.Spec.Dependencies) > 0 {
		return r.reconcileCapacity(ctx, &resource, status, deployment, scale, prom)
	}
	observation, err := prom.Query(ctx, resource.Spec.Metric.PrometheusQuery)
	if err != nil {
		return reconcile.Result{RequeueAfter: reconcileInterval}, r.protectedStatus(ctx, &resource, status, err.Error())
	}

	now := time.Now().UTC()
	var lastScale *time.Time
	if resource.Status.LastScaleTime != nil {
		value := resource.Status.LastScaleTime.Time
		lastScale = &value
	}
	policyDecision := policy.Evaluate(policy.Input{
		CurrentReplicas:     current,
		MinReplicas:         resource.Spec.MinReplicas,
		MaxReplicas:         resource.Spec.MaxReplicas,
		ObservedMetric:      observation.Value,
		TelemetryValid:      true,
		TelemetryObservedAt: observation.Timestamp,
		MaxTelemetryAge:     maxTelemetryAge,
		Now:                 now,
		ScaleUpThreshold:    resource.Spec.Policy.ScaleUpThreshold,
		ScaleDownThreshold:  resource.Spec.Policy.ScaleDownThreshold,
		MaxScaleUpStep:      resource.Spec.Policy.MaxScaleUpStep,
		MaxScaleDownStep:    resource.Spec.Policy.MaxScaleDownStep,
		Cooldown:            time.Duration(resource.Spec.Policy.CooldownSeconds) * time.Second,
		LastScaleTime:       lastScale,
	})
	requestRates := requestRatesObservation(ctx, prom, resource.Spec.Metric.RequestRateQuery, resource.Spec.Metric.ErrorRequestRateQuery, now)

	status.ObservedMetric = float64Ptr(observation.Value)
	status.DesiredReplicas = policyDecision.DesiredReplicas
	status.ControlMode = controlMode(policyDecision.Action)
	recordInput := decision.Input{
		Action: string(policyDecision.Action), Reason: policyDecision.Reason,
		ObservedMetric: float64Ptr(observation.Value), CurrentReplicas: current,
		DesiredReplicas: policyDecision.DesiredReplicas, ObservedAt: observation.Timestamp,
		TargetRequestRate:           metricValuePointer(requestRates.RequestRate),
		TargetSuccessfulRequestRate: metricValuePointer(requestRates.SuccessfulRequestRate),
		TargetErrorRate:             metricValuePointer(requestRates.ErrorRate),
	}
	if policyDecision.HasThreshold {
		recordInput.Threshold = float64Ptr(policyDecision.Threshold)
	}
	status.LastDecision = decision.NewRecord(recordInput)
	setConditions(&status, resource.Generation, policyDecision.Action != policy.ActionProtectedMode, policyDecision.Action == policy.ActionProtectedMode, conditionReason(policyDecision), policyDecision.Reason)

	if (policyDecision.Action == policy.ActionScaleTarget || policyDecision.Action == policy.ActionPrescaleTarget) && shouldUpdateScale(current, policyDecision.DesiredReplicas) {
		scale.Spec.Replicas = policyDecision.DesiredReplicas
		if err := r.SubResource("scale").Update(ctx, deployment, client.WithSubResourceBody(&scale)); err != nil {
			return reconcile.Result{}, fmt.Errorf("update %s/%s scale: %w", targetKey.Namespace, targetKey.Name, err)
		}
		status.CurrentReplicas = policyDecision.DesiredReplicas
		status.DesiredReplicas = policyDecision.DesiredReplicas
		status.LastScaleTime = timePtr(now)
		status.LastScaleDecision = status.LastDecision.DeepCopy()
		if r.Recorder != nil {
			eventReason, message := "ScaleTarget", "Scaled %s/%s from %d to %d: %s"
			if policyDecision.Action == policy.ActionPrescaleTarget {
				eventReason, message = "PreScaleTarget", "Prescaled %s/%s from %d to %d: %s"
			}
			r.Recorder.Eventf(&resource, corev1.EventTypeNormal, eventReason, message, targetKey.Namespace, targetKey.Name, current, policyDecision.DesiredReplicas, policyDecision.Reason)
		}
		logger.Info("scaled target", "target", targetKey, "from", current, "to", policyDecision.DesiredReplicas, "reason", policyDecision.Reason)
	}

	return reconcile.Result{RequeueAfter: reconcileInterval}, r.updateStatus(ctx, &resource, status)
}

func (r *OptiScalerReconciler) protectedStatus(ctx context.Context, resource *optiscalev1alpha1.OptiScaler, status optiscalev1alpha1.OptiScalerStatus, reason string) error {
	status.ControlMode = optiscalev1alpha1.ControlModeProtected
	status.DesiredReplicas = status.CurrentReplicas
	status.ObservedMetric = nil
	status.LastDecision = decision.NewRecord(decision.Input{Action: string(optiscalev1alpha1.ActionProtectedMode), Reason: reason, CurrentReplicas: status.CurrentReplicas, DesiredReplicas: status.CurrentReplicas})
	setConditions(&status, resource.Generation, false, true, "TelemetryUnavailable", reason)
	return r.updateStatus(ctx, resource, status)
}

func (r *OptiScalerReconciler) updateStatus(ctx context.Context, resource *optiscalev1alpha1.OptiScaler, status optiscalev1alpha1.OptiScalerStatus) error {
	if equality.Semantic.DeepEqual(resource.Status, status) {
		return nil
	}
	resource.Status = status
	return r.Status().Update(ctx, resource)
}

func (r *OptiScalerReconciler) SetupWithManager(mgr manager.Manager) error {
	return builder.ControllerManagedBy(mgr).
		For(&optiscalev1alpha1.OptiScaler{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

func validateSpec(spec optiscalev1alpha1.OptiScalerSpec) error {
	if spec.ScaleTargetRef.APIVersion != "apps/v1" || spec.ScaleTargetRef.Kind != "Deployment" {
		return fmt.Errorf("scaleTargetRef must reference apps/v1 Deployment")
	}
	if strings.TrimSpace(spec.ScaleTargetRef.Name) == "" {
		return fmt.Errorf("scaleTargetRef.name is required")
	}
	if spec.MinReplicas < 1 {
		return fmt.Errorf("minReplicas must be at least 1")
	}
	if spec.MaxReplicas < spec.MinReplicas {
		return fmt.Errorf("maxReplicas must be at least minReplicas")
	}
	if strings.TrimSpace(spec.Metric.PrometheusQuery) == "" {
		return fmt.Errorf("metric.prometheusQuery is required")
	}
	if strings.TrimSpace(spec.Metric.UtilizationQuery) == "" || strings.TrimSpace(spec.Metric.HottestReplicaUtilizationQuery) == "" || strings.TrimSpace(spec.Metric.ServingReplicaCountQuery) == "" {
		return fmt.Errorf("metric.utilizationQuery, hottestReplicaUtilizationQuery, and servingReplicaCountQuery are required for target capacity attribution")
	}
	if math.IsNaN(spec.Metric.PhysicalConcurrencyLimit) || math.IsInf(spec.Metric.PhysicalConcurrencyLimit, 0) || spec.Metric.PhysicalConcurrencyLimit < 1 {
		return fmt.Errorf("metric.physicalConcurrencyLimit must be finite and at least 1")
	}
	if err := validateRateQueryPair(spec.Metric.RequestRateQuery, spec.Metric.ErrorRequestRateQuery, "metric"); err != nil {
		return err
	}
	if spec.Prediction.PredictiveDemandWindowSeconds != 0 && (spec.Prediction.PredictiveDemandWindowSeconds < minimumPredictiveDemandWindowSeconds || spec.Prediction.PredictiveDemandWindowSeconds > maximumPredictiveDemandWindowSeconds) {
		return fmt.Errorf("prediction.predictiveDemandWindowSeconds must be in [%d,%d]", minimumPredictiveDemandWindowSeconds, maximumPredictiveDemandWindowSeconds)
	}
	if spec.Prediction.Enabled {
		if strings.TrimSpace(spec.Metric.RequestRateQuery) == "" || strings.TrimSpace(spec.Metric.ErrorRequestRateQuery) == "" {
			return fmt.Errorf("prediction requires target requestRateQuery and errorRequestRateQuery")
		}
		if strings.TrimSpace(spec.Metric.PredictiveRequestRateQuery) == "" {
			return fmt.Errorf("prediction requires metric.predictiveRequestRateQuery; stable requestRateQuery is not a predictive fallback")
		}
		if spec.Prediction.PredictiveDemandWindowSeconds < minimumPredictiveDemandWindowSeconds || spec.Prediction.PredictiveDemandWindowSeconds > maximumPredictiveDemandWindowSeconds {
			return fmt.Errorf("prediction.predictiveDemandWindowSeconds must be in [%d,%d] when prediction is enabled", minimumPredictiveDemandWindowSeconds, maximumPredictiveDemandWindowSeconds)
		}
		if math.IsNaN(spec.Prediction.SafeCapacityMargin) || math.IsInf(spec.Prediction.SafeCapacityMargin, 0) || spec.Prediction.SafeCapacityMargin < minimumSafeCapacityMargin || spec.Prediction.SafeCapacityMargin > 1 {
			return fmt.Errorf("prediction.safeCapacityMargin must be finite and in [0.000001,1]")
		}
		if math.IsNaN(spec.Prediction.MaxErrorRate) || math.IsInf(spec.Prediction.MaxErrorRate, 0) || spec.Prediction.MaxErrorRate < 0 || spec.Prediction.MaxErrorRate > 1 {
			return fmt.Errorf("prediction.maxErrorRate must be finite and in [0,1]")
		}
	}
	if math.IsNaN(spec.SLO.TargetP95Milliseconds) || math.IsInf(spec.SLO.TargetP95Milliseconds, 0) || spec.SLO.TargetP95Milliseconds < 0 {
		return fmt.Errorf("slo.targetP95Milliseconds must be finite and nonnegative")
	}
	if math.IsNaN(spec.Policy.ScaleUpThreshold) || math.IsInf(spec.Policy.ScaleUpThreshold, 0) || math.IsNaN(spec.Policy.ScaleDownThreshold) || math.IsInf(spec.Policy.ScaleDownThreshold, 0) || spec.Policy.ScaleUpThreshold < 0 || spec.Policy.ScaleDownThreshold < 0 {
		return fmt.Errorf("policy thresholds must be finite and nonnegative")
	}
	if spec.Policy.ScaleDownThreshold > spec.Policy.ScaleUpThreshold {
		return fmt.Errorf("scaleDownThreshold must not exceed scaleUpThreshold")
	}
	if spec.Policy.MaxScaleUpStep < 1 || spec.Policy.MaxScaleDownStep < 1 {
		return fmt.Errorf("scale steps must be at least 1")
	}
	if strings.TrimSpace(spec.Prometheus.Address) == "" {
		return fmt.Errorf("prometheus.address is required")
	}
	seen := map[string]struct{}{}
	for _, dependency := range spec.Dependencies {
		if strings.TrimSpace(dependency.Name) == "" || strings.TrimSpace(dependency.ScaleTargetRef.Name) == "" {
			return fmt.Errorf("dependency name and scaleTargetRef.name are required")
		}
		if dependency.ScaleTargetRef.APIVersion != "apps/v1" || dependency.ScaleTargetRef.Kind != "Deployment" {
			return fmt.Errorf("dependency %q must reference apps/v1 Deployment", dependency.Name)
		}
		if _, exists := seen[dependency.Name]; exists {
			return fmt.Errorf("dependency name %q is duplicated", dependency.Name)
		}
		if dependency.Name == "target" {
			return fmt.Errorf("dependency name \"target\" is reserved")
		}
		seen[dependency.Name] = struct{}{}
		if dependency.MinReplicas < 1 || dependency.MaxReplicas < dependency.MinReplicas {
			return fmt.Errorf("dependency %q has invalid replica bounds", dependency.Name)
		}
		if dependency.ScaleTargetRef.Name == spec.ScaleTargetRef.Name {
			return fmt.Errorf("dependency %q cannot be the primary scale target", dependency.Name)
		}
		if strings.TrimSpace(dependency.Metrics.LatencyQuery) == "" || strings.TrimSpace(dependency.Metrics.UtilizationQuery) == "" {
			return fmt.Errorf("dependency %q metric queries are required", dependency.Name)
		}
		if err := validateRateQueryPair(dependency.Metrics.RequestRateQuery, dependency.Metrics.ErrorRequestRateQuery, "dependency "+dependency.Name); err != nil {
			return err
		}
		if math.IsNaN(dependency.Thresholds.LatencyMilliseconds) || math.IsInf(dependency.Thresholds.LatencyMilliseconds, 0) || dependency.Thresholds.LatencyMilliseconds < 0 || math.IsNaN(dependency.Thresholds.Utilization) || math.IsInf(dependency.Thresholds.Utilization, 0) || dependency.Thresholds.Utilization < 0 {
			return fmt.Errorf("dependency %q thresholds must be finite and nonnegative", dependency.Name)
		}
	}
	for _, dependency := range spec.Dependencies {
		if dependency.DependsOn == "" {
			continue
		}
		if dependency.DependsOn == dependency.Name {
			return fmt.Errorf("dependency %q cannot depend on itself", dependency.Name)
		}
		if _, exists := seen[dependency.DependsOn]; !exists {
			return fmt.Errorf("dependency %q refers to unconfigured dependsOn %q", dependency.Name, dependency.DependsOn)
		}
	}
	return nil
}

func validateRateQueryPair(requestRateQuery, errorRequestRateQuery, component string) error {
	requestRateMissing := strings.TrimSpace(requestRateQuery) == ""
	errorRateMissing := strings.TrimSpace(errorRequestRateQuery) == ""
	if requestRateMissing != errorRateMissing {
		return fmt.Errorf("%s requestRateQuery and errorRequestRateQuery must be configured together", component)
	}
	return nil
}

func setConditions(status *optiscalev1alpha1.OptiScalerStatus, generation int64, ready, protected bool, reason, message string) {
	conditions := []metav1.Condition{
		{Type: ConditionReady, Status: conditionStatus(ready), ObservedGeneration: generation, LastTransitionTime: metav1.Now(), Reason: reason, Message: message},
		{Type: ConditionProtected, Status: conditionStatus(protected), ObservedGeneration: generation, LastTransitionTime: metav1.Now(), Reason: reason, Message: message},
	}
	for i := range conditions {
		for _, previous := range status.Conditions {
			if previous.Type == conditions[i].Type && previous.Status == conditions[i].Status && previous.ObservedGeneration == conditions[i].ObservedGeneration && previous.Reason == conditions[i].Reason && previous.Message == conditions[i].Message {
				conditions[i].LastTransitionTime = previous.LastTransitionTime
			}
		}
	}
	status.Conditions = conditions
}

func conditionStatus(value bool) metav1.ConditionStatus {
	if value {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}
func controlMode(action policy.Action) optiscalev1alpha1.ControlMode {
	switch action {
	case policy.ActionProtectedMode:
		return optiscalev1alpha1.ControlModeProtected
	case policy.ActionHold:
		return optiscalev1alpha1.ControlModeHold
	default:
		return optiscalev1alpha1.ControlModeAutomatic
	}
}
func conditionReason(d policy.Decision) string {
	if d.Action == policy.ActionProtectedMode {
		return "ProtectedMode"
	}
	if d.Action == policy.ActionHold {
		return "PolicyHold"
	}
	return "PolicyScale"
}
func float64Ptr(value float64) *float64             { return &value }
func timePtr(value time.Time) *metav1.Time          { result := metav1.NewTime(value); return &result }
func shouldUpdateScale(current, desired int32) bool { return current != desired }
