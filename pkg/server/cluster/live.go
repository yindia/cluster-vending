package cluster

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	liveread "github.com/yindia/cluster-vending/pkg/live"
)

// liveReads are how the service reads the workload clusters live (PROP-02
// 5.5, option D): through the live reader, never itself. The API holds no
// workload credentials.
type liveReads struct {
	client liveread.Client
	// refused: the API runs without authentication (auth.mode none), where a
	// caller could name any tenant, and live.allowWithoutAuth is not set
	// (7.5): the live RPCs answer FAILED_PRECONDITION.
	refused bool
}

// WithLive makes the node lists and GetNodeResources live, read by the live
// reader c. refused (auth.mode none without live.allowWithoutAuth) answers
// them FAILED_PRECONDITION "live reads need authentication". Without it the
// node lists are the stored ones and GetNodeResources is UNIMPLEMENTED.
func WithLive(c liveread.Client, refused bool) Option {
	return func(s *ClusterService) { s.live = &liveReads{client: c, refused: refused} }
}

func errLiveRefused() error {
	return liveread.ToConnect(&liveread.Error{Reason: liveread.ReasonAuthRequired, Message: liveread.MessageAuthRequired})
}

// liveNodes is ListNodeGroupNodes read live: the API has checked the tenant
// and the cluster (row) and the group; the live reader resolves the rest
// from the tenant and cluster IDs alone.
func (s *ClusterService) liveNodes(ctx context.Context, row *model.Cluster, req *cloudv1.ListNodeGroupNodesRequest) (*cloudv1.ListNodeGroupNodesResponse, error) {
	if s.live.refused {
		return nil, errLiveRefused()
	}
	if known, _ := nodeGroupKnown(row, req.GetNodeGroup()); !known {
		return nil, connect.NewError(connect.CodeNotFound, "cluster "+req.GetClusterId()+" has no node group "+req.GetNodeGroup())
	}
	res, err := s.live.client.ListNodeGroupNodes(ctx, req)
	if err != nil {
		return nil, liveread.Forward(err)
	}
	return res, nil
}

// liveNodeResources is GetNodeResources read live.
func (s *ClusterService) liveNodeResources(ctx context.Context, req *cloudv1.GetNodeResourcesRequest) (*cloudv1.GetNodeResourcesResponse, error) {
	if s.live.refused {
		return nil, errLiveRefused()
	}
	res, err := s.live.client.GetNodeResources(ctx, req)
	if err != nil {
		return nil, liveread.Forward(err)
	}
	return res, nil
}
