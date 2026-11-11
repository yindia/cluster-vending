package network

import (
	"context"
	"fmt"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// WatchNetwork streams the network's state and its managed resources'
// changes until it is deleted or the client cancels. Each read is a
// GetNetwork, so watching moves a deletion on like --wait.
func (s *NetworkService) WatchNetwork(ctx context.Context, req *cloudv1.WatchNetworkRequest,
	stream cloudv1connect.NetworkServiceWatchNetworkServerStream,
) error {
	if err := service.Validate(req); err != nil {
		return err
	}
	get := &cloudv1.GetNetworkRequest{TenantId: req.GetTenantId(), NetworkId: req.GetNetworkId()}
	read := func(ctx context.Context) (service.Snapshot[*cloudv1.Network], error) {
		res, err := s.GetNetwork(ctx, get)
		if err != nil {
			if connect.CodeOf(err) != connect.CodeNotFound {
				s.logger().WarnContext(ctx, "watch network: read failed", "network_id", req.GetNetworkId(), "error", err)
			}
			return service.Snapshot[*cloudv1.Network]{}, err
		}
		n := res.GetNetwork()
		return service.Snapshot[*cloudv1.Network]{
			Resource: n, Resources: n.GetStatus().GetManagedResources(),
			Summary: fmt.Sprintf("%s|%s|%d", n.GetStatus().GetState(), n.GetStatus().GetMessage(), n.GetGeneration()),
		}, nil
	}
	send := func(m service.Message[*cloudv1.Network]) error {
		res := &cloudv1.WatchNetworkResponse{Events: m.Events, Deleted: m.Deleted}
		if m.Changed {
			res.Network = m.Resource
		}
		return stream.Send(res)
	}
	return service.Watch(ctx, service.WatchInterval, read, send)
}
