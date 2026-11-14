package cli

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// NetworkCreate calls NetworkService.CreateNetwork with the Network
// manifest; the server assigns the id. With --wait it returns once the
// network is READY.
func NetworkCreate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	m, err := LoadManifest(o.Input, KindNetwork)
	if err != nil {
		return nil, err
	}
	if m.Metadata.ID != "" {
		return nil, fmt.Errorf("metadata.id %q: a network's id is assigned by the server", m.Metadata.ID)
	}
	tenant, err := m.tenant(o.Tenant)
	if err != nil {
		return nil, err
	}
	n, err := m.Network()
	if err != nil {
		return nil, err
	}
	req := &cloudv1.CreateNetworkRequest{TenantId: tenant, Network: n}
	if req.RequestId, err = requestID(tenant, req); err != nil {
		return nil, err
	}
	res, err := call(ctx, req, c.Network.CreateNetwork)
	if err != nil || !o.Wait {
		return res, err
	}
	return waitNetwork(ctx, c, o, tenant, res.(*cloudv1.CreateNetworkResponse).GetNetwork().GetId(), false)
}

// NetworkGet calls NetworkService.GetNetwork.
func NetworkGet(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	return call(ctx, &cloudv1.GetNetworkRequest{TenantId: o.Tenant, NetworkId: o.ID, Live: o.Live}, c.Network.GetNetwork)
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

// NetworkUpdate calls NetworkService.UpdateNetwork with the Network
// manifest and --update-mask; the network is the argument or metadata.id.
func NetworkUpdate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	m, err := LoadManifest(o.Input, KindNetwork)
	if err != nil {
		return nil, err
	}
	tenant, err := m.tenant(o.Tenant)
	if err != nil {
		return nil, err
	}
	if m.Metadata.ID, err = updateID(o.ID, m.Metadata.ID); err != nil {
		return nil, err
	}
	n, err := m.Network()
	if err != nil {
		return nil, err
	}
	if o.ETag != "" {
		n.Etag = o.ETag
	}
	return call(ctx, &cloudv1.UpdateNetworkRequest{
		TenantId: tenant, Network: n, UpdateMask: &fieldmaskpb.FieldMask{Paths: o.UpdateMask},
	}, c.Network.UpdateNetwork)
}

// NetworkDelete calls NetworkService.DeleteNetwork, which stores the
// deletion (the answer is the network, DELETING). The server then deletes
// the network's resources in order; --wait reads until the network is
// gone.
func NetworkDelete(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.DeleteNetworkRequest{TenantId: o.Tenant, NetworkId: o.ID, Etag: o.ETag}, c.Network.DeleteNetwork)
	if err != nil || !o.Wait {
		return res, err
	}
	if _, err := waitNetwork(ctx, c, o, o.Tenant, o.ID, true); err != nil {
		return nil, err
	}
	return res, nil
}

// NetworkImport calls NetworkService.ImportNetwork with the NetworkImport
// manifest. The call only stores the import: the answer is the network as
// IMPORTED with its import DISCOVERING, before the VPC was looked at. With
// --wait it returns once the controller has looked the VPC up and the
// plan (status.import) is there: PLANNED with the resources observed, or
// BLOCKED. A VPC that does not exist or cannot be read ends the wait as
// an error (the network is FAILED, with the reason).
func NetworkImport(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	m, err := LoadManifest(o.Input, KindNetworkImport)
	if err != nil {
		return nil, err
	}
	tenant, err := m.tenant(o.Tenant)
	if err != nil {
		return nil, err
	}
	req, err := m.NetworkImport()
	if err != nil {
		return nil, err
	}
	req.TenantId = tenant
	if req.GetRequestId() == "" {
		if req.RequestId, err = requestID(tenant, req); err != nil {
			return nil, err
		}
	}
	res, err := call(ctx, req, c.Network.ImportNetwork)
	if err != nil || !o.Wait {
		return res, err
	}
	id := res.(*cloudv1.ImportNetworkResponse).GetNetwork().GetId()
	var last proto.Message
	err = waitUntil(ctx, o, "network "+id, false, func(ctx context.Context) (observation, error) {
		res, err := call(ctx, &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: id, Live: true}, c.Network.GetNetwork)
		if err != nil {
			return observation{}, err
		}
		last = res
		st := res.(*cloudv1.GetNetworkResponse).GetNetwork().GetStatus()
		return importObservation(st.GetState().String(), st.GetMessage(), st.GetImport(), len(st.GetManagedResources()),
			st.GetState() == cloudv1.NetworkState_NETWORK_STATE_FAILED), nil
	})
	return last, err
}

// NetworkTakeOver calls NetworkService.TakeOverNetwork. With --wait it
// returns once the network is READY under the platform.
func NetworkTakeOver(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.TakeOverNetworkRequest{
		TenantId: o.Tenant, NetworkId: o.ID, Etag: o.ETag, AcknowledgeNotImported: o.AcknowledgeNotImported,
	}, c.Network.TakeOverNetwork)
	if err != nil || !o.Wait {
		return res, err
	}
	return waitNetwork(ctx, c, o, o.Tenant, o.ID, false)
}

// waitNetwork waits until the network is READY, or IMPORTED with its
// resources observed (gone: deleted), and returns its last read. A call
// only stores what was asked for; what goes wrong applying it is the
// network's status: FAILED ends the wait with its message as the error.
func waitNetwork(ctx context.Context, c Clients, o Options, tenant, id string, gone bool) (proto.Message, error) {
	var last proto.Message
	err := waitUntil(ctx, o, "network "+id, gone, func(ctx context.Context) (observation, error) {
		// The live status: Get returns what the server last recorded.
		res, err := call(ctx, &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: id, Live: true}, c.Network.GetNetwork)
		if err != nil {
			return observation{}, err
		}
		last = res
		st := res.(*cloudv1.GetNetworkResponse).GetNetwork().GetStatus()
		state := st.GetState()
		return observation{
			state: state.String(), message: st.GetMessage(),
			// An import is stored IMPORTED at once; it is done when the server
			// created its (Observe-only) resources.
			done: !gone && (state == cloudv1.NetworkState_NETWORK_STATE_READY ||
				(state == cloudv1.NetworkState_NETWORK_STATE_IMPORTED && len(st.GetManagedResources()) > 0)),
			failed: state == cloudv1.NetworkState_NETWORK_STATE_FAILED && !gone,
		}, nil
	})
	return last, err
}

// updateID is the id an update targets: the argument or metadata.id, which
// must agree when both are set.
func updateID(arg, meta string) (string, error) {
	switch {
	case arg == "" && meta == "":
		return "", fmt.Errorf("the id is required: the argument or metadata.id")
	case arg != "" && meta != "" && arg != meta:
		return "", fmt.Errorf("the argument %q and metadata.id %q differ", arg, meta)
	case arg != "":
		return arg, nil
	default:
		return meta, nil
	}
}
