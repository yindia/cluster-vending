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
// ClusterAuth wait too: they would only report errors until then.
var AfterActiveKinds = map[string]bool{
	awseks.Addon_Kind: true, awseks.PodIdentityAssociation_Kind: true,
	awseks.AccessEntry_Kind: true, awseks.AccessPolicyAssociation_Kind: true,
	awseks.NodeGroup_Kind: true, awseks.ClusterAuth_Kind: true,
}

// afterActive reports whether obj is of one of AfterActiveKinds.
func afterActive(obj client.Object) bool {
	switch obj.(type) {
	case *awseks.Addon, *awseks.PodIdentityAssociation, *awseks.AccessEntry, *awseks.AccessPolicyAssociation,
		*awseks.NodeGroup, *awseks.ClusterAuth:
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
//     (DesiredSizeAtCreateOnly);
//   - a plan with objects that need a value of another resource (the
//     encryption key's ARN, the cluster's issuer or security group, a log
//     group that must exist first: FillLive) lists every object it will
//     have, and each is built at apply time from what exists then: an
//     object the render leaves out so far (Waits) waits, and says what
//     for. p's own late values are not used: the apply reads them.
func ClusterSet(p ClusterPlan) sequence.Set {
	full := p
	var late *lateRender
	if p.late() {
		// The cluster as it ends up: the identities of all its objects.
		full.AssumeLive()
		late = &lateRender{plan: p}
	}
	set := sequence.Of(ClusterResources(full)...)
	// Named, not looked up in the set: while the EKS cluster itself is
	// not rendered yet (ClusterPlan.Pending) what waits for it still does.
	eks := &awseks.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: clusterBuilder{p: p}.cluster().Name}}
	active := []sequence.Condition{sequence.Field(eks, EKSActive, "status", "atProvider", "status")}
	for i, item := range set {
		if late != nil {
			set[i].Build = late.build(item.Object)
		}
		switch obj := item.Object.(type) {
		case *awseks.NodeGroup:
			set[i].Adjust = DesiredSizeAtCreateOnly
			set[i].After = active
		case *protectionv1beta1.Usage:
			// After its "by" object, below.
		default:
			switch {
			case afterActive(obj):
				set[i].After = active
			case isHelmProviderConfig(obj):
				set[i].After = clusterBuilder{p: p}.afterAuth()
			}
		}
	}
	return usagesAfterTheirUsers(set, active)
}

// usagesAfterTheirUsers holds each Usage back as long as its "by" object
// is: a Usage whose user never exists is never garbage-collected and would
// block its object's deletion for good. A Usage whose user the set does
// not have waits for the EKS cluster, as before this rule was general.
func usagesAfterTheirUsers(set sequence.Set, active []sequence.Condition) sequence.Set {
	after := map[string][]sequence.Condition{}
	for _, item := range set {
		if _, usage := item.Object.(*protectionv1beta1.Usage); !usage {
			after[item.Object.GetObjectKind().GroupVersionKind().Kind+"/"+item.Object.GetName()] = item.After
			after[kindName(item.Object)+"/"+item.Object.GetName()] = item.After
		}
	}
	for i, item := range set {
		u, ok := item.Object.(*protectionv1beta1.Usage)
		if !ok {
			continue
		}
		by := u.Spec.By
		if by == nil || by.ResourceRef == nil {
			set[i].After = active
			continue
		}
		if conds, found := after[by.Kind+"/"+by.ResourceRef.Name]; found {
			set[i].After = conds
		} else if AfterActiveKinds[by.Kind] {
			set[i].After = active
		}
	}
	return set
}
