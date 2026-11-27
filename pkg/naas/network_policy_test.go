package naas

import (
	"errors"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/kube"
)

const enforcedVPCCNI = `{"enableNetworkPolicy":"true","nodeAgent":{"enabled":true}}`

// vpcCNIAddonFixture is the parent cluster's vpc-cni add-on as the
// provider reports it once EKS applied want and observed: Ready and Synced.
func vpcCNIAddonFixture(want, observed string) *awseks.Addon {
	addon := &awseks.Addon{ObjectMeta: metav1.ObjectMeta{Name: "prod-addon-vpc-cni", Namespace: "tenant-acme", Labels: crossplane.ClusterLabels("acme", "cluster-id")},
		Spec:   awseks.AddonSpec{ForProvider: awseks.AddonParameters{AddonName: ptr.To("vpc-cni"), ConfigurationValues: ptr.To(want)}},
		Status: awseks.AddonStatus{AtProvider: awseks.AddonObservation{AddonName: ptr.To("vpc-cni"), ConfigurationValues: ptr.To(observed)}}}
	addon.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
	return addon
}

func TestNetworkPolicyEnforcementWaitsForTheVPCCNIAddon(t *testing.T) {
	parent := &cloud.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "tenant-acme", Labels: map[string]string{cloudstore.LabelTenant: "acme", cloudstore.LabelID: "cluster-id"}}}
	coredns := vpcCNIAddonFixture("", "")
	coredns.Name, coredns.Spec.ForProvider.AddonName, coredns.Status.AtProvider.AddonName = "prod-addon-coredns", ptr.To("coredns"), ptr.To("coredns")
	other := vpcCNIAddonFixture(enforcedVPCCNI, enforcedVPCCNI)
	other.Name, other.Labels = "other-addon-vpc-cni", crossplane.ClusterLabels("acme", "another-cluster")
	for _, test := range []struct {
		name    string
		objects func() []client.Object
		want    error
	}{
		{name: "Ready, Synced and enforcing", want: nil,
			objects: func() []client.Object {
				return []client.Object{vpcCNIAddonFixture(enforcedVPCCNI, enforcedVPCCNI), coredns}
			}},
		{name: "no vpc-cni add-on of this cluster", want: ErrNetworkPolicyNotEnforced,
			objects: func() []client.Object { return []client.Object{coredns, other} }},
		{name: "neither asked for nor observed", want: ErrNetworkPolicyNotEnforced,
			objects: func() []client.Object { return []client.Object{vpcCNIAddonFixture("", "")} }},
		{name: "asked for, EKS not there yet", want: ErrNetworkPolicyPending,
			objects: func() []client.Object { return []client.Object{vpcCNIAddonFixture(enforcedVPCCNI, "")} }},
		{name: "observed but not Ready", want: ErrNetworkPolicyPending,
			objects: func() []client.Object {
				addon := vpcCNIAddonFixture(enforcedVPCCNI, enforcedVPCCNI)
				addon.SetConditions(xpv2.Creating())
				return []client.Object{addon}
			}},
		{name: "observed but not Synced", want: ErrNetworkPolicyPending,
			objects: func() []client.Object {
				addon := vpcCNIAddonFixture(enforcedVPCCNI, enforcedVPCCNI)
				addon.SetConditions(xpv2.ReconcileError(errors.New("throttled")))
				return []client.Object{addon}
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme, err := kube.Scheme()
			if err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(test.objects()...).Build()

			err = ObserveNetworkPolicyEnforcement(t.Context(), c, parent)

			if test.want == nil && err != nil || test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("err = %v, want %v", err, test.want)
			}
		})
	}
}
