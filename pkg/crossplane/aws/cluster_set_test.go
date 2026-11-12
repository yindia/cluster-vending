package aws_test

import (
	"testing"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// The set is the rendered objects in their order. Node groups, the
// ClusterAuth, add-ons, pod identities and access entries wait for the EKS
// cluster, and so do the Usages they are the user of; everything else
// waits for nothing. Only the node group
// is adjusted before it is sent.
func TestClusterSetHoldsWhatNeedsAnActiveCluster(t *testing.T) {
	plan := clusterPlan()
	objects := crossplaneaws.ClusterResources(plan)
	set := crossplaneaws.ClusterSet(plan)
	if len(set) != len(objects) {
		t.Fatalf("set of %d items for %d objects", len(set), len(objects))
	}
	waiting := map[string]int{}
	for i, item := range set {
		if item.Object.GetName() != objects[i].GetName() {
			t.Errorf("item %d is %s, want %s", i, item.Object.GetName(), objects[i].GetName())
		}
		wants, kind := false, ""
		switch obj := item.Object.(type) {
		case *awseks.Addon:
			wants, kind = true, awseks.Addon_Kind
		case *awseks.PodIdentityAssociation:
			wants, kind = true, awseks.PodIdentityAssociation_Kind
		case *awseks.AccessEntry:
			wants, kind = true, awseks.AccessEntry_Kind
		case *awseks.AccessPolicyAssociation:
			wants, kind = true, awseks.AccessPolicyAssociation_Kind
		case *awseks.NodeGroup:
			wants, kind = true, awseks.NodeGroup_Kind
		case *awseks.ClusterAuth:
			wants, kind = true, awseks.ClusterAuth_Kind
		case *unstructured.Unstructured: // the helm ProviderConfig: after the ClusterAuth is Ready
			wants, kind = obj.GroupVersionKind() == crossplaneaws.HelmProviderConfigGVK, "helm ProviderConfig"
		case *protectionv1beta1.Usage:
			// A Usage waits as its "by" object does.
			wants = crossplaneaws.AfterActiveKinds[obj.Spec.By.Kind] || obj.Spec.By.APIVersion == crossplaneaws.HelmProviderConfigGVK.GroupVersion().String()
			kind = "Usage by " + obj.Spec.By.Kind
		}
		if (len(item.After) == 1) != wants {
			t.Errorf("%T %s waits for %d conditions, want waiting = %t", item.Object, item.Object.GetName(), len(item.After), wants)
		}
		if wants {
			waiting[kind]++
		}
		if _, isNodeGroup := item.Object.(*awseks.NodeGroup); (item.Adjust != nil) != isNodeGroup {
			t.Errorf("%T %s adjusted = %t", item.Object, item.Object.GetName(), item.Adjust != nil)
		}
	}
	for _, kind := range []string{awseks.Addon_Kind, awseks.PodIdentityAssociation_Kind, awseks.AccessEntry_Kind,
		awseks.AccessPolicyAssociation_Kind, "Usage by " + awseks.Addon_Kind} {
		if waiting[kind] == 0 {
			t.Errorf("no %s in the set waits for the EKS cluster (waiting: %v)", kind, waiting)
		}
	}
}
