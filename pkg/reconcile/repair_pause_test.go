package reconcile

import (
	"testing"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Nothing is created again while the owner is paused: a new object would
// not carry the pause, and its provider would act during the upgrade the
// operator paused for.
func TestNothingIsRepairedWhilePaused(t *testing.T) {
	const (
		missing = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING
		ready   = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
	)
	records := []model.ManagedResource{
		{APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: "NodeGroup", Name: "prod-default"},
		{APIVersion: "iam.aws.m.upbound.io/v1beta1", Kind: "Role", Name: "prod-node-role"},
	}
	tests := []struct {
		name  string
		views []*cloudv1.ManagedResource
		want  int
	}{
		{"a missing object, nothing paused", []*cloudv1.ManagedResource{{State: missing}, {State: ready}}, 1},
		{"a missing object while another is paused", []*cloudv1.ManagedResource{{State: missing}, {State: ready, Paused: true}}, 0},
		{"nothing missing", []*cloudv1.ManagedResource{{State: ready}, {State: ready}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := repairable(records, tt.views)

			if len(got) != tt.want {
				t.Fatalf("repairable = %v, want %d objects", got, tt.want)
			}
		})
	}
}
