package network

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	"github.com/google/uuid"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	awscatalog "github.com/yindia/cluster-vending/pkg/constant/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	corenetwork "github.com/yindia/cluster-vending/pkg/network"
	netreconcile "github.com/yindia/cluster-vending/pkg/reconcile/network"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// ImportNetwork looks an existing VPC up once, read-only, and stores the
// import: the spec derived from the VPC, the plan (status.import) and what
// was discovered. The reconciler creates the Observe-only managed
// resources from that, without looking the cloud up again. Nothing changes
// in the cloud. The network is IMPORTED, or FAILED when the plan is
// BLOCKED (then nothing is imported).
func (s *NetworkService) ImportNetwork(ctx context.Context, req *cloudv1.ImportNetworkRequest) (*cloudv1.ImportNetworkResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, service.ToConnect(err)
	}
	if req.GetAws() == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, "only aws networks can be imported")
	}
	if !awscatalog.IsRegion(req.GetRegion()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("region %q is not a known AWS region", req.GetRegion()))
	}
	if id := req.GetRequestId(); id != "" {
		if row, err := s.deps.Store.Networks().GetByCreateRequest(ctx, req.GetTenantId(), id); err == nil {
			n, err := s.stored(ctx, row)
			return &cloudv1.ImportNetworkResponse{Network: n}, service.ToConnect(err)
		} else if !errors.Is(err, repository.ErrNotFound) {
			return nil, service.ToConnect(err)
		}
	}
	account, err := s.rec().Account(ctx, req.GetTenantId(), req.GetBootstrapId())
	if err != nil {
		return nil, service.ToConnect(err)
	}
	if err := bootstrapped(account); err != nil {
		return nil, err
	}
	if err := s.refuseOwnedVPC(ctx, account, req.GetAws().GetVpcId()); err != nil {
		return nil, service.ToConnect(err)
	}
	found, err := s.discover(ctx, account, req)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	row, plan, err := s.insertImport(ctx, req, account, found)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	s.logger().InfoContext(ctx, "network import stored", "tenant_id", row.TenantID, "network_id", row.ID,
		"vpc_id", req.GetAws().GetVpcId(), "blocked", plan.Blocked(), "plan_lines", len(plan.Lines))
	n, err := s.stored(ctx, row)
	return &cloudv1.ImportNetworkResponse{Network: n}, service.ToConnect(err)
}

// discover makes the read-only AWS lookup.
func (s *NetworkService) discover(ctx context.Context, a *model.CloudAccount, req *cloudv1.ImportNetworkRequest) (*awscloud.Network, error) {
	if s.deps.AWSDiscovery == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "AWS lookups are not configured on this server")
	}
	access, err := s.deps.AWSAccess(ctx, a)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	found, err := s.deps.AWSDiscovery.DiscoverNetwork(ctx, access, req.GetRegion(),
		req.GetAws().GetVpcId(), req.GetAws().GetSubnetIds())
	switch {
	case err == nil:
		return found, nil
	case errors.Is(err, awscloud.ErrNotFound):
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	case errors.Is(err, awscloud.ErrInvalid):
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	case errors.Is(err, awscloud.ErrAccessDenied):
		return nil, connect.NewError(connect.CodeFailedPrecondition, "cannot read the VPC with the account's credentials: "+err.Error())
	default:
		s.logger().ErrorContext(ctx, "discover network", "vpc_id", req.GetAws().GetVpcId(), "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, "the AWS lookup failed: "+err.Error())
	}
}

// insertImport plans the import and stores the network: the derived spec,
// the request as sent with what was discovered (input: the reconciler
// renders the network's objects from it), and the plan (status), IMPORTED
// or, when blocked, FAILED. The ID is chosen first: it names the
// resources.
func (s *NetworkService) insertImport(ctx context.Context, req *cloudv1.ImportNetworkRequest, a *model.CloudAccount, found *awscloud.Network) (*model.Network, corenetwork.ImportPlan, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, corenetwork.ImportPlan{}, connect.NewError(connect.CodeInternal, err.Error())
	}
	plan := corenetwork.PlanImport(found, crossplaneaws.ImportedNetwork{
		Namespace: a.Namespace, ProviderConfigName: a.ProviderConfigName, TenantID: req.GetTenantId(),
		NetworkID: id.String(), NetworkName: req.GetName(), Region: req.GetRegion(),
		Prefix: crossplaneaws.NetworkPrefix(req.GetTenantId(), id.String(), req.GetName()),
	})
	phase, state := cloudv1.ImportPhase_IMPORT_PHASE_PLANNED, netreconcile.StateImported
	if plan.Blocked() {
		phase, state = cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED, netreconcile.StateBlocked
	}
	spec := &cloudv1.NetworkSpec{Region: req.GetRegion(), BootstrapId: a.ID, Layout: plan.Layout}
	rawSpec, err1 := netreconcile.Encode(spec)
	input, err2 := netreconcile.Encode(req)
	stored, err3 := netreconcile.Encode(&cloudv1.NetworkStatus{Import: &cloudv1.ImportStatus{Phase: phase, Resources: plan.Lines}})
	if err2 == nil && !plan.Blocked() {
		input, err2 = netreconcile.WithImportedResources(input, plan.Resources)
	}
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, corenetwork.ImportPlan{}, connect.NewError(connect.CodeInternal, err.Error())
	}
	row := &model.Network{
		Resource: model.Resource{Base: model.Base{ID: id.String()}, DisplayName: req.GetDisplayName(),
			State: state, Spec: rawSpec, Status: stored},
		TenantID: req.GetTenantId(), Name: req.GetName(), Input: input, CloudAccountID: a.ID,
		Provider: model.ProviderAWS, Region: req.GetRegion(),
	}
	if rid := req.GetRequestId(); rid != "" {
		row.CreateRequestID = &rid
	}
	if vpc := req.GetAws().GetVpcId(); !plan.Blocked() {
		// Unique per account and region: of two imports of one VPC at the
		// same moment (refuseOwnedVPC lets both pass), one is stored.
		row.ExternalID = &vpc
	}
	err = s.deps.Store.Networks().Create(ctx, row)
	if errors.Is(err, repository.ErrAlreadyExists) {
		return nil, corenetwork.ImportPlan{}, connect.NewError(connect.CodeAlreadyExists, fmt.Sprintf(
			"VPC %s is already imported (or request_id %q was used before)", req.GetAws().GetVpcId(), req.GetRequestId()))
	}
	if err != nil {
		return nil, corenetwork.ImportPlan{}, service.ToConnect(err)
	}
	return row, plan, nil
}

