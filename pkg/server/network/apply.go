package network

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

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
	hold, err := s.holdNAT(ctx, row)
	if err != nil {
		return err
	}
	if hold {
		// Created in sequence: the NAT gateways follow once the internet
		// gateway is attached (Reconcile applies the network again).
		if objects, err = s.withoutKind(objects, natGatewayKind); err != nil {
			return connect.NewError(connect.CodeInternal, err.Error())
		}
	}
	if err := s.applyObjects(ctx, row, a, objects); err != nil || hold {
		return err // held back: not applied in full yet
	}
	// What the spec no longer has (flow logs turned off) is deleted. Only
	// after a full render: an object held back is not a dropped one.
	owner := service.Owner{TenantID: row.TenantID, CloudAccountID: a.ID, Kind: model.OwnerNetwork, ID: row.ID}
	if err := s.deps.Prune(ctx, owner, objects); err != nil {
		s.logger().ErrorContext(ctx, "prune network resources", "network_id", row.ID, "error", err)
		return connect.NewError(connect.CodeInternal, "could not remove the network's dropped resources")
	}
	return s.markApplied(ctx, row)
}

// Kinds of the NAT gateway stage.
const (
	natGatewayKind      = "NATGateway"
	internetGatewayKind = "InternetGateway"
)

// holdNAT reports whether apply must leave out the NAT gateways: a public
// NAT gateway can only be created in a VPC whose internet gateway is
// attached, and created earlier it fails and turns the network FAILED.
// They are held back until the InternetGateway is Ready (the provider
// creates and attaches it in one step), and never again once they were
// created.
func (s *NetworkService) holdNAT(ctx context.Context, row *model.Network) (bool, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, row.ID)
	if err != nil {
		return false, service.ToConnect(err)
	}
	var gateways []model.ManagedResource
	for _, r := range records {
		switch r.Kind {
		case natGatewayKind:
			return false, nil
		case internetGatewayKind:
			gateways = append(gateways, r)
		}
	}
	views, err := s.deps.Observe(ctx, gateways)
	if err != nil {
		return false, err
	}
	for _, v := range views {
		if v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY {
			return false, nil
		}
	}
	return true, nil
}

// withoutKind drops the objects of one kind.
func (s *NetworkService) withoutKind(objects []client.Object, kind string) ([]client.Object, error) {
	out := make([]client.Object, 0, len(objects))
	for _, obj := range objects {
		gvk, err := apiutil.GVKForObject(obj, s.deps.Kube.Scheme())
		if err != nil {
			return nil, err
		}
		if gvk.Kind != kind {
			out = append(out, obj)
		}
	}
	return out, nil
}

// applyObjects applies objects in order and records them as the network's.
// The caller marks the row applied once nothing is held back.
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
	return nil
}

// markApplied records that every object of the row's generation was
// applied without error, with this build's renderer.
func (s *NetworkService) markApplied(ctx context.Context, row *model.Network) error {
	if row.AppliedGeneration == row.Generation && row.RenderVersion == crossplaneaws.RenderVersion {
		return nil
	}
	before, version := row.AppliedGeneration, row.RenderVersion
	row.AppliedGeneration, row.RenderVersion = row.Generation, crossplaneaws.RenderVersion
	err := s.deps.Store.Networks().Update(ctx, row)
	if errors.Is(err, repository.ErrConflict) {
		// The row changed while it was applied: whoever changed it applies
		// it, or the loop does (recheck).
		row.AppliedGeneration, row.RenderVersion = before, version
		return nil
	}
	if err != nil {
		return connect.NewError(connect.CodeInternal, "could not record the applied generation: "+err.Error())
	}
	return nil
}

// recheck is called by the loop after it applied row: when a call changed
// the row meanwhile, what the loop applied may be older than what that
// call applied, and may have been written over it. The row is marked as
// not applied, so the next pass applies it as it is now.
func (s *NetworkService) recheck(ctx context.Context, row *model.Network) {
	current, err := s.deps.Store.Networks().Get(ctx, row.TenantID, row.ID)
	if errors.Is(err, repository.ErrNotFound) || (err == nil && (current.ETag == row.ETag || current.State == stateDeleting)) {
		return
	}
	if err == nil {
		err = s.deps.Store.Networks().MarkBehind(ctx, row.TenantID, row.ID)
	}
	if err != nil {
		s.logger().ErrorContext(ctx, "recheck a network applied while it changed", "network_id", row.ID, "error", err)
		return
	}
	s.logger().InfoContext(ctx, "network changed while it was applied; it is applied again", "network_id", row.ID)
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
	var errs []error
	for _, obj := range crossplaneaws.NetworkObjects() {
		err := s.deps.Kube.DeleteAllOf(ctx, obj,
			client.InNamespace(kube.TenantNamespace(tenantID)),
			client.MatchingLabels{crossplane.LabelNetwork: networkID})
		// A 409 is the Usage webhook holding an object that is still in use:
		// Crossplane deletes it once its user is gone. Every kind is tried,
		// whatever one of them answers: stopping would leave the rest behind.
		if err != nil && !meta.IsNoMatchError(err) && !apierrors.IsConflict(err) {
			errs = append(errs, fmt.Errorf("delete %T: %w", obj, err))
		}
	}
	return errors.Join(errs...)
}

// rollback undoes a create whose resources could not be applied: the
// recorded objects one by one (a refused one does not stop the others),
// then whatever else carries the network's label.
func (s *NetworkService) rollback(ctx context.Context, row *model.Network) {
	if records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, row.ID); err == nil {
		if err := s.deps.DeleteRecorded(ctx, records); err != nil {
			s.logger().ErrorContext(ctx, "rollback recorded network resources", "network_id", row.ID, "error", err)
		}
	}
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
// accepts: the flow log, network ACL rules, routes and subnet
// associations; then the flow log's log group and its role's policy
// attachment, network ACLs, route tables and NAT gateways; then the
// delivery role and its policy and the NAT gateways' EIPs; then subnets,
// the internet gateway (it detaches only once no public IP is left) and
// the default network ACL (only its object goes: it is owned without
// Delete); then the VPC.
//
// The flow log's Usages (they hold its log group and role while it
// exists) sit in the tier after it: Kubernetes garbage-collects them with
// the flow log, and DeleteRecorded never deletes a Usage itself.
var deleteOrder = service.DeleteOrder{
	{"FlowLog", "NetworkACLRule", "RouteTableAssociation", "Route"},
	{"Usage", "Group", "RolePolicyAttachment", "NetworkACL", "RouteTable", "NATGateway"},
	{"Role", "Policy", "EIP"},
	{"Subnet", "InternetGateway", "DefaultNetworkACL"},
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
