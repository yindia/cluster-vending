package network

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	corenetwork "github.com/yindia/cluster-vending/pkg/network"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Bootstrapped refuses an account that cannot hold managed resources yet:
// nothing could ever be applied for a network in it.
func Bootstrapped(a *model.CloudAccount) error {
	if a.Namespace == "" || a.ProviderConfigName == "" {
		return fault.Newf(fault.FailedPrecondition, "account %s is not fully bootstrapped; run BootstrapTenant again", a.ID)
	}
	return nil
}

// NewRow checks a create request (the contract's rules, the labels, the
// account, the layout) and returns the row CreateNetwork stores for it,
// not stored yet: the network as sent (input) and resolved (spec with the
// layout's defaults filled in), state PROVISIONING, its generation not
// applied yet (Reconcile applies it). The stored row's ID becomes
// Network.id. The API handler and the controller (for a Network object
// that was created without the API) both build their row here. It reads
// the store only.
func (s *Reconciler) NewRow(ctx context.Context, req *cloudv1.CreateNetworkRequest) (*model.Network, error) {
	if err := reconcile.Validate(req); err != nil {
		return nil, err
	}
	if err := reconcile.CheckAWSLabels(req.GetNetwork().GetLabels()); err != nil {
		return nil, err
	}
	a, l, err := s.Plan(ctx, req.GetTenantId(), req.GetNetwork().GetSpec())
	if err != nil {
		return nil, err
	}
	if err := Bootstrapped(a); err != nil {
		return nil, err
	}
	in := req.GetNetwork()
	input, err := Encode(in)
	if err != nil {
		return nil, fault.New(fault.Internal, err.Error())
	}
	spec := proto.CloneOf(in.GetSpec())
	spec.Layout = corenetwork.LayoutProto(l)
	spec.FlowLogs = corenetwork.ResolveFlowLogs(spec.GetFlowLogs())
	raw, err := Encode(spec)
	if err != nil {
		return nil, fault.New(fault.Internal, err.Error())
	}
	row := &model.Network{
		Resource: model.Resource{
			DisplayName: in.GetDisplayName(), Labels: in.GetLabels(), State: StateProvisioning, Spec: raw,
		},
		TenantID: req.GetTenantId(), Name: in.GetName(), Input: input, CloudAccountID: a.ID,
		Provider: model.ProviderAWS, Region: spec.GetRegion(),
	}
	if id := req.GetRequestId(); id != "" {
		row.CreateRequestID = &id
	}
	return row, nil
}
