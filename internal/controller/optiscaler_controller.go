package controller

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	optiscalev1alpha1 "github.com/RajRaghupatruni/Silver-Leaf/api/v1alpha1"
	"github.com/RajRaghupatruni/Silver-Leaf/internal/decision"
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
