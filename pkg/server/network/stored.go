package network

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	netreconcile "github.com/yindia/cluster-vending/pkg/reconcile/network"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// stored is the API view of a network from the database alone: its spec,
// the status last recorded for it (remember) and the clusters using it
// now. GetNetwork and ListNetworks return it; nothing is read from the
// management cluster.
func (s *NetworkService) stored(ctx context.Context, row *model.Network) (*cloudv1.Network, error) {
	spec, err := netreconcile.SpecOf(row)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	st := &cloudv1.NetworkStatus{}
	if len(row.Status) > 0 {
		_ = netreconcile.Lenient.Unmarshal(row.Status, st) // written by remember; unreadable: start empty
	}
	if state := storedState(row.State, st.GetState()); state != st.GetState() {
		// The row moved on since the status was recorded (a change, a
		// delete or a takeover was stored): its message is about the past.
		st.State, st.Message = state, ""
	}
	clusters, err := s.deps.Store.Clusters().ListByNetwork(ctx, row.ID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	st.ClusterIds = nil
	for _, c := range clusters {
		st.ClusterIds = append(st.ClusterIds, c.Name)
	}
	return netreconcile.ToProto(row, spec, st), nil
}

// storedState is the state a stored network has: the recorded one, unless
// the row's lifecycle state says something happened since.
func storedState(rowState string, recorded cloudv1.NetworkState) cloudv1.NetworkState {
	own := cloudv1.NetworkState(cloudv1.NetworkState_value[rowState])
	switch {
	case recorded == cloudv1.NetworkState_NETWORK_STATE_UNSPECIFIED, rowState == netreconcile.StateDeleting, rowState == netreconcile.StateReady:
		// READY is stored only once the loop saw every resource ready; it
		// holds even if recording that pass's status failed.
		return own
	case recorded == cloudv1.NetworkState_NETWORK_STATE_READY && (rowState == netreconcile.StateProvisioning || rowState == netreconcile.StateUpdating):
		return own
	case recorded == cloudv1.NetworkState_NETWORK_STATE_IMPORTED && rowState != netreconcile.StateImported:
		return own
	default:
		return recorded
	}
}
