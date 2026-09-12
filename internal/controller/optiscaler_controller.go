package controller

import (
	"context"
	"fmt"
	"math"
	"strings"
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
	ConditionReady     = "Ready"
	ConditionProtected = "ProtectedMode"
	reconcileInterval  = 15 * time.Second
	maxTelemetryAge    = 2 * time.Minute
)

type OptiScalerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
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
	snapshot := observation.Snapshot{
		SLOTargetP95Milliseconds: resource.Spec.SLO.TargetP95Milliseconds,
		Target: observation.TargetObservation{
			Name:                 resource.Spec.ScaleTargetRef.Name,
			CurrentReplicas:      targetCurrent,
			P95Latency:           metricObservation(ctx, prom, resource.Spec.Metric.PrometheusQuery, now),
			Utilization:          metricObservation(ctx, prom, resource.Spec.Metric.UtilizationQuery, now),
			UtilizationThreshold: resource.Spec.Metric.UtilizationThreshold,
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
	recordInput := decision.Input{
		Action: string(policyDecision.Action), Reason: policyDecision.Reason,
		ObservedMetric: observedP95, CurrentReplicas: policyDecision.CurrentReplicas,
		DesiredReplicas: policyDecision.DesiredReplicas, ObservedAt: observedAt,
		SLOTargetP95Milliseconds:      resource.Spec.SLO.TargetP95Milliseconds,
		ObservedTargetP95Milliseconds: observedP95,
		DetectedBottleneck:            string(analysis.Classification), BottleneckComponent: analysis.Component,
		Confidence: string(analysis.Confidence),
		Evidence:   analysis.Evidence, ChosenTarget: chosenTarget, RejectedActions: rejected,
	}
	if policyDecision.HasThreshold {
		recordInput.Threshold = float64Ptr(policyDecision.Threshold)
	}
	record := decision.NewRecord(recordInput)
	status.LastDecision = record
	setConditions(&status, resource.Generation, policyDecision.Action != policy.ActionProtectedMode, policyDecision.Action == policy.ActionProtectedMode, conditionReason(policy.Decision{Action: policyDecision.Action}), policyDecision.Reason)

	if (policyDecision.Action == policy.ActionScaleTarget || policyDecision.Action == policy.ActionScaleDependency) && shouldUpdateScale(policyDecision.CurrentReplicas, policyDecision.DesiredReplicas) {
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
			if policyDecision.Action == policy.ActionScaleDependency {
				reason = "ScaleDependency"
			}
			r.Recorder.Eventf(resource, corev1.EventTypeNormal, reason, "Scaled %s/%s from %d to %d: %s", entry.key.Namespace, entry.key.Name, policyDecision.CurrentReplicas, policyDecision.DesiredReplicas, policyDecision.Reason)
		}
	}
	log.FromContext(ctx).Info("capacity decision", "target", resource.Spec.ScaleTargetRef.Name, "targetP95Milliseconds", metricLogValue(snapshot.Target.P95Latency), "sloTargetP95Milliseconds", resource.Spec.SLO.TargetP95Milliseconds, "bottleneck", analysis.Classification, "component", analysis.Component, "action", policyDecision.Action, "chosenWorkload", chosenTarget, "currentReplicas", policyDecision.CurrentReplicas, "desiredReplicas", policyDecision.DesiredReplicas, "reason", policyDecision.Reason)
	return reconcile.Result{RequeueAfter: reconcileInterval}, r.updateStatus(ctx, resource, status)
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

	status.ObservedMetric = float64Ptr(observation.Value)
	status.DesiredReplicas = policyDecision.DesiredReplicas
	status.ControlMode = controlMode(policyDecision.Action)
	recordInput := decision.Input{Action: string(policyDecision.Action), Reason: policyDecision.Reason, ObservedMetric: float64Ptr(observation.Value), CurrentReplicas: current, DesiredReplicas: policyDecision.DesiredReplicas, ObservedAt: observation.Timestamp}
	if policyDecision.HasThreshold {
		recordInput.Threshold = float64Ptr(policyDecision.Threshold)
	}
	status.LastDecision = decision.NewRecord(recordInput)
	setConditions(&status, resource.Generation, policyDecision.Action != policy.ActionProtectedMode, policyDecision.Action == policy.ActionProtectedMode, conditionReason(policyDecision), policyDecision.Reason)

	if policyDecision.Action == policy.ActionScaleTarget && shouldUpdateScale(current, policyDecision.DesiredReplicas) {
		scale.Spec.Replicas = policyDecision.DesiredReplicas
		if err := r.SubResource("scale").Update(ctx, deployment, client.WithSubResourceBody(&scale)); err != nil {
			return reconcile.Result{}, fmt.Errorf("update %s/%s scale: %w", targetKey.Namespace, targetKey.Name, err)
		}
		status.CurrentReplicas = policyDecision.DesiredReplicas
		status.DesiredReplicas = policyDecision.DesiredReplicas
		status.LastScaleTime = timePtr(now)
		status.LastScaleDecision = status.LastDecision.DeepCopy()
		if r.Recorder != nil {
			r.Recorder.Eventf(&resource, corev1.EventTypeNormal, "ScaleTarget", "Scaled %s/%s from %d to %d: %s", targetKey.Namespace, targetKey.Name, current, policyDecision.DesiredReplicas, policyDecision.Reason)
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
	if len(spec.Dependencies) > 0 && strings.TrimSpace(spec.Metric.UtilizationQuery) == "" {
		return fmt.Errorf("metric.utilizationQuery is required when dependencies are configured")
	}
	if spec.Metric.UtilizationQuery != "" && (math.IsNaN(spec.Metric.UtilizationThreshold) || math.IsInf(spec.Metric.UtilizationThreshold, 0) || spec.Metric.UtilizationThreshold < 0) {
		return fmt.Errorf("metric.utilizationThreshold must be finite and nonnegative")
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
