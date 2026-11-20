package aws_test

import (
	"strings"
	"testing"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// A component of a higher sequence waits for the last Release of every
// component of a lower sequence (Ready and deployed), and keeps it with a
// Usage; a component of the same sequence does not.
func TestPlatformSetWaitsForLowerSequences(t *testing.T) {
	c := clusterPlanForNames()
	plan := crossplaneaws.PlatformPlan{Namespace: c.Namespace, TenantID: c.TenantID, ClusterID: c.ClusterID, ClusterPrefix: c.Prefix,
		Components: []crossplaneaws.PlatformComponentPlan{
			{Name: "karp", Namespace: "k", Sequence: 0, Main: crossplaneaws.HelmReleasePlan{Name: "karp", Chart: chart("karp", "1")}},
			{Name: "one", Namespace: "o", Sequence: 100, Main: crossplaneaws.HelmReleasePlan{Name: "one", Chart: chart("one", "1")}},
			{Name: "two", Namespace: "t", Sequence: 100, Main: crossplaneaws.HelmReleasePlan{Name: "two", Chart: chart("two", "1")}},
		}}

	set := crossplaneaws.PlatformSet(plan)

	after := map[string]int{}
	pairs := map[string]bool{}
	for _, item := range set {
		switch o := item.Object.(type) {
		case *unstructured.Unstructured:
			after[strings.TrimPrefix(o.GetName(), plan.ClusterPrefix+"-")] = len(item.After)
		case *protectionv1beta1.Usage:
			if o.Spec.By.Kind == "Release" {
				pairs[strings.TrimPrefix(o.Spec.Of.ResourceRef.Name, plan.ClusterPrefix+"-")+"<-"+strings.TrimPrefix(o.Spec.By.ResourceRef.Name, plan.ClusterPrefix+"-")] = true
			}
		}
	}
	if after["karp"] != 1 || after["one"] != 3 || after["two"] != 3 {
		t.Errorf("conditions karp/one/two = %d/%d/%d, want 1/3/3", after["karp"], after["one"], after["two"])
	}
	if !pairs["karp<-one"] || !pairs["karp<-two"] || len(pairs) != 2 {
		t.Errorf("usages %v", pairs)
	}
}
