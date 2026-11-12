package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Group and Version of the platform's custom resources.
const (
	Group   = "platform.cluster-vending.io"
	Version = "v1alpha1"
)

// GroupVersion is the group and version of every kind in this package.
var GroupVersion = schema.GroupVersion{Group: Group, Version: Version}

// Labels on the platform's objects, for label selections: the tenant, the
// cloud account, and the row an object belongs to (a cluster's network, a
// node pool's cluster, a managed resource's owner).
const (
	LabelTenant       = Group + "/tenant"
	LabelCloudAccount = Group + "/cloud-account"
	LabelNetwork      = Group + "/network"
	LabelCluster      = Group + "/cluster"
	LabelOwnerKind    = Group + "/owner-kind"
	LabelOwner        = Group + "/owner"
)

var schemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme registers the kinds of this package.
var AddToScheme = schemeBuilder.AddToScheme

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&Tenant{}, &TenantList{}, &CloudAccount{}, &CloudAccountList{},
		&Network{}, &NetworkList{}, &Cluster{}, &ClusterList{},
		&NodePool{}, &NodePoolList{}, &ManagedResource{}, &ManagedResourceList{},
		&Claim{}, &ClaimList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
