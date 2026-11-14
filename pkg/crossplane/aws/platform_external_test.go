package aws_test

import (
	"testing"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// A plan of one component that requires a component of another plan (a
// tenant component requiring a platform one) waits for that component's
// last Release, and keeps it with a Usage until its own chart is gone.
func TestPlatformSetWaitsForAComponentOfAnotherPlan(t *testing.T) {
	c := clusterPlanForNames()
	base := crossplaneaws.HelmReleaseObjectName(c.Prefix, "base")
	tests := []struct {
		name      string
		external  map[string]string
		wantAfter int
		wantUsage bool
	}{
		// ProviderConfig + node group, then Ready and deployed of the other plan's Release.
		{"the required component is of another plan", map[string]string{"base": base}, 4, true},
		{"the required component is nowhere", nil, 2, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := crossplaneaws.PlatformPlan{
				Namespace: c.Namespace, TenantID: c.TenantID, ClusterID: c.ClusterID, ClusterPrefix: c.Prefix,
				NodeGroupName: c.Prefix + "-default", External: tt.external,
				Components: []crossplaneaws.PlatformComponentPlan{{Name: "policies", Namespace: "base-system", Requires: []string{"base"},
					Main: crossplaneaws.HelmReleasePlan{Name: "policies", Chart: chart("policies", "1.0.0")}}},
			}

			set := crossplaneaws.PlatformSet(plan)

			usage := false
			for _, item := range set {
				switch o := item.Object.(type) {
				case *unstructured.Unstructured:
					if len(item.After) != tt.wantAfter {
						t.Errorf("%s waits for %d conditions, want %d", o.GetName(), len(item.After), tt.wantAfter)
					}
				case *protectionv1beta1.Usage:
					usage = usage || (o.Spec.Of.ResourceRef.Name == base &&
						o.Spec.By.ResourceRef.Name == crossplaneaws.HelmReleaseObjectName(c.Prefix, "policies"))
				}
			}
			if usage != tt.wantUsage {
				t.Errorf("a Usage of %s by the component's chart: %v, want %v", base, usage, tt.wantUsage)
			}
		})
	}
}
