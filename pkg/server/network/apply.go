package network

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// resolved is a stored network ready to render.
type resolved struct {
	crossplaneaws.NetworkPlan
}

func newResolved(row *model.Network, spec *cloudv1.NetworkSpec, a *model.CloudAccount, l awsLayout) resolved {
	return resolved{
		NetworkPlan: crossplaneaws.NetworkPlan{
			Namespace:          a.Namespace,
			ProviderConfigName: a.ProviderConfigName,
			TenantID:           row.TenantID,
			NetworkID:          row.ID,
			NetworkName:        row.Name,
			Prefix:             crossplaneaws.NetworkPrefix(row.TenantID, row.ID, row.Name),
			Region:             spec.GetRegion(),
			CIDRBlock:          l.CIDRBlock,
			Subnets:            l.Subnets,
			NAT:                l.NAT,
			Tags:               row.Labels,
			AccountID:          a.AccountID,
			FlowLogs:           flowLogPlan(spec.GetFlowLogs()),
		},
	}
}

// layoutProto is the resolved layout as stored in the spec: explicit
// subnets, so later renders do not depend on defaults.
func layoutProto(r awsLayout) *cloudv1.NetworkLayout {
	l := &cloudv1.NetworkLayout{CidrBlock: r.CIDRBlock, NatGatewayMode: cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_SINGLE}
	if r.NAT == crossplaneaws.NATPerZone {
		l.NatGatewayMode = cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_PER_ZONE
	}
	for _, s := range r.Subnets {
		role := cloudv1.SubnetRole_SUBNET_ROLE_PRIVATE
		if s.Role == crossplaneaws.SubnetPublic {
			role = cloudv1.SubnetRole_SUBNET_ROLE_PUBLIC
		}
		l.Subnets = append(l.Subnets, &cloudv1.Subnet{Role: role, CidrBlock: s.CIDR, ZoneId: s.ZoneID})
	}
	return l
}

// apply creates or updates the network's managed resources, then records
// them (model.ManagedResource) as the network's.
func (s *NetworkService) apply(ctx context.Context, row *model.Network, a *model.CloudAccount, plan resolved) error {
	objects, err := crossplaneaws.NetworkResources(plan.NetworkPlan)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, "spec.layout: "+err.Error())
	}
	return s.applyObjects(ctx, row, a, objects)
}

// applyObjects applies objects in order and records them as the network's.
func (s *NetworkService) applyObjects(ctx context.Context, row *model.Network, a *model.CloudAccount, objects []client.Object) error {
	if a.Namespace == "" || a.ProviderConfigName == "" {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("account %s is not fully bootstrapped; run BootstrapTenant again", a.ID))
	}
	for _, obj := range objects {
		err := kube.Apply(ctx, s.deps.Kube, obj)
		switch {
		case err == nil:
		case meta.IsNoMatchError(err):
			return connect.NewError(connect.CodeFailedPrecondition,
				"a Crossplane AWS provider the network needs (ec2, iam, cloudwatchlogs) is not installed in the management cluster")
		default:
			s.logger().ErrorContext(ctx, "apply network resource", "tenant_id", row.TenantID,
				"network_id", row.ID, "object", obj.GetName(), "error", err)
			return connect.NewError(connect.CodeInternal, "could not create the network's resources")
		}
	}
	owner := service.Owner{TenantID: row.TenantID, CloudAccountID: a.ID, Kind: model.OwnerNetwork, ID: row.ID}
	if err := s.deps.Record(ctx, owner, objects); err != nil {
		s.logger().ErrorContext(ctx, "record network resources", "network_id", row.ID, "error", err)
		return connect.NewError(connect.CodeInternal, "could not record the network's resources")
	}
	return s.markApplied(ctx, row)
}

// markApplied records that every object of the row's generation was
// applied without error, with this build's renderer.
func (s *NetworkService) markApplied(ctx context.Context, row *model.Network) error {
	if row.AppliedGeneration == row.Generation && row.RenderVersion == crossplaneaws.RenderVersion {
		return nil
	}
	row.AppliedGeneration, row.RenderVersion = row.Generation, crossplaneaws.RenderVersion
	if err := s.deps.Store.Networks().Update(ctx, row); err != nil {
		return connect.NewError(connect.CodeInternal, "could not record the applied generation: "+err.Error())
	}
	return nil
}

// liveObjects counts the network's objects the API server still has (by
// label, uncached).
func (s *NetworkService) liveObjects(ctx context.Context, row *model.Network) (int, error) {
	n := 0
	for _, list := range crossplaneaws.NetworkLists() {
		err := s.deps.Reader().List(ctx, list, client.InNamespace(kube.TenantNamespace(row.TenantID)),
			client.MatchingLabels{crossplane.LabelNetwork: row.ID})
		if meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("list %T: %w", list, err)
		}
		n += meta.LenList(list)
	}
	return n, nil
}

