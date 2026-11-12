package cluster

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// clusterProto is the API view of a stored cluster with status st.
func clusterProto(row *model.Cluster, spec *cloudv1.ClusterSpec, st *cloudv1.ClusterStatus) *cloudv1.Cluster {
	return &cloudv1.Cluster{
		TenantId: row.TenantID, Id: row.ID, Name: row.Name, DisplayName: row.DisplayName, Labels: row.Labels,
		Spec: spec, Status: st, Generation: row.Generation, Etag: row.ETag,
		CreateTime: timestamppb.New(row.CreatedAt), UpdateTime: timestamppb.New(row.UpdatedAt),
	}
}

// stored is the API view of a cluster from the database alone: its spec
// and the status last recorded for it (remember). GetCluster and
// ListClusters return it; nothing is read from the management cluster.
func stored(row *model.Cluster) (*cloudv1.Cluster, error) {
	spec, err := specOf(row)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	st := &cloudv1.ClusterStatus{}
	if len(row.Status) > 0 {
		_ = lenient.Unmarshal(row.Status, st) // written by remember; unreadable: start empty
	}
	if state := storedState(row.State, st.GetState()); state != st.GetState() {
		// The row moved on since the status was recorded (a change, a
		// delete or a takeover was stored): its message is about the past.
		st.State, st.Message = state, ""
	}
	if st.GetObservedGeneration() == 0 {
		st.ObservedGeneration = row.ObservedGeneration
	}
	return clusterProto(row, spec, st), nil
}

// storedState is the state a stored cluster has: the recorded one, unless
// the row's lifecycle state says something happened since.
func storedState(rowState string, recorded cloudv1.ClusterState) cloudv1.ClusterState {
	own := cloudv1.ClusterState(cloudv1.ClusterState_value[rowState])
	switch {
	case recorded == cloudv1.ClusterState_CLUSTER_STATE_UNSPECIFIED, rowState == stateDeleting:
		return own
	case recorded == cloudv1.ClusterState_CLUSTER_STATE_RUNNING && (rowState == stateUpdating || rowState == stateProvisioning):
		return own
	case recorded == cloudv1.ClusterState_CLUSTER_STATE_IMPORTED && rowState != stateImported:
		return own
	default:
		return recorded
	}
}

// remember records a cluster's status as observed now, for GetCluster and
// ListClusters. It writes the status column only: the etag does not
// change. A cluster that is gone has nothing to record.
func (s *ClusterService) remember(ctx context.Context, row *model.Cluster, st *cloudv1.ClusterStatus) error {
	raw, err := encode(st)
	if err != nil {
		return err
	}
	if string(raw) == string(row.Status) {
		return nil
	}
	err = s.deps.Store.Clusters().SetStatus(ctx, row.TenantID, row.ID, raw)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err == nil {
		row.Status = raw
	}
	return err
}

// respond is the answer of a call that changed the cluster: its view with
// the live status, which is recorded on the way.
func (s *ClusterService) respond(ctx context.Context, row *model.Cluster) (*cloudv1.Cluster, error) {
	c, err := s.view(ctx, row)
	if err != nil {
		return nil, err
	}
	if err := s.remember(ctx, row, c.GetStatus()); err != nil {
		s.logger().WarnContext(ctx, "record cluster status", "cluster_id", row.ID, "error", err)
	}
	return c, nil
}

// GetClusterStatus returns one cluster with its status read live from the
// management cluster. Like every read it changes nothing.
func (s *ClusterService) GetClusterStatus(ctx context.Context, req *cloudv1.GetClusterStatusRequest) (*cloudv1.GetClusterStatusResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	c, err := s.view(ctx, row)
	if err != nil {
		return nil, err
	}
	return &cloudv1.GetClusterStatusResponse{Cluster: c}, nil
}
