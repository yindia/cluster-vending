package cluster

import (
	"testing"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The live reader's identity never holds the cluster back (PROP-02 7.1): an
// account without the reader stack has a live ClusterAuth that is not Ready,
// or an access entry EKS refuses, and its clusters still run.
func TestStatusIgnoresTheLiveIdentity(t *testing.T) {
	row := &model.Cluster{TenantID: "acme", Name: "prod", Resource: model.Resource{Base: model.Base{ID: "c-1"}, State: StateProvisioning}}
	prefix := crossplaneaws.ClusterPrefix(row.TenantID, row.ID, row.Name)
	ready := &cloudv1.ManagedResource{Kind: "Cluster", Name: prefix, State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY}
	auth := func(state cloudv1.ManagedResourceState) *cloudv1.ManagedResource {
		return &cloudv1.ManagedResource{Kind: "ClusterAuth", Name: crossplaneaws.LiveClusterAuthName(prefix), State: state}
	}
	entry := func(state cloudv1.ManagedResourceState) *cloudv1.ManagedResource {
		return &cloudv1.ManagedResource{Kind: "AccessEntry", Name: crossplaneaws.LiveAccessEntryName(prefix), State: state, Message: "invalid principal"}
	}
	tests := []struct {
		name  string
		views []*cloudv1.ManagedResource
		want  cloudv1.ClusterState
	}{
		{name: "live identity ready", views: []*cloudv1.ManagedResource{ready, auth(cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY)},
			want: cloudv1.ClusterState_CLUSTER_STATE_RUNNING},
		{name: "live ClusterAuth not ready", views: []*cloudv1.ManagedResource{ready, auth(cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING)},
			want: cloudv1.ClusterState_CLUSTER_STATE_RUNNING},
		{name: "live access entry failed", views: []*cloudv1.ManagedResource{ready, entry(cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED)},
			want: cloudv1.ClusterState_CLUSTER_STATE_RUNNING},
		{name: "another object still counts", views: []*cloudv1.ManagedResource{ready,
			{Kind: "ClusterAuth", Name: prefix + "-auth", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING}},
			want: cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := status(row, tt.views, false)
			if st.GetState() != tt.want {
				t.Fatalf("state = %v (%s), want %v", st.GetState(), st.GetMessage(), tt.want)
			}
			if len(st.GetManagedResources()) != len(tt.views) {
				t.Fatalf("the live identity is still listed: %v", st.GetManagedResources())
			}
		})
	}
}
