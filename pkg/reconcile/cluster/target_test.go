package cluster

import (
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/repository/mock"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// A new cluster goes only into a network that exists, is the platform's,
// in the cluster's region, and Ready (or being updated): one still being
// created has no subnets to choose yet.
func TestNewClusterNetwork(t *testing.T) {
	const networkID = "0190a000-0000-7000-8000-000000000001"
	tests := []struct {
		name    string
		state   string // empty: no such network
		region  string
		kind    fault.Kind // 0: allowed
		message string
	}{
		{name: "ready", state: "NETWORK_STATE_READY", region: "us-east-1"},
		{name: "updating", state: "NETWORK_STATE_UPDATING", region: "us-east-1"},
		{name: "provisioning", state: "NETWORK_STATE_PROVISIONING", region: "us-east-1", kind: fault.FailedPrecondition,
			message: "network " + networkID + " is still being created; a cluster can be created once it is Ready"},
		{name: "unspecified", state: "NETWORK_STATE_UNSPECIFIED", region: "us-east-1", kind: fault.FailedPrecondition,
			message: "network " + networkID + " is still being created; a cluster can be created once it is Ready"},
		{name: "missing", kind: fault.NotFound, message: "tenant acme has no network " + networkID},
		{name: "another region", state: "NETWORK_STATE_READY", region: "eu-west-1", kind: fault.Invalid,
			message: "network " + networkID + " is aws/eu-west-1; the cluster is aws/us-east-1"},
		{name: "imported", state: "NETWORK_STATE_IMPORTED", region: "us-east-1", kind: fault.FailedPrecondition,
			message: "network " + networkID + " is NETWORK_STATE_IMPORTED; clusters need a network the platform owns"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			store, networks := mock.NewMockStore(ctrl), mock.NewMockNetworks(ctrl)
			store.EXPECT().Networks().Return(networks).AnyTimes()
			if tt.state == "" {
				networks.EXPECT().Get(gomock.Any(), "acme", networkID).Return(nil, repository.ErrNotFound)
			} else {
				networks.EXPECT().Get(gomock.Any(), "acme", networkID).Return(&model.Network{
					Resource: model.Resource{Base: model.Base{ID: networkID}, State: tt.state},
					Provider: model.ProviderAWS, Region: tt.region}, nil)
			}
			spec := &cloudv1.ClusterSpec{Region: "us-east-1", Network: &cloudv1.ClusterNetwork{NetworkId: networkID}}

			n, err := New(reconcile.Deps{Store: store}).newClusterNetwork(t.Context(), "acme", spec)

			if tt.kind == 0 {
				if err != nil || n == nil || n.ID != networkID {
					t.Fatalf("network %v, err %v; want it allowed", n, err)
				}
				return
			}
			if !fault.Is(err, tt.kind) || err.Error() != tt.message {
				t.Errorf("err = %v (kind %v), want kind %d: %q", err, err, tt.kind, tt.message)
			}
		})
	}
}
