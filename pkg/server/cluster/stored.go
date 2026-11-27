package cluster

import (
	"context"
	"slices"

	"connectrpc.com/connect/v2"

	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/service"

	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

func (s *ClusterService) stored(ctx context.Context, row *model.Cluster) (*cloudv1.Cluster, error) {
	cluster, err := stored(row)
	if err != nil {
		return nil, err
	}
	tier, err := s.deps.NetworkTier(ctx, row.TenantID, row.NetworkID)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	cluster.Status.Tier = tier
	cluster.Spec.ProtoReflect().Clear(cluster.Spec.ProtoReflect().Descriptor().Fields().ByName("tier"))
	frozen, err := s.deps.FreezeFor(ctx, platform.FreezeTarget{TenantID: row.TenantID, NetworkID: row.NetworkID, ClusterID: row.ID})
	if err != nil {
		return nil, service.ToConnect(err)
	}
	cluster.Status.Message = platform.WithoutFreezeMessage(cluster.Status.Message, cluster.Status.Freeze)
	cluster.Status.Freeze = platform.FreezeProto(frozen)
	cluster.Status.Conditions = slices.DeleteFunc(cluster.Status.Conditions, func(condition *cloudv1.Condition) bool { return condition.GetType() == platform.ConditionFrozen })
	if condition, _ := platform.RowFreeze(frozen, nil); condition != nil {
		cluster.Status.Conditions = append(cluster.Status.Conditions, condition)
	}
	withControlPlaneLogs(cluster)

	return cluster, nil
}

// stored is the API view of a cluster from its stored Kubernetes state: its spec
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
	st.Recommendations = recommendationSummary(row)
	st.Rollout = rolloutOf(row)
	convert.WithSummary(st, reconcilecluster.StoredSummary(row))
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