// deleteResources deletes every managed resource of the network. Crossplane
// deletes the cloud resources and removes the objects when done.
func (s *NetworkService) deleteResources(ctx context.Context, tenantID, networkID string) error {
	for _, obj := range crossplaneaws.NetworkObjects() {
		err := s.deps.Kube.DeleteAllOf(ctx, obj,
			client.InNamespace(kube.TenantNamespace(tenantID)),
			client.MatchingLabels{crossplane.LabelNetwork: networkID})
		if err != nil && !meta.IsNoMatchError(err) {
			return fmt.Errorf("delete %T: %w", obj, err)
		}
	}
	return nil
}

// rollback undoes a create whose resources could not be applied.
func (s *NetworkService) rollback(ctx context.Context, row *model.Network) {
	if err := s.deleteResources(ctx, row.TenantID, row.ID); err != nil {
		s.logger().ErrorContext(ctx, "rollback network resources", "network_id", row.ID, "error", err)
	}
	if err := s.deps.Store.Networks().Delete(ctx, row.TenantID, row.ID); err != nil {
		s.logger().ErrorContext(ctx, "rollback network row", "network_id", row.ID, "error", err)
	}
	if err := s.deps.Store.ManagedResources().DeleteByOwner(ctx, model.OwnerNetwork, row.ID); err != nil {
		s.logger().ErrorContext(ctx, "rollback network records", "network_id", row.ID, "error", err)
	}
}

// deleteOrder deletes a network from the inside out, the order AWS
// accepts: the flow log, routes and subnet associations; then the flow
// log's log group and its role's policy attachment, route tables and NAT
// gateways; then the delivery role and its policy and the NAT gateways'
// EIPs; then subnets and the internet gateway (it detaches only once no
// public IP is left); then the VPC.
//
// The flow log's Usages (they hold its log group and role while it
// exists) sit in the tier after it: Kubernetes garbage-collects them with
// the flow log, and DeleteRecorded never deletes a Usage itself.
var deleteOrder = service.DeleteOrder{
	{"FlowLog", "RouteTableAssociation", "Route"},
	{"Usage", "Group", "RolePolicyAttachment", "RouteTable", "NATGateway"},
	{"Role", "Policy", "EIP"},
	{"Subnet", "InternetGateway"},
	{"VPC"},
}

// finalize moves a DELETING network's deletion on (deleteOrder: the next
// tier once the previous one is gone) and removes its row once all its
// resources are gone. DeleteNetwork and the background loop call it.
func (s *NetworkService) finalize(ctx context.Context, row *model.Network) error {
	if row.State != stateDeleting {
		return nil
	}
	o, err := s.observe(ctx, row.TenantID, row.ID)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, "cannot read the network's resources from the management cluster")
	}
	// records also records live objects that were never recorded (by
	// label), so every resource of the network is deleted in order.
	records, err := s.records(ctx, row, o.objects)
	if err != nil {
		return err
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return err
	}
	if err := s.deps.MarkGone(ctx, records, views); err != nil {
		return connect.NewError(connect.CodeInternal, err.Error())
	}
	if err := s.deps.ReleaseOrphanUsages(ctx, records, views); err != nil {
		return connect.NewError(connect.CodeInternal, err.Error())
	}
	left, err := s.deps.DeleteNext(ctx, records, views, deleteOrder)
	if err != nil {
		s.logger().ErrorContext(ctx, "delete network resources", "network_id", row.ID, "error", err)
		return connect.NewError(connect.CodeInternal, "could not delete the network's resources")
	}
	if left > 0 || o.total > 0 {
		return nil
	}
	// The cache says everything is gone: confirm with the API server before
	// forgetting the network (a cache that lost its objects must not make
	// the platform forget cloud resources).
	if live, err := s.liveObjects(ctx, row); err != nil || live > 0 {
		if err != nil {
			return connect.NewError(connect.CodeUnavailable, "cannot confirm the network's resources are gone: "+err.Error())
		}
		return nil
	}
	// Records first: a failure after it is retried on the next pass (the
	// row is still there), never leaving records without a network.
	if err := s.deps.Store.ManagedResources().DeleteByOwner(ctx, model.OwnerNetwork, row.ID); err != nil {
		return connect.NewError(connect.CodeInternal, "could not forget the deleted network's resources: "+err.Error())
	}
	if err := s.deps.Store.Networks().Delete(ctx, row.TenantID, row.ID); err != nil && !errors.Is(err, repository.ErrNotFound) {
		return connect.NewError(connect.CodeInternal, "could not remove the deleted network: "+err.Error())
	}
	return nil
}
