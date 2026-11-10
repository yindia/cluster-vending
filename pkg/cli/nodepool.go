package cli

import (
	"context"
	"errors"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func (o Options) requireCluster() error {
	if err := o.requireTenant(); err != nil {
		return err
	}
	if o.Cluster == "" {
		return errors.New("--cluster is required")
	}
	return nil
}

// NodePoolCreate calls NodePoolService.CreateNodePool with the NodePool in
// the input file; the argument, when given, is its id.
func NodePoolCreate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	p, err := nodePoolInput(o)
	if err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.CreateNodePoolRequest{TenantId: o.Tenant, ClusterId: o.Cluster, NodePool: p}, c.NodePool.CreateNodePool)
}

// NodePoolGet calls NodePoolService.GetNodePool.
func NodePoolGet(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireCluster(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.GetNodePoolRequest{TenantId: o.Tenant, ClusterId: o.Cluster, NodePoolId: o.ID}, c.NodePool.GetNodePool)
}

// NodePoolList calls NodePoolService.ListNodePools.
func NodePoolList(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireCluster(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.ListNodePoolsRequest{
		TenantId: o.Tenant, ClusterId: o.Cluster, PageSize: o.PageSize, PageToken: o.PageToken,
	}, c.NodePool.ListNodePools)
}

// NodePoolUpdate calls NodePoolService.UpdateNodePool with the NodePool in
// the input file and --update-mask.
func NodePoolUpdate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	p, err := nodePoolInput(o)
	if err != nil {
		return nil, err
	}
	if o.ETag != "" {
		p.Etag = o.ETag
	}
	return call(ctx, &cloudv1.UpdateNodePoolRequest{
		TenantId: o.Tenant, ClusterId: o.Cluster, NodePool: p, UpdateMask: &fieldmaskpb.FieldMask{Paths: o.UpdateMask},
	}, c.NodePool.UpdateNodePool)
}

// NodePoolDelete calls NodePoolService.DeleteNodePool.
func NodePoolDelete(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireCluster(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.DeleteNodePoolRequest{
		TenantId: o.Tenant, ClusterId: o.Cluster, NodePoolId: o.ID, Etag: o.ETag,
	}, c.NodePool.DeleteNodePool)
}

func nodePoolInput(o Options) (*cloudv1.NodePool, error) {
	if err := o.requireCluster(); err != nil {
		return nil, err
	}
	p := &cloudv1.NodePool{}
	if err := LoadInput(o.Input, p); err != nil {
		return nil, err
	}
	if o.ID != "" {
		p.Id = o.ID
	}
	return p, nil
}
