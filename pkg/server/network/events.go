package network

import (
	"context"

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
		return nil, service.ToConnect(err)
	}
	page, next, err := service.EventsPage(events, req.GetOnlyWarnings(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, service.ToConnect(err)
	}
	return &cloudv1.ListNetworkEventsResponse{Events: page, NextPageToken: next}, nil
}
