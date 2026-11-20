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

	awscatalog "github.com/yindia/cluster-vending/pkg/constant/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	corenetwork "github.com/yindia/cluster-vending/pkg/network"
	accountreconcile "github.com/yindia/cluster-vending/pkg/reconcile/account"
	netreconcile "github.com/yindia/cluster-vending/pkg/reconcile/network"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// ImportNetwork stores the import of an existing VPC: the request, and a
// network that is IMPORTED with its import DISCOVERING. It does not look
// at the cloud. The reconciler looks the VPC up once, read-only, stores
// the spec derived from it and the plan (status.import: PLANNED, or
// BLOCKED and then the network is FAILED and nothing is imported), and
// creates the Observe-only managed resources. A VPC that does not exist
// or cannot be read is the network's status (FAILED, with the reason),
// not the answer of this call. Nothing changes in the cloud.
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
	if err := s.usable(ctx, account, req.GetRegion(), accountreconcile.Import); err != nil {
		return nil, err
	}
	if err := s.refuseOwnedVPC(ctx, account, req.GetAws().GetVpcId()); err != nil {
		return nil, service.ToConnect(err)
	}
	row, err := s.insertImport(ctx, req, account)
	if err != nil {
		return nil, service.ToConnect(err)
	}
	s.logger().InfoContext(ctx, "network import stored", "tenant_id", row.TenantID, "network_id", row.ID,
		"vpc_id", req.GetAws().GetVpcId())
	n, err := s.stored(ctx, row)
	return &cloudv1.ImportNetworkResponse{Network: n}, service.ToConnect(err)
}

// insertImport stores the import as a network that is IMPORTED and
// DISCOVERING (netreconcile.NewImportRow): the request as sent, nothing of
// the VPC yet. The ID is chosen first: it names the resources.
func (s *NetworkService) insertImport(ctx context.Context, req *cloudv1.ImportNetworkRequest, a *model.CloudAccount) (*model.Network, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
	}
	row, err := netreconcile.NewImportRow(req, a, id.String())
	if err != nil {
		return nil, err
	}
	err = s.deps.Store.Networks().Create(ctx, row)
	if errors.Is(err, repository.ErrAlreadyExists) {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Sprintf(
			"VPC %s is already imported (or request_id %q was used before)", req.GetAws().GetVpcId(), req.GetRequestId()))
	}
	if err != nil {
		return nil, service.ToConnect(err)
	}
	return row, nil
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
	if err := s.usableByID(ctx, row.TenantID, row.CloudAccountID, row.Region, accountreconcile.TakeOver); err != nil {
		return nil, err
	}
	// The plan that was reviewed is the one applied: what was changed on
	// the object behind the API's back, and no pass has put back yet, is
	// dropped here.
	if _, err := netreconcile.RestoreDiscovered(row); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error())
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
			fmt.Sprintf("network %s is still being imported; retry once its resources are observed (network get --live)", row.ID))
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
