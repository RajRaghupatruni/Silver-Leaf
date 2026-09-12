package v1alpha1

import "testing"

func TestDeepCopyIncludesDependenciesAndScaleDecision(t *testing.T) {
	resource := &OptiScaler{
		Spec:   OptiScalerSpec{Dependencies: []DependencySpec{{Name: "inventory", ScaleTargetRef: ScaleTargetReference{Name: "inventory-service"}}}},
		Status: OptiScalerStatus{LastScaleDecision: &DecisionRecord{Evidence: []string{"observed"}, RejectedActions: []string{"SCALE_TARGET"}}},
	}
	copy := resource.DeepCopy()
	copy.Spec.Dependencies[0].Name = "changed"
	copy.Status.LastScaleDecision.Evidence[0] = "changed"
	copy.Status.LastScaleDecision.RejectedActions[0] = "changed"
	if resource.Spec.Dependencies[0].Name != "inventory" || resource.Status.LastScaleDecision.Evidence[0] != "observed" || resource.Status.LastScaleDecision.RejectedActions[0] != "SCALE_TARGET" {
		t.Fatal("deep copy shares mutable dependency or decision slices")
	}
}
