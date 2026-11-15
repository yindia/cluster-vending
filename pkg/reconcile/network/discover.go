package network

import (
	"context"
	"errors"
	"fmt"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	corenetwork "github.com/yindia/cluster-vending/pkg/network"
)

// NewImportRow is the row ImportNetwork stores for req in account a, not
// stored yet: IMPORTED with its import DISCOVERING, the request as sent
// (input), and a spec that only says where the VPC is. The VPC is not
// looked up here; Reconcile does that once (discoverImport) and fills in
// the rest. The VPC ID is the row's external ID from the start and for as
// long as the row exists: of two imports of one VPC, one is stored, also
// when the first turned out blocked or failed (delete it to try again).
func NewImportRow(req *cloudv1.ImportNetworkRequest, a *model.CloudAccount, id string) (*model.Network, error) {
	rawSpec, err1 := Encode(&cloudv1.NetworkSpec{Region: req.GetRegion(), BootstrapId: a.ID})
	input, err2 := Encode(req)
	status, err3 := Encode(&cloudv1.NetworkStatus{State: cloudv1.NetworkState_NETWORK_STATE_IMPORTED, Message: discoveringMessage,
		Import: &cloudv1.ImportStatus{Phase: cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING}})
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, fault.New(fault.Internal, err.Error())
	}
	vpc := req.GetAws().GetVpcId()
	row := &model.Network{
		Resource: model.Resource{Base: model.Base{ID: id}, DisplayName: req.GetDisplayName(),
			State: StateImported, Spec: rawSpec, Status: status},
		TenantID: req.GetTenantId(), Name: req.GetName(), Input: input, CloudAccountID: a.ID,
		Provider: model.ProviderAWS, Region: req.GetRegion(), ExternalID: &vpc,
	}
	if rid := req.GetRequestId(); rid != "" {
		row.CreateRequestID = &rid
	}
	return row, nil
}

// discoveringMessage is the status message of an import whose VPC was not
// looked up yet.
const discoveringMessage = "looking the VPC up; the import plan (status.import) follows"

// Discovering reports whether row is an import whose VPC was not looked
// up yet.
func Discovering(row *model.Network) bool {
	return row.State == StateImported && StoredImport(row).GetPhase() == cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING
}

// discoverImport looks the VPC of a DISCOVERING import up, read-only,
// plans the import and stores the result with the row: the spec derived
// from the VPC, the plan (status.import, PLANNED or BLOCKED) and what was
// discovered, from which the Observe-only objects are rendered. It runs
// once: a row that has its plan is never looked up again. It reports
// whether the row was stored; a row a call changed meanwhile is left for
// the next pass.
//
// A VPC that does not exist, cannot be read with the account's
// credentials or cannot be imported is a fault (the network is FAILED,
// with the reason); a lookup that failed for another reason is tried
// again.
func (s *Reconciler) discoverImport(ctx context.Context, row *model.Network) (bool, error) {
	req := &cloudv1.ImportNetworkRequest{}
	if err := Lenient.Unmarshal(row.Input, req); err != nil {
		return false, fmt.Errorf("network %s: stored import request: %w", row.ID, err)
	}
	account, err := s.Account(ctx, row.TenantID, row.CloudAccountID)
	if err != nil {
		return false, err
	}
	found, err := s.discover(ctx, account, req)
	if err != nil {
		return false, err
	}
	plan := corenetwork.PlanImport(found, crossplaneaws.ImportedNetwork{
		Namespace: account.Namespace, ProviderConfigName: account.ProviderConfigName, TenantID: row.TenantID,
		NetworkID: row.ID, NetworkName: row.Name, Region: row.Region,
		Prefix: crossplaneaws.NetworkPrefix(row.TenantID, row.ID, row.Name),
	})
	phase, state := cloudv1.ImportPhase_IMPORT_PHASE_PLANNED, StateImported
	if plan.Blocked() {
		// Nothing is imported. The network keeps the VPC as its external
		// ID: delete it to import the VPC again.
		phase, state = cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED, StateBlocked
	}
	spec, err1 := Encode(&cloudv1.NetworkSpec{Region: row.Region, BootstrapId: account.ID, Layout: plan.Layout})
	input, err2 := row.Input, error(nil)
	if !plan.Blocked() {
		input, err2 = WithImportedResources(row.Input, plan.Resources)
	}
	status, err3 := Encode(&cloudv1.NetworkStatus{Import: &cloudv1.ImportStatus{Phase: phase, Resources: plan.Lines}})
	if err := errors.Join(err1, err2, err3); err != nil {
		return false, fault.New(fault.Internal, err.Error())
	}
	before := *row
	row.Spec, row.Input, row.Status, row.State = spec, input, status, state
	if err := s.deps.Store.Networks().Record(ctx, row); err != nil {
		*row = before
		if errors.Is(err, repository.ErrConflict) || errors.Is(err, repository.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	s.logger().InfoContext(ctx, "network import planned", "tenant_id", row.TenantID, "network_id", row.ID,
		"vpc_id", req.GetAws().GetVpcId(), "blocked", plan.Blocked(), "plan_lines", len(plan.Lines))
	return true, nil
}

// discover makes the read-only AWS lookup of an import's VPC.
func (s *Reconciler) discover(ctx context.Context, a *model.CloudAccount, req *cloudv1.ImportNetworkRequest) (*awscloud.Network, error) {
	if s.deps.AWSDiscovery == nil {
		return nil, fault.New(fault.Unavailable, "AWS lookups are not configured on this controller")
	}
	access, err := s.deps.AWSAccess(ctx, a)
	if err != nil {
		return nil, err
	}
	found, err := s.deps.AWSDiscovery.DiscoverNetwork(ctx, access, req.GetRegion(),
		req.GetAws().GetVpcId(), req.GetAws().GetSubnetIds())
	switch {
	case err == nil:
		return found, nil
	case errors.Is(err, awscloud.ErrNotFound):
		return nil, fault.New(fault.NotFound, err.Error())
	case errors.Is(err, awscloud.ErrInvalid):
		return nil, fault.New(fault.Invalid, err.Error())
	case errors.Is(err, awscloud.ErrAccessDenied):
		return nil, fault.New(fault.FailedPrecondition, "cannot read the VPC with the account's credentials: "+err.Error())
	default:
		s.logger().ErrorContext(ctx, "discover network", "vpc_id", req.GetAws().GetVpcId(), "error", err)
		return nil, fault.New(fault.Unavailable, "the AWS lookup failed: "+err.Error())
	}
}
