package cloudstore_test

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
)

func TestInventoryPreservesProviderIdentityInKubernetesStatus(t *testing.T) {
	w := newWorld(t)
	for _, version := range []string{"ec2.aws.m.upbound.io/v1beta1", "compute.gcp.m.upbound.io/v1beta1"} {
		if err := w.s.ManagedResources().Put(t.Context(), []model.ManagedResource{{TenantID: "acme", CloudAccountID: w.account, OwnerKind: model.OwnerNetwork, OwnerID: w.network.ID, APIVersion: version, Kind: "Network", Namespace: "tenant-acme", Name: "shared"}}); err != nil {
			t.Fatal(err)
		}
	}
	network := &cloud.Network{}
	if err := w.k.Get(t.Context(), client.ObjectKey{Namespace: "tenant-acme", Name: w.network.Name}, network); err != nil {
		t.Fatal(err)
	}
	if len(network.Status.Inventory) != 2 || network.Status.Inventory[0].APIVersion == network.Status.Inventory[1].APIVersion {
		t.Fatalf("provider identities merged: %+v", network.Status.Inventory)
	}
	records, err := w.s.ManagedResources().ListByOwner(t.Context(), model.OwnerNetwork, w.network.ID)
	if err != nil || len(records) != 2 || records[0].ID == records[1].ID {
		t.Fatalf("inventory IDs: %+v %v", records, err)
	}
}
