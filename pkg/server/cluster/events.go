package cluster

import (
	"context"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	recon "github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// ListClusterEvents returns a page of what the controller reported about
// the cluster, newest first: the Kubernetes events of its object, read
// from the management cluster through the store. They are kept for about
// an hour; the cluster's status.recent_events keeps the last ten.
func (s *ClusterService) ListClusterEvents(ctx context.Context, req *cloudv1.ListClusterEventsRequest) (_ *cloudv1.ListClusterEventsResponse, err error) {
	defer func() { err = rpcError(err) }()
	if err := service.Validate(req); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetClusterId())
	if err != nil {
		return nil, err
	}
	events, err := s.deps.Store.Events().ListFor(ctx, row.TenantID, recon.KindCluster, row.Name)
	if err != nil {
		s.logger().ErrorContext(ctx, "list cluster events", "cluster_id", row.ID, "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, "cannot read the cluster's events from the management cluster")
	}
	page, next, err := service.EventsPage(events, req.GetOnlyWarnings(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &cloudv1.ListClusterEventsResponse{Events: page, NextPageToken: next}, nil
}
