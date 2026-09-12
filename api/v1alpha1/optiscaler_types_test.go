package v1alpha1

import "testing"

func TestDeepCopyIncludesDependenciesAndScaleDecision(t *testing.T) {
	requestRate := 12.0
	resource := &OptiScaler{
		Spec: OptiScalerSpec{Dependencies: []DependencySpec{{Name: "inventory", ScaleTargetRef: ScaleTargetReference{Name: "inventory-service"}}}},
		Status: OptiScalerStatus{LastScaleDecision: &DecisionRecord{
			Evidence: []string{"observed"}, RejectedActions: []string{"SCALE_TARGET"}, TargetRequestRate: &requestRate,
			DependencyRequestRates: []DependencyRequestRate{{Name: "postgres", RequestRate: &requestRate}},
		}},
	}
	copy := resource.DeepCopy()
	copy.Spec.Dependencies[0].Name = "changed"
	copy.Status.LastScaleDecision.Evidence[0] = "changed"
	copy.Status.LastScaleDecision.RejectedActions[0] = "changed"
	*copy.Status.LastScaleDecision.TargetRequestRate = 99
	*copy.Status.LastScaleDecision.DependencyRequestRates[0].RequestRate = 99
	if resource.Spec.Dependencies[0].Name != "inventory" || resource.Status.LastScaleDecision.Evidence[0] != "observed" || resource.Status.LastScaleDecision.RejectedActions[0] != "SCALE_TARGET" || *resource.Status.LastScaleDecision.TargetRequestRate != requestRate || *resource.Status.LastScaleDecision.DependencyRequestRates[0].RequestRate != requestRate {
		t.Fatal("deep copy shares mutable dependency or decision slices")
	}
}
