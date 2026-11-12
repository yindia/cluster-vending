package aws

import (
	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
)

// EKSActive is the EKS cluster status (status.atProvider.status) once the
// cluster can be used.
const EKSActive = "ACTIVE"

// AfterActiveKinds are the kinds created only once the EKS cluster is
// ACTIVE. Their references resolve as soon as the cluster has a name (set
// at create), so applied together with it they would fail for the 10+
// minutes EKS takes and turn the cluster FAILED. Node groups and the
// ClusterAuth wait for ACTIVE by themselves.
var AfterActiveKinds = map[string]bool{
	awseks.Addon_Kind: true, awseks.PodIdentityAssociation_Kind: true,
	awseks.AccessEntry_Kind: true, awseks.AccessPolicyAssociation_Kind: true,
}

// afterActive reports whether obj is of one of AfterActiveKinds.
func afterActive(obj client.Object) bool {
	switch obj.(type) {
	case *awseks.Addon, *awseks.PodIdentityAssociation, *awseks.AccessEntry, *awseks.AccessPolicyAssociation:
		return true
	}
	return false
}

// ClusterSet is ClusterResources as a set to apply in order:
//
//   - the objects of AfterActiveKinds wait for the EKS cluster to be
//     ACTIVE;
//   - a Usage whose "by" object waits is held back with it: a Usage whose
//     user never exists is never garbage-collected and would block its
//     object's deletion for good;
//   - the node group is applied with its desired size at creation only
//     (DesiredSizeAtCreateOnly).
func ClusterSet(p ClusterPlan) sequence.Set {
	set := sequence.Of(ClusterResources(p)...)
	// Named, not looked up in the set: while the EKS cluster itself is
	// not rendered yet (ClusterPlan.Pending) what waits for it still does.
	eks := &awseks.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: clusterBuilder{p: p}.cluster().Name}}
	active := []sequence.Condition{sequence.Field(eks, EKSActive, "status", "atProvider", "status")}
	for i, item := range set {
		switch obj := item.Object.(type) {
		case *awseks.NodeGroup:
			set[i].Adjust = DesiredSizeAtCreateOnly
		case *protectionv1beta1.Usage:
			if by := obj.Spec.By; by == nil || by.ResourceRef == nil || AfterActiveKinds[by.Kind] {
				set[i].After = active
			}
		default:
			if afterActive(obj) {
				set[i].After = active
			}
		}
	}
	return set
}
