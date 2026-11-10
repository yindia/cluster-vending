package cli

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// NetworkCreate calls NetworkService.CreateNetwork with the Network in the
// input file; the argument, when given, is its name. The server assigns
// the id.
func NetworkCreate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	n := &cloudv1.Network{}
	if err := LoadInput(o.Input, n); err != nil {
		return nil, err
	}
	if n.GetId() != "" {
		return nil, fmt.Errorf("--config: id %q: a network's id is assigned by the server; set name instead", n.GetId())
	}
	if o.ID != "" {
		n.Name = o.ID
	}
	req := &cloudv1.CreateNetworkRequest{TenantId: o.Tenant, Network: n}
	id, err := requestID(o.Tenant, req)
	if err != nil {
		return nil, err
	}
	req.RequestId = id
	return call(ctx, req, c.Network.CreateNetwork)
}

// NetworkGet calls NetworkService.GetNetwork.
func NetworkGet(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.GetNetworkRequest{TenantId: o.Tenant, NetworkId: o.ID}, c.Network.GetNetwork)
}

// NetworkList calls NetworkService.ListNetworks.
func NetworkList(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.ListNetworksRequest{
		TenantId: o.Tenant, PageSize: o.PageSize, PageToken: o.PageToken,
	}, c.Network.ListNetworks)
}

// NetworkUpdate calls NetworkService.UpdateNetwork with the Network in the
// input file and --update-mask.
func NetworkUpdate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	n, err := networkInput(o)
	if err != nil {
		return nil, err
	}
	if o.ETag != "" {
		n.Etag = o.ETag
	}
	return call(ctx, &cloudv1.UpdateNetworkRequest{
		TenantId: o.Tenant, Network: n, UpdateMask: &fieldmaskpb.FieldMask{Paths: o.UpdateMask},
	}, c.Network.UpdateNetwork)
}

// NetworkDelete calls NetworkService.DeleteNetwork.
func NetworkDelete(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.DeleteNetworkRequest{TenantId: o.Tenant, NetworkId: o.ID, Etag: o.ETag}, c.Network.DeleteNetwork)
}

func networkInput(o Options) (*cloudv1.Network, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	n := &cloudv1.Network{}
	if err := LoadInput(o.Input, n); err != nil {
		return nil, err
	}
	n.Id = o.ID // update: the argument is the network id
	return n, nil
}

// NetworkImport calls NetworkService.ImportNetwork with the request in the
// input file (name, bootstrapId, region, aws: {vpcId, subnetIds}); the
// argument, when given, is the network name.
func NetworkImport(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	req := &cloudv1.ImportNetworkRequest{}
	if err := LoadInput(o.Input, req); err != nil {
		return nil, err
	}
	req.TenantId = o.Tenant
	if o.ID != "" {
		req.Name = o.ID
	}
	if req.GetRequestId() == "" {
		id, err := requestID(o.Tenant, req)
		if err != nil {
			return nil, err
		}
		req.RequestId = id
	}
	return call(ctx, req, c.Network.ImportNetwork)
}

// NetworkTakeOver calls NetworkService.TakeOverNetwork.
func NetworkTakeOver(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.TakeOverNetworkRequest{
		TenantId: o.Tenant, NetworkId: o.ID, Etag: o.ETag, AcknowledgeNotImported: o.AcknowledgeNotImported,
	}, c.Network.TakeOverNetwork)
}
