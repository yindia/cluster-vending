package reconcile_test

import (
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// A recorded object that is gone reads as what it means to a user: deleted
// while its owner is being deleted, else deleted outside the platform.
func TestExplainMissing(t *testing.T) {
	missing := func() *cloudv1.ManagedResource {
		return &cloudv1.ManagedResource{Kind: "Subnet", Name: "s", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING,
			Message: "not found in the management cluster"}
	}
	ready := &cloudv1.ManagedResource{Kind: "VPC", Name: "v", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY}
	tests := []struct {
		name     string
		deleting bool
		want     string
	}{
		{"while the owner is deleted", true, "deleted"},
		{"otherwise", false, "deleted outside the platform; it is created again unless its cloud ID is assigned by AWS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := []*cloudv1.ManagedResource{missing(), ready}

			out := reconcile.ExplainMissing(in, tt.deleting)

			if out[0].GetMessage() != tt.want || out[1] != ready {
				t.Fatalf("got %q, %v", out[0].GetMessage(), out[1])
			}
			if in[0].GetMessage() != "not found in the management cluster" {
				t.Error("the input was changed")
			}
		})
	}
}
