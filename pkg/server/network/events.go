package network

import (
	"context"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// ListNetworkEvents returns a page of what the controller reported about
// the network, newest first: the Kubernetes events of its object, read
// from the management cluster through the store. They are kept for about
// an hour; the network's status.recent_events keeps the last ten.
func (s *NetworkService) ListNetworkEvents(ctx context.Context, req *cloudv1.ListNetworkEventsRequest) (*cloudv1.ListNetworkEventsResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, service.ToConnect(err)
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetNetworkId())
	if err != nil {
		return nil, service.ToConnect(err)
	}
	events, err := s.deps.Store.Events().ListFor(ctx, row.TenantID, reconcile.KindNetwork, row.Name)
	if err != nil {
		s.logger().ErrorContext(ctx, "list network events", "network_id", row.ID, "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, "cannot read the network's events from the management cluster")
	}
	page, next, err := service.EventsPage(events, req.GetOnlyWarnings(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, service.ToConnect(err)
	}
	return &cloudv1.ListNetworkEventsResponse{Events: page, NextPageToken: next}, nil
}