// TakeOverNetwork stores an IMPORTED network as taken over. The reconciler
// then makes the platform its owner: every managed resource switches from
// Observe to full management, and subnets get their load balancer tag (the
// plan's WILL_UPDATE lines). The network then provisions like a created
// one.
func (s *NetworkService) TakeOverNetwork(ctx context.Context, req *cloudv1.TakeOverNetworkRequest) (*cloudv1.TakeOverNetworkResponse, error) {
	if err := service.Validate(req); err != nil {
		return nil, service.ToConnect(err)
	}
	row, err := s.get(ctx, req.GetTenantId(), req.GetNetworkId())
	if err != nil {
		return nil, service.ToConnect(err)
	}
	imp, err := takeOverAllowed(row, req)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	// The row only: a network whose objects are owned (deleting them
	// deletes the cloud resources) must never be stored as read-only, so
	// the reconciler owns them once the row says so (moveOn).
	imp.Phase = cloudv1.ImportPhase_IMPORT_PHASE_CONVERGING
	if row.Status, err = netreconcile.Encode(&cloudv1.NetworkStatus{Import: imp}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	row.State = netreconcile.StateProvisioning
	row.Generation++
	if err := s.deps.Store.Networks().Update(ctx, row); err != nil {
		return nil, service.ToConnect(err)
	}
	s.logger().InfoContext(ctx, "network takeover stored", "tenant_id", row.TenantID, "network_id", row.ID)
	n, err := s.stored(ctx, row)
	return &cloudv1.TakeOverNetworkResponse{Network: n}, service.ToConnect(err)
}

// takeOverAllowed checks state, etag, phase and the acknowledgement of
// NOT_IMPORTED resources, and returns the stored plan.
func takeOverAllowed(row *model.Network, req *cloudv1.TakeOverNetworkRequest) (*cloudv1.ImportStatus, error) {
	if row.State != netreconcile.StateImported {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("network %s is not IMPORTED (only imported networks are taken over)", row.ID))
	}
	if etag := req.GetEtag(); etag != "" && etag != row.ETag {
		return nil, connect.NewError(connect.CodeAborted, "etag mismatch: the network changed; get it again and retry")
	}
	if row.AppliedGeneration < row.Generation {
		// Its Observe-only objects are not there yet: there is nothing to own.
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Sprintf("network %s is still being imported; retry once its resources are observed (network status)", row.ID))
	}
	imp := netreconcile.StoredImport(row)
	if imp.GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_PLANNED {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf("import phase is %s, want PLANNED", imp.GetPhase()))
	}
	notImported := corenetwork.ImportPlan{Lines: imp.GetResources()}.NotImported()
	if len(notImported) > 0 && !req.GetAcknowledgeNotImported() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Sprintf(
			"%d resources are NOT_IMPORTED (first: %s %s: %s); give them another owner, then retry with acknowledge_not_imported",
			len(notImported), notImported[0].GetKind(), notImported[0].GetExternalName(), notImported[0].GetDetail()))
	}
	return imp, nil
}

// refuseOwnedVPC fails with ALREADY_EXISTS when a network of the tenant
// (created or imported) already has the VPC: two networks would manage the
// same cloud resources.
func (s *NetworkService) refuseOwnedVPC(ctx context.Context, a *model.CloudAccount, vpcID string) error {
	vpcs := &awsec2.VPCList{}
	// From the API server, not the cache: an import a moment ago must be
	// seen. Of two imports in the very same instant, the unique index on
	// networks.external_id stores one (insertImport).
	if err := s.deps.Reader().List(ctx, vpcs, client.InNamespace(a.Namespace)); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return connect.NewError(connect.CodeUnavailable, "cannot list the tenant's VPCs: "+err.Error())
	}
	for _, v := range vpcs.Items {
		if v.Annotations[crossplaneaws.ExternalNameAnnotation] == vpcID {
			return connect.NewError(connect.CodeAlreadyExists, fmt.Sprintf(
				"VPC %s already belongs to network %s; delete that network first, or use it", vpcID, v.Labels[crossplane.LabelNetwork]))
		}
	}
	return nil
}
