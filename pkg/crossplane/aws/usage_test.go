package aws_test

import (
	"strings"
	"testing"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// Every Usage refers to objects the render contains (subnets: the
// network's, given in the plan), and comes after them.
func TestUsagesReferToRenderedObjects(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	plan := clusterPlan()
	plan.NodeGroupName = "prod-1a2b3c4d-default-abc123"
	plan.SubnetObjects = []string{"net-private-use1-az1", "net-private-use1-az2"}
	objects := crossplaneaws.ClusterResources(plan)

	seen := map[string]int{} // "Kind/name" -> position
	key := func(kind, name string) string { return kind + "/" + name }
	var usages []*protectionv1beta1.Usage
	for i, obj := range objects {
		gvk, err := apiutil.GVKForObject(obj, scheme)
		if err != nil {
			t.Fatal(err)
		}
		seen[key(gvk.Kind, obj.GetName())] = i
		if u, ok := obj.(*protectionv1beta1.Usage); ok {
			usages = append(usages, u)
		}
	}
	for _, s := range plan.SubnetObjects {
		seen[key("Subnet", s)] = -1 // the network's
	}
	if len(usages) != 13 { // 11 + 2 subnets
		t.Fatalf("%d usages, want 13", len(usages))
	}
	names := map[string]bool{}
	for _, u := range usages {
		pos := seen[key(u.Spec.Of.Kind, u.Spec.Of.ResourceRef.Name)]
		for _, ref := range []struct{ kind, name string }{
			{u.Spec.Of.Kind, u.Spec.Of.ResourceRef.Name}, {u.Spec.By.Kind, u.Spec.By.ResourceRef.Name},
		} {
			p, ok := seen[key(ref.kind, ref.name)]
			if !ok {
				t.Errorf("usage %s refers to %s %s, which is not rendered", u.Name, ref.kind, ref.name)
			}
			if p > pos {
				pos = p
			}
		}
		if seen[key("Usage", u.Name)] < pos {
			t.Errorf("usage %s comes before an object it refers to", u.Name)
		}
		if !*u.Spec.ReplayDeletion || u.Labels["cluster-vending.io/cluster"] != plan.ClusterID {
			t.Errorf("usage %s: replayDeletion %v, labels %v", u.Name, *u.Spec.ReplayDeletion, u.Labels)
		}
		if len(u.Name) > 63 || names[u.Name] {
			t.Errorf("usage name %q too long or duplicate", u.Name)
		}
		names[u.Name] = true
	}
	var _ client.Object = usages[0]
}

func TestUsageNameIsBoundedAndDeterministic(t *testing.T) {
	long := crossplaneaws.UsagePair{
		Of: crossplaneaws.UsageRef{Name: "p-" + strings.Repeat("a", 60)},
		By: crossplaneaws.UsageRef{Name: "p-" + strings.Repeat("b", 60)},
	}
	other := long
	other.By.Name += "x"
	a, b := crossplaneaws.UsageName("p", long), crossplaneaws.UsageName("p", other)
	if len(a) > 63 || a != crossplaneaws.UsageName("p", long) || a == b {
		t.Errorf("names %q / %q: want <= 63, stable, distinct", a, b)
	}
	short := crossplaneaws.UsagePair{Of: crossplaneaws.UsageRef{Name: "p-node-role"}, By: crossplaneaws.UsageRef{Name: "p-default"}}
	if got := crossplaneaws.UsageName("p", short); got != "p-usage-node-role-by-default" {
		t.Errorf("short name = %q", got)
	}
}
