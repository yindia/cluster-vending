package cli

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// ClusterCreate calls ClusterService.CreateCluster with the Cluster in the
// input file; the argument, when given, is its name. The request ID is
// derived from the request, so running the same create twice returns the
// first cluster instead of building a second one.
func ClusterCreate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	n := &cloudv1.Cluster{}
	if err := LoadInput(o.Input, n); err != nil {
		return nil, err
	}
	if n.GetId() != "" {
		return nil, fmt.Errorf("--config: id %q: a cluster's id is assigned by the server; set name instead", n.GetId())
	}
	if o.ID != "" {
		n.Name = o.ID
	}
	req := &cloudv1.CreateClusterRequest{TenantId: o.Tenant, Cluster: n}
	id, err := requestID(o.Tenant, req)
	if err != nil {
		return nil, err
	}
	req.RequestId = id
	return call(ctx, req, c.Cluster.CreateCluster)
}

// ClusterGet calls ClusterService.GetCluster.
func ClusterGet(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.GetClusterRequest{TenantId: o.Tenant, ClusterId: o.ID}, c.Cluster.GetCluster)
}

// ClusterList calls ClusterService.ListClusters.
func ClusterList(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.ListClustersRequest{
		TenantId: o.Tenant, PageSize: o.PageSize, PageToken: o.PageToken,
	}, c.Cluster.ListClusters)
}

// ClusterUpdate calls ClusterService.UpdateCluster with the Cluster in the
// input file and --update-mask.
func ClusterUpdate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	n, err := clusterInput(o)
	if err != nil {
		return nil, err
	}
	if o.ETag != "" {
		n.Etag = o.ETag
	}
	return call(ctx, &cloudv1.UpdateClusterRequest{
		TenantId: o.Tenant, Cluster: n, UpdateMask: &fieldmaskpb.FieldMask{Paths: o.UpdateMask},
	}, c.Cluster.UpdateCluster)
}

// ClusterDelete calls ClusterService.DeleteCluster.
func ClusterDelete(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.DeleteClusterRequest{TenantId: o.Tenant, ClusterId: o.ID, Etag: o.ETag}, c.Cluster.DeleteCluster)
}

func clusterInput(o Options) (*cloudv1.Cluster, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	n := &cloudv1.Cluster{}
	if err := LoadInput(o.Input, n); err != nil {
		return nil, err
	}
	if o.ID != "" {
		n.Id = o.ID
	}
	return n, nil
}
