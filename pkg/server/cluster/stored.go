package cluster

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// stored is the API view of a cluster from the database alone: its spec
// and the status last recorded for it (remember). GetCluster and
// ListClusters return it; nothing is read from the management cluster.
func stored(row *model.Cluster) (*cloudv1.Cluster, error) {
	spec, err := reconcilecluster.SpecOf(row)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	st := &cloudv1.ClusterStatus{}
	if len(row.Status) > 0 {
		_ = reconcilecluster.Lenient.Unmarshal(row.Status, st) // written by remember; unreadable: start empty
	}
	if state := storedState(row.State, st.GetState()); state != st.GetState() {
		// The row moved on since the status was recorded (a change, a
		// delete or a takeover was stored): its message is about the past.
		st.State, st.Message = state, ""
	}
	if st.GetObservedGeneration() == 0 {
		st.ObservedGeneration = row.ObservedGeneration
	}
	st.Deprecations = deprecationSummary(row)
	return reconcilecluster.ClusterProto(row, spec, st), nil
}

// storedState is the state a stored cluster has: the recorded one, unless
// the row's lifecycle state says something happened since.
func storedState(rowState string, recorded cloudv1.ClusterState) cloudv1.ClusterState {
	own := cloudv1.ClusterState(cloudv1.ClusterState_value[rowState])
	switch {
	case recorded == cloudv1.ClusterState_CLUSTER_STATE_UNSPECIFIED, rowState == reconcilecluster.StateDeleting, rowState == reconcilecluster.StateRunning:
		// RUNNING is stored only once the loop saw the cluster converged; it
		// holds even if recording that pass's status failed.
		return own
	case recorded == cloudv1.ClusterState_CLUSTER_STATE_RUNNING && (rowState == reconcilecluster.StateUpdating || rowState == reconcilecluster.StateProvisioning):
		return own
	case recorded == cloudv1.ClusterState_CLUSTER_STATE_IMPORTED && rowState != reconcilecluster.StateImported:
		return own
	default:
		return recorded
	}
}

// GetClusterStatus returns one cluster with its status read live from the
// management cluster. Like every read it changes nothing.
func (s *ClusterService) GetClusterStatus(ctx context.Context, req *cloudv1.GetClusterStatusRequest) (_ *cloudv1.GetClusterStatusResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	c, err := s.View(ctx, row)
	if err != nil {
		return nil, err
	}
	return &cloudv1.GetClusterStatusResponse{Cluster: c}, nil
}
