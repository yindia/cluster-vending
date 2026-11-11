package cluster

import (
	"context"
	"fmt"

	"connectrpc.com/connect/v2"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// WatchCluster streams the cluster's state and its managed resources'
// changes until it is deleted or the client cancels. Each read is a
// GetCluster, so watching moves creation and deletion on like --wait.
func (s *ClusterService) WatchCluster(ctx context.Context, req *cloudv1.WatchClusterRequest,
	stream cloudv1connect.ClusterServiceWatchClusterServerStream,
) error {
	if err := service.Validate(req); err != nil {
		return err
	}
	get := &cloudv1.GetClusterRequest{TenantId: req.GetTenantId(), ClusterId: req.GetClusterId()}
	read := func(ctx context.Context) (service.Snapshot[*cloudv1.Cluster], error) {
		res, err := s.GetCluster(ctx, get)
		if err != nil {
			if connect.CodeOf(err) != connect.CodeNotFound {
				s.logger().WarnContext(ctx, "watch cluster: read failed", "cluster_id", req.GetClusterId(), "error", err)
			}
			return service.Snapshot[*cloudv1.Cluster]{}, err
		}
		c := res.GetCluster()
		return service.Snapshot[*cloudv1.Cluster]{
			Resource: c, Resources: c.GetStatus().GetManagedResources(),
			Summary: fmt.Sprintf("%s|%s|%d", c.GetStatus().GetState(), c.GetStatus().GetMessage(), c.GetGeneration()),
		}, nil
	}
	send := func(m service.Message[*cloudv1.Cluster]) error {
		res := &cloudv1.WatchClusterResponse{Events: m.Events, Deleted: m.Deleted}
		if m.Changed {
			res.Cluster = m.Resource
		}
		return stream.Send(res)
	}
	return service.Watch(ctx, service.WatchInterval, read, send)
}
