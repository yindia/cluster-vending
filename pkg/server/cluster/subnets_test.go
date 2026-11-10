package cluster

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The server-side apply itself (tags merged per key next to the network's,
// removed by applying no fields) is not covered here: the fake client
// round-trips applies through the provider's Go types, which send
// forProvider.region (no omitempty) as null and conflict. It was checked
// against the API server (kubectl apply --server-side --dry-run=server).

func TestObjectNames(t *testing.T) {
	subnets := []privateSubnet{{"subnet-1", "az1", "net-private-az1"}, {"subnet-2", "az2", "net-private-az2"}}
	got := objectNames(subnets, []string{"subnet-2", "subnet-1"})
	if !equal(got, []string{"net-private-az2", "net-private-az1"}) {
		t.Errorf("objectNames = %v", got)
	}
}

func TestManagedBy(t *testing.T) {
	fields := []metav1.ManagedFieldsEntry{{Manager: "cluster-vending"}, {Manager: subnetManager("c-1")}}
	if !managedBy(fields, subnetManager("c-1")) || managedBy(fields, subnetManager("c-2")) {
		t.Error("managedBy does not match the cluster's manager exactly")
	}
}
