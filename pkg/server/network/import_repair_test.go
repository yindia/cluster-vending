package network

import (
	"slices"
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// subnets are the network's Subnet objects.
func (f fixture) subnets(t *testing.T, network string) []awsec2.Subnet {
	t.Helper()
	list := &awsec2.SubnetList{}
	if err := f.kube.List(t.Context(), list, client.InNamespace("tenant-acme"),
		client.MatchingLabels{crossplane.LabelNetwork: network}); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// A subnet object of an imported network deleted from the management
// cluster is created again by the next passes, observing the same subnet:
// with its ID and only Observe, as the import made it. A subnet's ID is
// AWS's, so a new object without it would be a second subnet once taken
// over.
func TestRepairOfAnImportedNetworkObservesTheSameSubnet(t *testing.T) {
	for _, takeOver := range []bool{false, true} {
		name := "imported"
		if takeOver {
			name = "taken over"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.svc.deps.AWSDiscovery = fakeDiscoverer{network: existingVPC()}
			res, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID))
			if err != nil {
				t.Fatal(err)
			}
			id := res.GetNetwork().GetId()
			f.reconcile(t, id)
			f.reconcile(t, id)
			if takeOver {
				if _, err := f.svc.TakeOverNetwork(t.Context(), &cloudv1.TakeOverNetworkRequest{TenantId: tenant, NetworkId: id,
					AcknowledgeNotImported: true}); err != nil {
					t.Fatalf("TakeOverNetwork: %v", err)
				}
				f.reconcile(t, id)
				f.reconcile(t, id)
			}
			subnets := f.subnets(t, id)
			if len(subnets) == 0 {
				t.Fatal("the import made no subnets")
			}
			was := subnets[0]
			// The record keeps the cloud identity (as Observe stores it).
			f.reconcile(t, id)
			if err := f.kube.Delete(t.Context(), &was); err != nil {
				t.Fatal(err)
			}

			f.reconcile(t, id)
			f.reconcile(t, id)

			i := slices.IndexFunc(f.subnets(t, id), func(s awsec2.Subnet) bool { return s.Name == was.Name })
			if i < 0 {
				t.Fatalf("subnet %s was not created again", was.Name)
			}
			now := f.subnets(t, id)[i]
			if a, b := was.Annotations[crossplaneaws.ExternalNameAnnotation], now.Annotations[crossplaneaws.ExternalNameAnnotation]; a == "" || a != b {
				t.Errorf("external name %q became %q: another subnet", a, b)
			}
			if !slices.Equal(now.Spec.ManagementPolicies, was.Spec.ManagementPolicies) {
				t.Errorf("recreated with managementPolicies %v, want %v", now.Spec.ManagementPolicies, was.Spec.ManagementPolicies)
			}
			if got := len(f.subnets(t, id)); got != len(subnets) {
				t.Errorf("%d subnets after the repair, want %d", got, len(subnets))
			}
		})
	}
}
